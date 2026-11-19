package main

import (
	"context"
	"errors"
	"math/rand/v2"
	"regexp"
	"sort"
	"strings"
)

const surpriseLimit = 15

type chartList struct {
	title, id string
}

// surprise adds a random sample of songs that are popular right now.
// The short trending chart comes first. Remixes, covers and very long mixes
// stay out, and the queue that is already playing is not replaced.
func (c *Catalog) surprise(ctx context.Context, queue []Track) ([]Track, error) {
	page, err := c.request(ctx, "browse", map[string]any{
		"browseId": "FEmusic_charts",
		"formData": map[string]any{"selectedValues": []string{"RU"}},
	})
	if err != nil {
		page, err = c.request(ctx, "browse", map[string]any{"browseId": "FEmusic_charts"})
		if err != nil {
			return nil, err
		}
	}
	lists := orderedChartPlaylists(chartPlaylists(page))
	var pool []Track
	for _, list := range lists {
		if ctx.Err() != nil {
			break
		}
		playlist, err := c.request(ctx, "browse", map[string]any{"browseId": list.id})
		if err != nil {
			continue
		}
		pool = append(pool, parseTracks(playlist)...)
		if len(surpriseTracks(queue, pool, surpriseLimit)) >= surpriseLimit {
			break
		}
	}
	if len(pool) == 0 {
		if tracks := parseTracks(page); len(tracks) > 0 {
			pool = tracks
		} else {
			return nil, errors.New("YouTube Music не вернул популярные треки")
		}
	}
	added := surpriseTracks(queue, pool, 0)
	if len(added) == 0 {
		return nil, errors.New("все эти треки уже есть в очереди")
	}
	rand.Shuffle(len(added), func(i, j int) { added[i], added[j] = added[j], added[i] })
	if len(added) > surpriseLimit {
		added = added[:surpriseLimit]
	}
	return added, nil
}

func chartPlaylists(root any) []chartList {
	var out []chartList
	var walk func(any)
	walk = func(x any) {
		switch v := x.(type) {
		case map[string]any:
			if item, ok := v["musicTwoRowItemRenderer"].(map[string]any); ok {
				id := str(get(item, "navigationEndpoint", "browseEndpoint", "browseId"))
				title := text(get(item, "title"))
				if strings.HasPrefix(id, "VL") && title != "" {
					out = append(out, chartList{title: title, id: id})
				}
				return
			}
			keys := make([]string, 0, len(v))
			for key := range v {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				walk(v[key])
			}
		case []any:
			for _, item := range v {
				walk(item)
			}
		}
	}
	walk(root)
	return out
}

// A short trending list is fresher and smaller than the top-100 chart.
func chartPlaylistRank(title string) int {
	t := strings.ToLower(title)
	switch {
	case strings.Contains(t, "20") || strings.Contains(t, "тренд") || strings.Contains(t, "trend"):
		return 0
	case strings.Contains(t, "день") || strings.Contains(t, "daily") || strings.Contains(t, "today"):
		return 1
	default:
		return 2
	}
}

func orderedChartPlaylists(lists []chartList) []chartList {
	sort.SliceStable(lists, func(i, j int) bool {
		return chartPlaylistRank(lists[i].title) < chartPlaylistRank(lists[j].title)
	})
	return lists
}

func preferChartPlaylist(lists []chartList) string {
	lists = orderedChartPlaylists(lists)
	if len(lists) == 0 {
		return ""
	}
	return lists[0].id
}

// Remix and cover uploads crowd out the song that is actually in the chart.
var surpriseJunk = regexp.MustCompile(`(?i)(remix|cover|karaoke|кавер|ремикс|караоке|bass\s*boost|slowed|nightcore|tiktok|sped\s*up|8d audio|reverb)`)

func surpriseCandidate(t Track) bool {
	if t.DurationMS > 0 && (t.DurationMS < 75_000 || t.DurationMS > 7*60*1000) {
		return false
	}
	return !surpriseJunk.MatchString(t.Title + " " + t.Artists)
}

// surpriseTracks drops songs already queued, then remixes and clips.
// Junk is used only when the chart has nothing else left.
func surpriseTracks(queue, chart []Track, limit int) []Track {
	var clean, rest []Track
	for _, track := range chart {
		if surpriseCandidate(track) {
			clean = append(clean, track)
		} else {
			rest = append(rest, track)
		}
	}
	added := newRadioTracks(queue, clean)
	if len(added) == 0 {
		added = newRadioTracks(queue, rest)
	}
	if limit > 0 && len(added) > limit {
		added = added[:limit]
	}
	return added
}

// appendSurpriseLocked adds tracks to the end. The caller holds s.mu.
// Playback starts only when the queue was empty.
func (s *Server) appendSurpriseLocked(tracks []Track) (added int, start bool) {
	tracks = newRadioTracks(s.state.Queue, tracks)
	if len(tracks) == 0 {
		return 0, false
	}
	start = len(s.state.Queue) == 0
	s.queueOrder = append(s.queueOrder, tracks...)
	s.state.Queue = append(s.state.Queue, tracks...)
	if start {
		s.state.Index = 0
		s.state.PositionMS = 0
	}
	s.saveLocked()
	return len(tracks), start
}
