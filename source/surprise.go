package main

import (
	"context"
	"errors"
	"math/rand/v2"
	"regexp"
	"strings"
)

const (
	surpriseLimit       = 15
	surpriseSearches    = 4
	surpriseMaxSearches = 6
)

// Broad genres, not the current chart. A random mood or year is added so two
// clicks do not search the same thing.
var surpriseGenres = []string{
	"джаз", "блюз", "соул", "фанк", "диско", "босса-нова", "свинг",
	"инди", "фолк", "авторская песня", "шансон", "романс", "баллада",
	"рок", "панк", "постпанк", "метал", "новая волна",
	"электроника", "синтипоп", "эмбиент", "техно", "хаус", "транс",
	"хип-хоп", "регги", "ска", "латина", "фламенко", "кантри",
	"классика", "фортепиано", "гитара", "саксофон",
	"city pop", "lo-fi", "synthwave", "k-pop", "j-pop",
	"скандинавский фолк",
}

var surpriseModifiers = []string{
	"", "ночь", "утро", "дождь", "город", "дорога", "любовь", "осень",
	"зима", "лето", "море", "тишина", "танец", "память", "письмо",
	"окно", "поезд", "1978", "1986", "1994", "2003", "акустика",
}

func knownSurpriseGenre(query string) (string, bool) {
	query = strings.TrimSpace(query)
	for _, genre := range surpriseGenres {
		if strings.EqualFold(genre, query) {
			return genre, true
		}
	}
	return "", false
}

// knownSurpriseGenres accepts a comma-separated list. An empty query means
// every genre.
func knownSurpriseGenres(query string) ([]string, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	var genres []string
	seen := map[string]bool{}
	for _, part := range strings.Split(query, ",") {
		genre, ok := knownSurpriseGenre(part)
		if !ok || seen[genre] {
			return nil, errors.New("неизвестный жанр")
		}
		seen[genre] = true
		genres = append(genres, genre)
	}
	if len(genres) > surpriseMaxSearches {
		return nil, errors.New("можно выбрать не больше 6 жанров")
	}
	return genres, nil
}

func surpriseQueries(n int, genres []string, rng *rand.Rand) []string {
	if n <= 0 {
		return nil
	}
	mods := append([]string(nil), surpriseModifiers...)
	rng.Shuffle(len(mods), func(i, j int) { mods[i], mods[j] = mods[j], mods[i] })
	chosen := append([]string(nil), genres...)
	if len(chosen) == 0 {
		order := rng.Perm(len(surpriseGenres))
		if n > len(surpriseGenres) {
			n = len(surpriseGenres)
		}
		chosen = make([]string, n)
		for i := 0; i < n; i++ {
			chosen[i] = surpriseGenres[order[i]]
		}
	}
	searches := n
	if len(genres) > searches {
		searches = len(genres)
	}
	out := make([]string, 0, searches)
	seen := map[string]bool{}
	for i := 0; i < searches; i++ {
		query := chosen[i%len(chosen)]
		if mod := mods[i%len(mods)]; mod != "" {
			query += " " + mod
		}
		if seen[query] {
			query = chosen[i%len(chosen)]
		}
		seen[query] = true
		out = append(out, query)
	}
	return out
}

// surprise adds songs from several random genre searches. The top few hits of
// each search are skipped, remixes stay out, and the current queue is kept.
func (c *Catalog) surprise(ctx context.Context, queue []Track, genres []string) ([]Track, error) {
	rng := rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))
	var pool []Track
	var last error
	for _, query := range surpriseQueries(surpriseSearches, genres, rng) {
		if ctx.Err() != nil {
			break
		}
		tracks, err := c.lookup(ctx, query)
		if err != nil {
			last = err
			continue
		}
		if len(tracks) > 8 {
			tracks = tracks[3:]
		}
		pool = append(pool, tracks...)
	}
	if len(pool) == 0 {
		if last != nil {
			return nil, last
		}
		return nil, errors.New("YouTube Music не нашёл случайные треки")
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
