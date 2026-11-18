package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const streamAttempts = 3

func (c *Catalog) discardStream(src streamInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cached, ok := c.streams[src.id]; ok && cached.url == src.url {
		delete(c.streams, src.id)
	}
}

// forgetPlayer drops the cached YouTube player program. A player that decodes
// without error but wrongly (e.g. after YouTube rotated it) yields URLs the
// CDN rejects with 403; until the next restart every refresh reused it.
func (c *Catalog) forgetPlayer() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.playerSource = ""
	// The next program may be another version; its timestamp is read anew.
	c.setSignatureTimestamp(0)
	// withYT also caches the embed page that selects base.js. Drop that
	// client as well, otherwise a 403 refresh downloads the old program again.
	c.ytMu.Lock()
	c.ytClient = nil
	c.ytMu.Unlock()
}

// Signed URLs and underlying error strings must never reach the log.
func streamErrorClass(err error) string {
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	if strings.Contains(err.Error(), "timeout awaiting response headers") {
		return "response-header-timeout"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline"
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "dns"
	}
	var certificate *tls.CertificateVerificationError
	if errors.As(err, &certificate) {
		return "tls-certificate"
	}
	var network net.Error
	if errors.As(err, &network) && network.Timeout() {
		return "network-timeout"
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return "short-read"
	}
	return "network-io"
}

func streamLog(src streamInfo, offset, last int64, attempt int, reason string) {
	host := "unknown"
	if u, err := url.Parse(src.url); err == nil {
		host = u.Hostname()
	}
	log.Printf("CDN track=%s host=%s range=%d-%d attempt=%d/%d reason=%s", src.id, host, offset, last, attempt, streamAttempts, reason)
}

// Pulsar's playbin holds only about two seconds of a network stream, so a CDN
// request that stalls longer is heard as a dropout. The relay downloads ahead
// of the player instead: up to relayAhead chunks wait in memory (~4 MB, a few
// minutes of audio). The first chunk is small so playback starts quickly.
const (
	relayFirstChunk = 256 << 10
	relayChunkSize  = 512 << 10
	relayAhead      = 8

)

// Sent at once (~15 s of 128 kbit/s audio), then relayPace bytes/s, four times
// the playback rate, in small writes. Pulsar pauses on any buffering message,
// and its queue2 estimates the input rate: large bursts of input make it
// report 0 % for a moment, an audible hiccup.
const (
	relayBurst    = 256 << 10
	relayPaceStep = 4 << 10
)

var relayPace int64 = 64 << 10

