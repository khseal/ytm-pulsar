package main

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
)

func TestSurpriseQueriesMixGenres(t *testing.T) {
	got := surpriseQueries(4, nil, rand.New(rand.NewPCG(1, 2)))
	if len(got) != 4 {
		t.Fatalf("%v", got)
	}
	seen := map[string]bool{}
	for _, query := range got {
		matched := false
		for _, genre := range surpriseGenres {
			if query == genre || strings.HasPrefix(query, genre+" ") {
				matched = true
				break
			}
		}
		if !matched || seen[query] {
			t.Fatalf("query %q", query)
		}
		seen[query] = true
	}
	again := surpriseQueries(4, nil, rand.New(rand.NewPCG(1, 2)))
	if strings.Join(got, "|") != strings.Join(again, "|") {
		t.Fatalf("seed changed the queries: %v vs %v", got, again)
	}
}

func TestSurpriseQueriesStayInOneGenre(t *testing.T) {
	got := surpriseQueries(4, []string{"джаз"}, rand.New(rand.NewPCG(3, 4)))
	if len(got) != 4 {
		t.Fatalf("%v", got)
	}
	seen := map[string]bool{}
	for _, query := range got {
		if query != "джаз" && !strings.HasPrefix(query, "джаз ") || seen[query] {
			t.Fatalf("query %q", query)
		}
		seen[query] = true
	}
}

func TestSurpriseQueriesCoverEveryChosenGenre(t *testing.T) {
	got := surpriseQueries(4, []string{"джаз", "рок"}, rand.New(rand.NewPCG(5, 6)))
	found := map[string]bool{}
	for _, query := range got {
		switch {
		case query == "джаз" || strings.HasPrefix(query, "джаз "):
			found["джаз"] = true
		case query == "рок" || strings.HasPrefix(query, "рок "):
			found["рок"] = true
		default:
			t.Fatalf("query %q", query)
		}
	}
	if !found["джаз"] || !found["рок"] {
		t.Fatalf("%v", got)
	}
	if _, err := knownSurpriseGenres("джаз, рок"); err != nil {
		t.Fatal(err)
	}
	if _, err := knownSurpriseGenres("французские песни"); err == nil {
		t.Fatal("language list still accepted")
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
	s.state.Exact = true
	added, start := s.appendSurpriseLocked([]Track{{ID: "bbbbbbbbbbb", Title: "Новая", Artists: "Б"}})
	if start || added != 1 || len(s.state.Queue) != 2 || s.state.Queue[0].ID != "aaaaaaaaaaa" || s.state.Index != 0 || s.state.Exact || !s.state.Random {
		t.Fatalf("added=%d start=%v index=%d exact=%v random=%v %+v", added, start, s.state.Index, s.state.Exact, s.state.Random, s.state.Queue)
	}
}

func TestAppendSurpriseStartsAnEmptyQueue(t *testing.T) {
	t.Setenv("YTM_ALICE_DATA", t.TempDir())
	s := &Server{state: State{Status: "stopped", Queue: []Track{}}}
	added, start := s.appendSurpriseLocked([]Track{{ID: "bbbbbbbbbbb", Title: "Новая", Artists: "Б"}})
	if !start || added != 1 || s.state.Index != 0 || s.state.Queue[0].ID != "bbbbbbbbbbb" || !s.state.Random {
		t.Fatalf("added=%d start=%v random=%v %+v", added, start, s.state.Random, s.state)
	}
}

func TestRandomKeepsGrowingAndAPlaylistDoesNot(t *testing.T) {
	if queueContinuation(false, true, true, true) != "surprise" {
		t.Fatal("random queue should add another batch")
	}
	if queueContinuation(true, false, true, true) != "" {
		t.Fatal("playlist should stay unchanged")
	}
	if queueContinuation(false, false, true, true) != "radio" {
		t.Fatal("a single track should still get similar songs")
	}
	if queueContinuation(false, true, false, true) != "" || queueContinuation(false, true, true, false) != "" {
		t.Fatal("continuation must wait for the last track and the radio switch")
	}
}

func TestSavedGenresFeedTheNextSurprise(t *testing.T) {
	t.Setenv("YTM_ALICE_DATA", t.TempDir())
	s := &Server{settings: &Settings{WebAddr: ":24721", Radio: true}}
	genres, remember, err := surpriseGenreChoice("джаз, рок")
	if err != nil || !remember || strings.Join(genres, ",") != "джаз,рок" {
		t.Fatalf("choice: %v %v %v", err, remember, genres)
	}
	s.mu.Lock()
	if err := s.storeGenresLocked(genres); err != nil {
		s.mu.Unlock()
		t.Fatal(err)
	}
	s.mu.Unlock()
	if _, remember, err := surpriseGenreChoice(""); err != nil || remember {
		t.Fatalf("empty query should reuse the saved genres: %v %v", err, remember)
	}
	saved := s.getSettings().Genres
	got := surpriseQueries(4, saved, rand.New(rand.NewPCG(5, 6)))
	for _, query := range got {
		if query != "джаз" && !strings.HasPrefix(query, "джаз ") && query != "рок" && !strings.HasPrefix(query, "рок ") {
			t.Fatalf("left the saved genres: %q", query)
		}
	}
	out := s.settingsCommand(request{Action: "config-set", Settings: &SettingsPatch{Genres: []string{}}})
	if out.Error != "" || len(out.Settings.Genres) != 0 {
		t.Fatalf("clear: %s %+v", out.Error, out.Settings)
	}
	if _, _, err := surpriseGenreChoice("не жанр"); err == nil {
		t.Fatal("unknown genre accepted")
	}
}
