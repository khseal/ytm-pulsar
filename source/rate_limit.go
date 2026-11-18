package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	yt "github.com/kkdai/youtube/v2"
)

// The limit belongs to the IP, across public and authenticated player clients.
type rateLimited struct{ until time.Time }

func (e rateLimited) Error() string {
	minutes := max(1, int(time.Until(e.until).Minutes())+1)
	return fmt.Sprintf("YouTube временно ограничил запросы с этого IP (429); повторите через %d мин", minutes)
}
func (c *Catalog) playbackLimit() error {
	c.limitMu.Lock()
	defer c.limitMu.Unlock()
	if time.Now().Before(c.limitedUntil) {
		return rateLimited{c.limitedUntil}
	}
	return nil
}
func (c *Catalog) noteRateLimit() error {
	c.limitMu.Lock()
	defer c.limitMu.Unlock()
	c.limitBackoff = min(max(2*c.limitBackoff, 2*time.Minute), 30*time.Minute)
	c.limitedUntil = time.Now().Add(c.limitBackoff)
	return rateLimited{c.limitedUntil}
}
func (c *Catalog) clearRateLimit() {
	c.limitMu.Lock()
	defer c.limitMu.Unlock()
	// Another in-flight metadata request may just have observed a 429.
	if time.Now().Before(c.limitedUntil) {
		return
	}
	c.limitBackoff = 0
	c.limitedUntil = time.Time{}
}
func (c *Catalog) videoInfo(ctx context.Context, client *yt.Client, id string) (*yt.Video, error) {
	if err := c.playbackLimit(); err != nil {
		return nil, err
	}
	return client.GetVideoContext(ctx, id)
}

func (c *Catalog) trackVideo(ctx context.Context, id string) (*yt.Video, error) {
	if err := c.playbackLimit(); err != nil {
		return nil, err
	}
	var v *yt.Video
	// Public player first: the cookie-backed WEB_REMIX streams carry no PO token,
	// and the CDN answers 403 after their first megabyte. The Music endpoint
	// (browser cookies; a TV token is not accepted there) stays the fallback
	// for tracks the public player refuses.
	err := c.withYT(func(client *yt.Client) (err error) { v, err = c.videoInfo(ctx, client, id); return err })
	if err == nil {
		return v, nil
	}
	var limited rateLimited
	if errors.As(err, &limited) {
		return nil, err
	}
	if ctx.Err() != nil {
		return nil, resolverError(ctx.Err())
	}
	// The raw error may carry a signed URL; log only the playability verdict.
	detail := logSafe(err.Error())
	var status *yt.ErrPlayabiltyStatus
	if errors.As(err, &status) {
		detail = status.Status + " " + strconv.Quote(status.Reason)
	} else if errors.Is(err, yt.ErrLoginRequired) {
		detail = "login required"
	}
	log.Printf("YouTube public player track=%s: %s", id, detail)
	if strings.Contains(strings.ToLower(detail), "live stream") {
		live, lerr := c.liveVideo(ctx, id)
		if lerr == nil {
			return live, nil
		}
		log.Printf("YouTube live track=%s: %s", id, logSafe(lerr.Error()))
	}
	return c.musicVideo(ctx, id)
}

// liveVideo asks the iOS player for the HLS manifest of a live stream: the
// public client does not serve live streams, and iOS needs no PO token for HLS.
func (c *Catalog) liveVideo(ctx context.Context, id string) (*yt.Video, error) {
	const version = "20.10.4"
	body, _ := json.Marshal(map[string]any{
		"context": map[string]any{"client": map[string]any{
			"clientName": "IOS", "clientVersion": version, "deviceMake": "Apple", "deviceModel": "iPhone16,2",
			"osName": "iPhone", "osVersion": "18.3.2.22D82", "hl": "en", "gl": "US",
		}},
		"videoId": id, "contentCheckOk": true, "racyCheckOk": true,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://www.youtube.com/youtubei/v1/player?prettyPrint=false", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "com.google.ios.youtube/"+version+" (iPhone16,2; U; CPU iOS 18_3_2 like Mac OS X;)")
	req.Header.Set("X-YouTube-Client-Name", "5")
	req.Header.Set("X-YouTube-Client-Version", version)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("player: HTTP %d", resp.StatusCode)
	}
	var data any
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&data); err != nil {
		return nil, err
	}
	if status := str(get(data, "playabilityStatus", "status")); status != "OK" {
		return nil, fmt.Errorf("%s %q", status, str(get(data, "playabilityStatus", "reason")))
	}
	manifest := str(get(data, "streamingData", "hlsManifestUrl"))
	if manifest == "" {
		return nil, errors.New("no HLS manifest")
	}
	return &yt.Video{ID: id, Title: str(get(data, "videoDetails", "title")), Author: str(get(data, "videoDetails", "author")), HLSManifestURL: manifest}, nil
}

var urlInText = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://\S+`)

// logSafe hides URLs, which may be signed, and caps the length of an error
// before it goes to the log.
func logSafe(message string) string {
	message = urlInText.ReplaceAllString(message, "<url>")
	if len([]rune(message)) > 200 {
		message = string([]rune(message)[:200]) + "…"
	}
	return message
}

// RoundTrip cannot log wrapped network errors: they may include signed URLs.
func (p *ytPageCache) network(req *http.Request) (*http.Response, error) {
	if p.catalog != nil {
		if err := p.catalog.playbackLimit(); err != nil {
			return nil, err
		}
	}
	response, err := p.base.RoundTrip(req)
	if err == nil && response.StatusCode == http.StatusTooManyRequests && p.catalog != nil {
		response.Body.Close()
		return nil, p.catalog.noteRateLimit()
	}
	return response, err
}
