package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestParseCastMessages(t *testing.T) {
	in := `[[7,["c","SIDXYZ","",8]],[8,["S","gsess-1"]],[9,["loungeStatus",{"devices":"[{\"type\":\"REMOTE_CONTROL\",\"id\":\"a\"}]"}]],[10,["noop"]],[11,["setPlaylist",{"listId":"RDAMVM1","videoIds":"dQw4w9WgXcQ,kJQP7kiw5Fk","currentIndex":"1","videoId":"kJQP7kiw5Fk","currentTime":"0"}]]]`
	m := parseCastMessages(in)
	if len(m) != 5 {
		t.Fatalf("got %d messages: %+v", len(m), m)
	}
	c := &castReceiver{}
	c.noteMessages(m)
	if c.sid != "SIDXYZ" || c.gsid != "gsess-1" || c.aid != 11 {
		t.Fatalf("sid=%q gsid=%q aid=%d", c.sid, c.gsid, c.aid)
	}
	if pstr(m[4].Payload, "currentIndex") != "1" || pstr(m[4].Payload, "listId") != "RDAMVM1" {
		t.Fatalf("payload %+v", m[4].Payload)
	}
	if m[3].Name != "noop" || m[3].Payload != nil {
		t.Fatalf("noop %+v", m[3])
	}
}

func TestCastPlaylistTargetUsesTheSendersVideo(t *testing.T) {
	ids := []string{"aaaaaaaaaaa", "bbbbbbbbbbb"}
	payload := map[string]any{"videoId": "zedy1eH0Klc", "currentIndex": "0", "videoIds": "aaaaaaaaaaa,bbbbbbbbbbb"}
	if got := castPlaylistTarget("setPlaylist", payload, ids); got != "zedy1eH0Klc" {
		t.Fatalf("setPlaylist played %s", got)
	}
	payload = map[string]any{"videoId": "bbbbbbbbbbb", "currentIndex": "0"}
	if got := castPlaylistTarget("setPlaylist", payload, ids); got != "bbbbbbbbbbb" {
		t.Fatalf("index won over videoId: %s", got)
	}
	payload = map[string]any{"currentIndex": "1"}
	if got := castPlaylistTarget("setPlaylist", payload, ids); got != "bbbbbbbbbbb" {
		t.Fatalf("currentIndex: %s", got)
	}
	payload = map[string]any{"videoIds": "aaaaaaaaaaa,bbbbbbbbbbb"}
	if got := castPlaylistTarget("updatePlaylist", payload, ids); got != "" {
		t.Fatalf("update without a selection changed the video to %s", got)
	}
	payload = map[string]any{"eventDetails": `{"eventType":"VIDEO_SELECTED","videoId":"bbbbbbbbbbb"}`}
	if got := castPlaylistTarget("updatePlaylist", payload, ids); got != "bbbbbbbbbbb" {
		t.Fatalf("selection: %s", got)
	}
}

func TestApplyStreamMetaFillsAPlaceholder(t *testing.T) {
	got := applyStreamMeta(Track{ID: "zedy1eH0Klc", Title: "zedy1eH0Klc", ThumbnailURL: fallbackThumbnail("zedy1eH0Klc")}, streamInfo{
		title: "Название", author: "Автор", thumbnail: "https://i.ytimg.com/vi/zedy1eH0Klc/maxresdefault.jpg", durationMS: 125000,
	})
	if got.Title != "Название" || got.Artists != "Автор" || got.DurationMS != 125000 || !strings.Contains(got.ThumbnailURL, "maxresdefault") {
		t.Fatalf("%+v", got)
	}
	kept := applyStreamMeta(Track{ID: "abcdefghijk", Title: "Уже есть", Artists: "Кто-то", DurationMS: 1000, ThumbnailURL: "https://i.ytimg.com/vi/abcdefghijk/hqdefault.jpg"}, streamInfo{title: "Другое", durationMS: 9})
	if kept.Title != "Уже есть" || kept.DurationMS != 1000 {
		t.Fatalf("overwrote a real track: %+v", kept)
	}
}

func TestCastStatus(t *testing.T) {
	q := []Track{{ID: "dQw4w9WgXcQ"}}
	cases := map[string]int{"playing": 1, "paused": 2, "buffering": 3, "stopped": 4}
	for s, want := range cases {
		if got := castStatus(State{Status: s, Queue: q}); got != want {
			t.Errorf("%s: %d != %d", s, got, want)
		}
	}
	if castStatus(State{Status: "stopped"}) != -1 {
		t.Error("empty queue must be idle")
	}
}

func TestResolveRateLimitGate(t *testing.T) {
	c := newCatalog()
	c.limitedUntil = time.Now().Add(time.Minute)
	_, err := c.resolve(context.Background(), "dQw4w9WgXcQ")
	if err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("want rate-limit error without a request, got %v", err)
	}
}
