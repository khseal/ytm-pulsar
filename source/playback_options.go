package main

import (
	"context"
	"math/rand/v2"
	"regexp"
	"sort"
	"strings"
	"time"
)

func (s *Server) installQueueLocked(tracks []Track, index int, exact bool) {
	s.queueOrder = append([]Track(nil), tracks...)
	s.state.Queue = append([]Track(nil), tracks...)
	s.state.Index = index
	if index < 0 {
		s.state.Index = 0
	}
	s.queueCast = false
	s.state.Exact = exact
	s.state.Random = false
	if s.settingsLocked().Shuffle {
		rand.Shuffle(len(s.state.Queue), func(i, j int) { s.state.Queue[i], s.state.Queue[j] = s.state.Queue[j], s.state.Queue[i] })
		// Selecting a track must still start the selected track.
		if index >= 0 && index < len(tracks) {
			for i, track := range s.state.Queue {
				if track.ID == tracks[index].ID {
					s.state.Queue[0], s.state.Queue[i] = s.state.Queue[i], s.state.Queue[0]
					break
				}
			}
		}
		s.state.Index = 0
	}
}

func (s *Server) shuffleQueue(enabled bool) response {
	patch := SettingsPatch{Shuffle: &enabled}
	result := s.settingsCommand(request{Action: "config-set", Settings: &patch})
	if result.Error != "" {
		return result
	}
	s.mu.Lock()
	start := s.state.Index + 1
	if s.state.Status == "stopped" {
		start = 0
	}
	if start >= 0 && start < len(s.state.Queue) {
		tail := s.state.Queue[start:]
		if enabled {
			rand.Shuffle(len(tail), func(i, j int) { tail[i], tail[j] = tail[j], tail[i] })
		} else {
			order := map[string]int{}
			for i, t := range s.queueOrder {
				order[t.ID] = i
			}
			sort.SliceStable(tail, func(i, j int) bool { return order[tail[i].ID] < order[tail[j].ID] })
		}
		s.saveLocked()
	}
	s.mu.Unlock()
	value := s.snapshot()
	return response{State: &value}
}

// queueContinuation chooses how a queue grows on its last track.
// A playlist stays as it is. Random adds another batch of the saved genres.
// Anything else may add similar tracks.
func queueContinuation(exact, random, radioOn, atTail bool) string {
	if !radioOn || !atTail {
		return ""
	}
	if random {
		return "surprise"
	}
	if exact {
		return ""
	}
	return "radio"
}

// Prefetch only at the tail. A stopped/replaced queue and a disabled radio can
// never be revived by a slow recommendation request.
func (s *Server) fillRadio(g uint64, id string) {
	s.mu.Lock()
	atTail := s.state.Index+1 >= len(s.state.Queue)
	mode := queueContinuation(s.state.Exact, s.state.Random, s.settingsLocked().Radio, atTail)
	if s.generation != g || s.queueCast || mode == "" {
		s.mu.Unlock()
		return
	}
	genres := append([]string(nil), s.settingsLocked().Genres...)
	queue := append([]Track(nil), s.state.Queue...)
	task := s.radioLoad
	owner := task == nil || task.generation != g || task.id != id || task.mode != mode
	if owner {
		task = &radioLoad{generation: g, id: id, mode: mode, done: make(chan struct{})}
		s.radioLoad = task
	}
	s.mu.Unlock()
	if owner {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		if mode == "surprise" {
			task.tracks, _ = s.catalog.surprise(ctx, queue, genres)
		} else {
			task.tracks = s.catalog.radio(ctx, id)
		}
		cancel()
		close(task.done)
	} else {
		<-task.done
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.generation != g || s.closing || s.queueCast {
		return
	}
	atTail = s.state.Index+1 >= len(s.state.Queue)
	if queueContinuation(s.state.Exact, s.state.Random, s.settingsLocked().Radio, atTail) != mode {
		return
	}
	added := newRadioTracks(s.state.Queue, task.tracks)
	s.queueOrder = append(s.queueOrder, added...)
	if s.settingsLocked().Shuffle {
		rand.Shuffle(len(added), func(i, j int) { added[i], added[j] = added[j], added[i] })
	}
	s.state.Queue = append(s.state.Queue, added...)
	s.saveLocked()
}

type radioLoad struct {
	generation uint64
	id         string
	mode       string
	done       chan struct{}
	tracks     []Track
}

func (s *Server) finishQueue(g uint64, id string) {
	s.fillRadio(g, id)
	s.mu.Lock()
	if s.generation != g || s.closing {
		s.mu.Unlock()
		return
	}
	if s.state.Index+1 < len(s.state.Queue) {
		s.state.Index++
		s.state.PositionMS = 0
		s.mu.Unlock()
		s.startGuarded(0, 0, &g)
		return
	}
	s.state.Status = "stopped"
	s.state.PositionMS = 0
	s.saveLocked()
	s.mu.Unlock()
}

// newRadioTracks returns radio tracks not already queued. YouTube radio often
// lists a song twice, as the audio track and as its music video under another
// ID; a song is the same when its main artist and its title without a
// "(Official Video)"-style suffix match. Of two versions the non-video wins.
func newRadioTracks(queue, radio []Track) []Track {
	seenID := map[string]bool{}
	seenSong := map[string]bool{}
	for _, track := range queue {
		seenID[track.ID] = true
		seenSong[songKey(track)] = true
	}
	added := []Track{}
	at := map[string]int{}
	for _, track := range radio {
		key := songKey(track)
		if seenID[track.ID] || seenSong[key] {
			continue
		}
		seenID[track.ID] = true
		if i, ok := at[key]; ok {
			if musicVideo(added[i].Title) && !musicVideo(track.Title) {
				added[i] = track
			}
			continue
		}
		at[key] = len(added)
		added = append(added, track)
	}
	return added
}

var videoSuffix = regexp.MustCompile(`(?i)\s*[\(\[][^\)\]]*\b(official|video|clip|lyrics?|audio|visuali[sz]er|hd|4k|remaster(ed)?)\b[^\)\]]*[\)\]]`)

// Marketing suffixes may also name a distinct performance. Removing an
// entire "(Official Remix)" or "(Live Video)" would discard that version.
var performanceMarker = regexp.MustCompile(`(?i)\b(remix|mix|live|acoustic|instrumental|edit|sped|slowed|karaoke)\b`)

var videoMarker = regexp.MustCompile(`(?i)[\(\[][^\)\]]*\b(video|clip|lyrics?|visuali[sz]er|live)\b`)

// musicVideo reports a clip/lyric/live upload rather than the audio track.
func musicVideo(title string) bool {
	return videoMarker.MatchString(title)
}

func songKey(track Track) string {
	cleanTitle := videoSuffix.ReplaceAllStringFunc(track.Title, func(suffix string) string {
		if performanceMarker.MatchString(suffix) {
			return suffix
		}
		return ""
	})
	title := strings.Join(searchWords(cleanTitle), " ")
	artist := strings.ToLower(track.Artists)
	for _, sep := range []string{",", "&", " и ", " feat", " ft.", " x "} {
		if i := strings.Index(artist, sep); i > 0 {
			artist = artist[:i]
		}
	}
	return strings.Join(searchWords(artist), " ") + "|" + title
}