func waitStreamRetry(ctx context.Context, attempt int) bool {
	timer := time.NewTimer(time.Duration(attempt) * 200 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func (s *Server) relayAudio(w http.ResponseWriter, r *http.Request, p *nativePlayer, src streamInfo, start, end int64) {
	s.relayAudioRange(w, r, p, src, start, end, false)
}

// relayAudioRange copies [start, end] of the CDN stream; headersSent means the
// caller has already written the response status and headers.
func (s *Server) relayAudioRange(w http.ResponseWriter, r *http.Request, p *nativePlayer, src streamInfo, start, end int64, headersSent bool) {
	client := *s.catalog.http
	client.Timeout = 0 // A paused reader must not expire the upstream body.
	if transport, ok := client.Transport.(*http.Transport); ok {
		copy := transport.Clone()
		copy.ResponseHeaderTimeout = 5 * time.Second
		copy.TLSHandshakeTimeout = 5 * time.Second
		copy.DialContext = (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext
		client.Transport = copy
		defer copy.CloseIdleConnections()
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	mime, length := src.mime, src.length
	chunks := make(chan []byte, relayAhead)
	go func() {
		defer close(chunks)
		size := int64(relayFirstChunk)
		for offset := start; offset <= end; {
			last := min(offset+size-1, end)
			fetchStart := time.Now()
			data, ok := s.fetchRange(ctx, &client, p, &src, offset, last)
			if took := time.Since(fetchStart); took > 2*time.Second {
				log.Printf("relay track=%s chunk %d-%d took=%s ahead=%d", src.id, offset, last, took.Round(10*time.Millisecond), len(chunks))
			}
			if !ok {
				return
			}
			select {
			case chunks <- data:
			case <-ctx.Done():
				return
			}
			offset = last + 1
			size = relayChunkSize
		}
	}()
	first := !headersSent
	sent := start
	pacingStart := time.Now()
	for data := range chunks {
		if first {
			w.Header().Set("Content-Type", mime)
			w.Header().Set("Accept-Ranges", "bytes")
			w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
			w.Header().Set("Cache-Control", "no-store")
			if r.Header.Get("Range") != "" {
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, length))
				w.WriteHeader(206)
			} else {
				w.WriteHeader(200)
			}
			first = false
		}
		for len(data) > 0 {
			n := min(len(data), relayPaceStep)
			if _, err := w.Write(data[:n]); err != nil {
				return
			}
			data = data[n:]
			sent += int64(n)
			// Past the burst, hand data over at relayPace: the whole track at
			// once lands while Pulsar is starting its pipeline and decoder.
			if over := sent - start - relayBurst; over > 0 {
				due := pacingStart.Add(time.Duration(over*int64(time.Second)/relayPace))
				if wait := time.Until(due); wait > 0 {
					select {
					case <-time.After(wait):
					case <-ctx.Done():
						return
					}
				}
			}
		}
	}
	if sent <= end {
		if first && r.Context().Err() == nil {
			w.WriteHeader(http.StatusBadGateway)
		}
		return
	}
	if p != nil && end == length-1 {
		p.reachedEnd.Store(true)
	}
}

// fetchRange downloads [offset, last] of the CDN stream. Retries request only
// the missing bytes; the first retry refreshes an expired or rejected URL.
func (s *Server) fetchRange(ctx context.Context, client *http.Client, p *nativePlayer, src *streamInfo, offset, last int64) ([]byte, bool) {
	buf := make([]byte, last-offset+1)
	failures := 0
	forbidden := false
	for pos := offset; pos <= last; {
		if ctx.Err() != nil {
			return nil, false
		}
		if failures > 0 {
			if !waitStreamRetry(ctx, failures) {
				return nil, false
			}
			if failures == 1 && src.id != "" {
				s.catalog.discardStream(*src)
				if forbidden {
					s.catalog.forgetPlayer()
				}
				fresh, err := s.catalog.resolve(ctx, src.id)
				if ctx.Err() != nil {
					return nil, false
				}
				if err == nil && fresh.length == src.length && fresh.mime == src.mime {
					s.mu.Lock()
					if s.player == p && p.source.url == src.url {
						p.source = fresh
					}
					s.mu.Unlock()
					*src = fresh
				} else {
					streamLog(*src, pos, last, failures+1, "refresh-unavailable")
				}
			}
		}
		req, err := http.NewRequestWithContext(ctx, "GET", src.url, nil)
		if err != nil {
			return nil, false
		}
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", pos, last))
		req.Header.Set("Accept-Encoding", "identity")
		resp, err := client.Do(req)
		if ctx.Err() != nil {
			if resp != nil {
				resp.Body.Close()
			}
			return nil, false
		}
		if err != nil {
			streamLog(*src, pos, last, failures+1, streamErrorClass(err))
		} else if resp.StatusCode != http.StatusPartialContent {
			status := resp.StatusCode
			resp.Body.Close()
			streamLog(*src, pos, last, failures+1, "http-"+strconv.Itoa(status))
			forbidden = forbidden || status == 403
			if status != 403 && status != 408 && status != 410 && status != 429 && status < 500 {
				return nil, false
			}
		} else {
			// Reject a server that returns a different position/file on retry.
			if span := resp.Header.Get("Content-Range"); span != "" && span != fmt.Sprintf("bytes %d-%d/%d", pos, last, src.length) {
				resp.Body.Close()
				streamLog(*src, pos, last, failures+1, "wrong-content-range")
				return nil, false
			}
			n, readErr := io.ReadFull(resp.Body, buf[pos-offset:])
			resp.Body.Close()
			pos += int64(n)
			if readErr == nil || ctx.Err() != nil {
				continue
			}
			streamLog(*src, pos, last, failures+1, streamErrorClass(readErr))
		}
		failures++
		if failures >= streamAttempts {
			s.catalog.discardStream(*src)
			if forbidden {
				s.catalog.forgetPlayer()
			}
			return nil, false
		}
	}
	return buf, true
}

