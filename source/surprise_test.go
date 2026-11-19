package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestPreferChartPlaylistUsesTheShortTrendingList(t *testing.T) {
	page := map[string]any{
		"contents": []any{
			map[string]any{"musicTwoRowItemRenderer": map[string]any{
				"title":              map[string]any{"runs": []any{map[string]any{"text": "Россия – 100 лучших клипов"}}},
				"navigationEndpoint": map[string]any{"browseEndpoint": map[string]any{"browseId": "VLPL100"}},
			}},
			map[string]any{"musicTwoRowItemRenderer": map[string]any{
				"title":              map[string]any{"runs": []any{map[string]any{"text": "Лучшие клипы за день – Россия"}}},
				"navigationEndpoint": map[string]any{"browseEndpoint": map[string]any{"browseId": "VLPLDAY"}},
			}},
			map[string]any{"musicTwoRowItemRenderer": map[string]any{
				"title":              map[string]any{"simpleText": "Россия – 20 трендовых клипов"},
				"navigationEndpoint": map[string]any{"browseEndpoint": map[string]any{"browseId": "VLPL20"}},
			}},
		},
	}
	if got := preferChartPlaylist(chartPlaylists(page)); got != "VLPL20" {
		t.Fatalf("playlist %s", got)
	}
}

func TestSurpriseSkipsRemixesAndSongsAlreadyQueued(t *testing.T) {
	queue := []Track{{ID: "aaaaaaaaaaa", Title: "Уже есть", Artists: "А", DurationMS: 180000}}
	chart := []Track{
		{ID: "aaaaaaaaaaa", Title: "Уже есть", Artists: "А", DurationMS: 180000},
		{ID: "bbbbbbbbbbb", Title: "Хит", Artists: "Б", DurationMS: 180000},
		{ID: "ccccccccccc", Title: "Хит (Remix 2026)", Artists: "Б", DurationMS: 200000},
		{ID: "ddddddddddd", Title: "Кавер на хит", Artists: "В", DurationMS: 190000},
		{ID: "eeeeeeeeeee", Title: "Короткий", Artists: "Г", DurationMS: 20000},
		{ID: "fffffffffff", Title: "Нормальная", Artists: "Д", DurationMS: 210000},
	}
	got := surpriseTracks(queue, chart, surpriseLimit)
	var ids []string
	for _, track := range got {
		ids = append(ids, track.ID)
	}
	if strings.Join(ids, ",") != "bbbbbbbbbbb,fffffffffff" {
		t.Fatalf("%v", ids)
	}
}

func TestSurpriseStopsAtTheLimit(t *testing.T) {
	chart := make([]Track, 20)
	for i := range chart {
		chart[i] = Track{ID: fmt.Sprintf("s%010d", i), Title: fmt.Sprintf("Песня %d", i), Artists: "И", DurationMS: 180000}
	}
	if got := surpriseTracks(nil, chart, surpriseLimit); len(got) != surpriseLimit {
		t.Fatalf("%d", len(got))
	}
}

func TestAppendSurpriseKeepsTheCurrentTrack(t *testing.T) {
	t.Setenv("YTM_ALICE_DATA", t.TempDir())
	s := &Server{state: State{Status: "playing", Index: 0, Queue: []Track{{ID: "aaaaaaaaaaa", Title: "Сейчас", Artists: "А"}}}}
	s.queueOrder = append([]Track(nil), s.state.Queue...)
	added, start := s.appendSurpriseLocked([]Track{{ID: "bbbbbbbbbbb", Title: "Новая", Artists: "Б"}})
	if start || added != 1 || len(s.state.Queue) != 2 || s.state.Queue[0].ID != "aaaaaaaaaaa" || s.state.Index != 0 {
		t.Fatalf("added=%d start=%v index=%d %+v", added, start, s.state.Index, s.state.Queue)
	}
}

func TestAppendSurpriseStartsAnEmptyQueue(t *testing.T) {
	t.Setenv("YTM_ALICE_DATA", t.TempDir())
	s := &Server{state: State{Status: "stopped", Queue: []Track{}}}
	added, start := s.appendSurpriseLocked([]Track{{ID: "bbbbbbbbbbb", Title: "Новая", Artists: "Б"}})
	if !start || added != 1 || s.state.Index != 0 || s.state.Queue[0].ID != "bbbbbbbbbbb" {
		t.Fatalf("added=%d start=%v %+v", added, start, s.state)
	}
}
