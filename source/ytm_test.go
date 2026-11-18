package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

func TestRelayBoundsChunksAndPreservesRanges(t *testing.T) {
	data := bytes.Repeat([]byte("abcdefg"), 400000)
	c := newCatalog()
	calls := 0
	c.http = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		span := r.Header.Get("Range")
		if strings.HasSuffix(span, "-") {
			t.Fatal("open-ended CDN range")
		}
		var start, end int64
		if _, e := fmt.Sscanf(span, "bytes=%d-%d", &start, &end); e != nil {
			t.Fatal(e)
		}
		if end-start+1 > 1<<20 {
			t.Fatal("unbounded CDN request")
		}
		calls++
		return &http.Response{StatusCode: 206, Body: io.NopCloser(bytes.NewReader(data[start : end+1])), Header: make(http.Header)}, nil
	})}
	s := &Server{catalog: c, player: &nativePlayer{token: "test-token", source: streamInfo{url: "https://example.googlevideo.com/a", mime: "audio/mp4", length: int64(len(data))}}}
	for _, span := range []string{"", "bytes=100-", "bytes=200-399", "bytes=-100"} {
		req := httptest.NewRequest("GET", "http://127.0.0.1/audio/test-token", nil)
		if span != "" {
			req.Header.Set("Range", span)
		}
		w := httptest.NewRecorder()
		s.relay(w, req)
		start, end, e := byteRange(span, int64(len(data)))
		if e != nil {
			t.Fatal(e)
		}
		if !bytes.Equal(w.Body.Bytes(), data[start:end+1]) {
			t.Fatalf("wrong audio range %q", span)
		}
		if span != "" && w.Code != 206 {
			t.Fatal("range status lost")
		}
	}
	if calls < 6 {
		t.Fatal("relay did not split the whole stream")
	}
	for _, span := range []string{"bytes=99999999-", "bytes=20-10", "bytes=1-3,4-5", "bytes=-0"} {
		if _, _, e := byteRange(span, 100); e == nil {
			t.Fatalf("invalid range accepted: %s", span)
		}
	}
	req := httptest.NewRequest("HEAD", "http://127.0.0.1/audio/test-token", nil)
	w := httptest.NewRecorder()
	before := calls
	s.relay(w, req)
	if w.Code != 200 || w.Body.Len() != 0 || calls != before {
		t.Fatal("HEAD must use metadata without a CDN HEAD request")
	}
}
func TestCatalogActualSearch(t *testing.T) {
	raw, e := os.ReadFile("testdata/search.json")
	if e != nil {
		t.Fatal(e)
	}
	var data any
	if json.Unmarshal(raw, &data) != nil {
		t.Fatal("fixture invalid")
	}
	tracks := parseTracks(data)
	if len(tracks) < 3 || tracks[0].ID != "4D7u5KF7SP8" || !strings.Contains(tracks[0].Artists, "Daft Punk") {
		t.Fatalf("catalogue lost ordered tracks: %#v", tracks)
	}
}
func TestPlaylistParsingDeduplicates(t *testing.T) {
	raw := `{"contents":{"unknownNewRenderer":{"contents":[{"playlistPanelVideoRenderer":{"videoId":"abcdefghijk","title":{"simpleText":"First"},"lengthText":{"simpleText":"1:02"},"longBylineText":{"runs":[{"text":"Artist"}]}}},{"playlistPanelVideoRenderer":{"videoId":"abcdefghijk","title":{"simpleText":"Duplicate"}}},{"playlistPanelVideoRenderer":{"videoId":"short","title":{"simpleText":"Broken"}}}]}}}`
	var data any
	json.Unmarshal([]byte(raw), &data)
	tracks := parseTracks(data)
	if len(tracks) != 1 || tracks[0].DurationMS != 62000 || tracks[0].Artists != "Artist" {
		t.Fatalf("playlist parse: %#v", tracks)
	}
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestStopCancelsPendingSearch(t *testing.T) {
	t.Setenv("YTM_ALICE_DATA", t.TempDir())
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	c := newCatalog()
	c.http = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		once.Do(func() { close(entered) })
		<-release
		body := `{"contents":[{"playlistPanelVideoRenderer":{"videoId":"abcdefghijk","title":{"simpleText":"Track"}}}]}`
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	s := &Server{catalog: c, state: State{Status: "stopped"}}
	done := make(chan response, 1)
	go func() { done <- s.command(context.Background(), request{Action: "play", Query: "jazz"}) }()
	<-entered
	s.command(context.Background(), request{Action: "stop"})
	close(release)
	result := <-done
	if result.Error == "" || s.snapshot().Status != "stopped" || s.player != nil {
		t.Fatal("old search restarted stopped playback")
	}
}
func TestExternalLinksRejected(t *testing.T) {
	c := newCatalog()
	for _, q := range []string{"file:///etc/passwd", "http://127.0.0.1/secrets", "https://youtube.com.attacker.invalid/watch?v=abcdefghijk"} {
		if _, err := c.lookup(context.Background(), q); err == nil {
			t.Fatalf("accepted %q", q)
		}
	}
}

func TestStaleRetryDoesNotRestartStoppedPlayer(t *testing.T) {
	s := &Server{generation: 3, state: State{Status: "stopped", Queue: []Track{{ID: "abcdefghijk"}}}}
	old := uint64(2)
	s.startGuarded(0, 1, &old)
	if s.snapshot().Status != "stopped" || s.generation != 3 || s.player != nil {
		t.Fatal("stale retry restarted playback")
	}
}
