package main

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestUnavailableQueueTerminatesWithoutRadio(t *testing.T) {
	t.Setenv("YTM_ALICE_DATA", t.TempDir())
	c := newCatalog()
	c.playerSTS = 20761
	c.auth = &Credentials{Cookie: "SAPISID=fixture-only", AuthUser: "0"}
	c.http = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		return jsonReply(map[string]any{"playabilityStatus": map[string]any{"status": "UNPLAYABLE"}}), nil
	})}
	options := defaultSettings()
	s := &Server{catalog: c, settings: &options, state: State{Queue: []Track{{ID: "aaaaaaaaaaa"}, {ID: "bbbbbbbbbbb"}, {ID: "ccccccccccc"}}}}
	s.start(0)
	deadline := time.After(5 * time.Second)
	for {
		state := s.snapshot()
		if state.Status == "error" {
			if state.Index != 2 || len(state.Queue) != 3 {
				t.Fatalf("queue stopped before exhausting unavailable tracks: %+v", state)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatal("unavailable queue did not terminate")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestUnavailableClassification(t *testing.T) {
	c := newCatalog()
	c.playerSTS = 20761
	for _, status := range []string{"LOGIN_REQUIRED", "CONTENT_CHECK_REQUIRED", "AGE_CHECK_REQUIRED", "UNPLAYABLE", "ERROR"} {
		c.http = &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
			return jsonReply(map[string]any{"playabilityStatus": map[string]any{"status": status}}), nil
		})}
		_, err := c.musicVideo(context.Background(), "aaaaaaaaaaa")
		var unavailable unavailableTrack
		if !errors.As(err, &unavailable) {
			t.Fatalf("%s must mark track unavailable: %v", status, err)
		}
	}
}

