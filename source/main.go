package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const version = "0.7.6"
const relayAddr = "127.0.0.1:18883"

func dataDir() string {
	if s := os.Getenv("YTM_ALICE_DATA"); s != "" {
		return s
	}
	if s := os.Getenv("YAM_VAR"); s != "" {
		return filepath.Join(s, "local.ytm-alice")
	}
	root := os.Getenv("YAM_ROOT")
	if root == "" {
		root = "/data/yam"
	}
	return filepath.Join(root, "var", "local.ytm-alice")
}
func runDir() string     { return "/tmp/ytm-alice-run" }
func socketPath() string { return filepath.Join(runDir(), "control.sock") }

type State struct {
	Version    string      `json:"version"`
	Status     string      `json:"status"`
	Queue      []Track     `json:"queue"`
	Index      int         `json:"index"`
	PositionMS int64       `json:"position_ms"`
	Error      string      `json:"error,omitempty"`
	LoggedIn   bool        `json:"logged_in"`
	Radio      bool        `json:"radio"`
	Shuffle    bool        `json:"shuffle"`
	Volume     int         `json:"volume"`
	HasVolume  bool        `json:"has_volume,omitempty"`
	Muted      bool        `json:"muted,omitempty"`
	Auth       *AuthStatus `json:"auth,omitempty"`
}
type request struct {
	Action     string         `json:"action"`
	Query      string         `json:"query,omitempty"`
	Index      int            `json:"index,omitempty"`
	PositionMS int64          `json:"position_ms,omitempty"`
	Volume     int            `json:"volume,omitempty"`
	Auth       *Credentials   `json:"auth,omitempty"`
	Settings   *SettingsPatch `json:"settings,omitempty"`
}
type response struct {
	State     *State           `json:"state,omitempty"`
	Tracks    []Track          `json:"tracks,omitempty"`
	Error     string           `json:"error,omitempty"`
	Auth      *AuthStatus      `json:"auth,omitempty"`
	Playlists []Playlist       `json:"playlists,omitempty"`
	Accounts  []YouTubeAccount `json:"accounts,omitempty"`
	TVLogin   *TVLoginState    `json:"tv_login,omitempty"`
	Settings  *Settings        `json:"settings,omitempty"`
	WebAddr   string           `json:"web_addr,omitempty"`
	Genres    []string         `json:"genres,omitempty"`
}

// nativePlayer is the current track on Pulsar's media player.
type nativePlayer struct {
	token   string
	source  streamInfo
	attempt int
	window  *seekWindow // set when playback starts mid-track
	pulsar  *pulsarClient
	// Guarded by Server.mu, except fetched and reachedEnd (set by the relay).
	sawPlaying   bool
	playingSince time.Time // zero while not playing
	fetched      atomic.Bool
	reachedEnd   atomic.Bool
}

// positionMS adds the time played since the last Playing to base.
func (p *nativePlayer) positionMS(base int64, now time.Time) int64 {
	if p.playingSince.IsZero() {
		return base
	}
	return base + now.Sub(p.playingSince).Milliseconds()
}

func (p *nativePlayer) send(line string) {
	if p.pulsar == nil {
		return
	}
	var err error
	switch line {
	case "pause":
		err = p.pulsar.command(pulsarPause)
	case "resume":
		err = p.pulsar.command(pulsarPlay)
	case "stop":
		err = p.pulsar.command(pulsarStop)
	}
	if err != nil {
		log.Print("Pulsar ", line, ": ", err)
	}
}
func (p *nativePlayer) stop() {
	p.send("stop")
}

type Server struct {
	mu               sync.Mutex
	state            State
	catalog          *Catalog
	player           *nativePlayer
	pulsar           *pulsarClient
	generation       uint64
	lookupGeneration uint64
	prepCancel       context.CancelFunc
	latestSearch     []Track
	closing          bool
	settings         *Settings
	webAddr          string
	queueOrder       []Track
	queueCast        bool
	radioLoad        *radioLoad
}

func (s *Server) snapshot() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.state
	options := s.settingsLocked()
	v.Radio, v.Shuffle = options.Radio, options.Shuffle
	v.Queue = append([]Track(nil), v.Queue...)
	for i := range v.Queue {
		if v.Queue[i].ThumbnailURL == "" {
			v.Queue[i].ThumbnailURL = fallbackThumbnail(v.Queue[i].ID)
		}
	}
	if s.player != nil {
		v.PositionMS = s.player.positionMS(v.PositionMS, time.Now())
	}
	v.Volume, v.Muted, v.HasVolume = shownVolume()
	v.LoggedIn = s.catalog != nil && s.catalog.credentials() != nil
	if s.catalog != nil {
		status := s.catalog.authStatus()
		v.Auth = &status
	}
	return v
}
func (s *Server) saveLocked() {
	raw, _ := json.Marshal(s.state)
	path := filepath.Join(dataDir(), "state.json")
	if err := os.WriteFile(path+".tmp", raw, 0600); err == nil {
		os.Rename(path+".tmp", path)
	}
}
func (s *Server) stopLocked() {
	s.generation++
	if s.prepCancel != nil {
		s.prepCancel()
		s.prepCancel = nil
	}
	if s.player != nil {
		s.player.stop()
		s.player = nil
	}
}
func (s *Server) command(ctx context.Context, r request) response {
	switch r.Action {
	case "config-get", "config-set":
		return s.settingsCommand(r)
	case "shuffle-on", "shuffle-off":
		return s.shuffleQueue(r.Action == "shuffle-on")
	case "auth-status", "auth-check", "auth-import", "auth-logout", "auth-discover", "auth-accounts", "auth-select", "auth-tv-start", "auth-tv-status", "auth-tv-cancel":
		return s.authCommand(ctx, r)
	case "playlists":
		playlists, err := s.catalog.library(ctx)
		if err != nil {
			return s.catalogError(err)
		}
		return response{Playlists: playlists}
	case "playlist":
		if err := s.catalog.requireAuth(ctx); err != nil {
			return s.catalogError(err)
		}
		if !playlistID.MatchString(r.Query) {
			return response{Error: "нужен ID плейлиста из команды playlists"}
		}
		return s.command(ctx, request{Action: "play", Query: "https://music.youtube.com/playlist?list=" + r.Query})
	case "status", "queue":
		v := s.snapshot()
		return response{State: &v}
	case "volume":
		if r.Volume < 0 || r.Volume > 100 {
			return response{Error: "громкость от 0 до 100"}
		}
		requestVolume(r.Volume)
		v := s.snapshot()
		return response{State: &v}
	case "search", "liked":
		epoch := s.catalog.authEpoch.Load()
		query := r.Query
		if r.Action == "liked" {
			query = "мои лайки"
		}
		t, e := s.catalog.lookup(ctx, query)
		if e != nil {
			if r.Action == "liked" && errors.Is(e, errNoTracks) {
				return response{Tracks: []Track{}}
			}
			return s.catalogError(e)
		}
		s.mu.Lock()
		if epoch != s.catalog.authEpoch.Load() {
			s.mu.Unlock()
			return response{Error: "аккаунт изменился; повторите поиск"}
		}
		s.latestSearch = append([]Track(nil), t...)
		s.mu.Unlock()
		return response{Tracks: t}
	case "play", "recommendations":
		s.mu.Lock()
		// Resolve before stopping: an irrelevant/missing result must not
		// interrupt the song already playing (#14).
		s.lookupGeneration++
		lookup := s.lookupGeneration
		g := s.generation
		s.mu.Unlock()
		var tracks []Track
		var e error
		if r.Action == "recommendations" {
			tracks, e = s.catalog.recommendations(ctx)
			// A personal mix lists songs twice like radio does (audio and
			// clip under different IDs).
			tracks = newRadioTracks(nil, tracks)
		} else {
			tracks, e = s.catalog.playLookup(ctx, r.Query)
		}
		s.mu.Lock()
		if s.generation != g || s.lookupGeneration != lookup {
			s.mu.Unlock()
			return response{Error: "запрос заменён более новым"}
		}
		if e != nil {
			s.mu.Unlock()
			return response{Error: e.Error()}
		}
		index := -1
		lower := strings.ToLower(r.Query)
		if r.Action == "play" && !isLikedQuery(r.Query) && !strings.HasPrefix(lower, "альбом ") && !strings.HasPrefix(lower, "плейлист ") && !strings.Contains(r.Query, "list=") {
			index = 0
		}
		s.installQueueLocked(tracks, index)
		s.state.PositionMS = 0
		s.saveLocked()
		s.mu.Unlock()
		s.startGuarded(0, 0, &g)
	case "genres":
		return response{Genres: append([]string(nil), surpriseGenres...)}
	case "surprise":
		genres, err := knownSurpriseGenres(r.Query)
		if err != nil {
			return response{Error: err.Error()}
		}
		s.mu.Lock()
		s.lookupGeneration++
		lookup := s.lookupGeneration
		g := s.generation
		queue := append([]Track(nil), s.state.Queue...)
		s.mu.Unlock()
		tracks, e := s.catalog.surprise(ctx, queue, genres)
		s.mu.Lock()
		if s.generation != g || s.lookupGeneration != lookup {
			s.mu.Unlock()
			return response{Error: "запрос заменён более новым"}
		}
		if e != nil {
			s.mu.Unlock()
			return response{Error: e.Error()}
		}
		added, start := s.appendSurpriseLocked(tracks)
		s.mu.Unlock()
		if added == 0 {
			return response{Error: "все эти треки уже есть в очереди"}
		}
		if start {
			s.startGuarded(0, 0, &g)
		}
	case "select", "jump":
		s.mu.Lock()
		i := r.Index - 1
		tracks := s.latestSearch
		message := "номер вне результатов поиска"
		if r.Action == "jump" {
			tracks = s.state.Queue
			message = "номер вне очереди"
		}
		if i < 0 || i >= len(tracks) {
			s.mu.Unlock()
			return response{Error: message}
		}
		if r.Query != "" && r.Query != tracks[i].ID {
			s.mu.Unlock()
			return response{Error: "список изменился; обновите его и выберите трек снова"}
		}
		s.installQueueLocked(tracks, i)
		s.state.PositionMS = 0
		s.mu.Unlock()
		s.start(0)
	case "next", "previous", "resume", "pause", "toggle", "stop", "seek", "clear":
		s.mu.Lock()
		s.lookupGeneration++
		if r.Action == "toggle" {
			r.Action = "resume"
			if s.state.Status == "playing" || s.state.Status == "buffering" || s.state.Status == "searching" {
				r.Action = "pause"
			}
		}
		switch r.Action {
		case "pause":
			if s.player != nil {
				s.player.send("pause")
				s.state.Status = "paused"
			} else if s.state.Status == "buffering" || s.state.Status == "searching" {
				s.stopLocked()
				s.state.Status = "paused"
			} else {
				s.mu.Unlock()
				return response{Error: "плеер не запущен"}
			}
		case "resume":
			if s.player != nil {
				s.player.send("resume")
				s.state.Status = "playing"
			} else {
				s.mu.Unlock()
				s.start(0)
				v := s.snapshot()
				return response{State: &v}
			}
		case "stop":
			s.stopLocked()
			s.state.Status = "stopped"
			s.state.PositionMS = 0
			s.state.Error = ""
		case "clear":
			s.stopLocked()
			s.state.Queue = []Track{}
			s.state.Index = 0
			s.state.PositionMS = 0
			s.state.Status = "stopped"
			s.state.Error = ""
			s.queueOrder = nil
			s.queueCast = false
		case "seek":
			if r.PositionMS < 0 || r.PositionMS > 7200000 {
				s.mu.Unlock()
				return response{Error: "нужна позиция от 0 до 7200 секунд"}
			}
			if len(s.state.Queue) == 0 {
				s.mu.Unlock()
				return response{Error: "плеер не запущен"}
			}
			if r.PositionMS > 0 && s.player != nil && !s.player.source.hasSeekIndex() {
				s.mu.Unlock()
				return response{Error: "этот аудиопоток не поддерживает перемотку"}
			}
			// Restart the track from the fragment that contains the position (see seek.go).
			s.mu.Unlock()
			s.start(max(r.PositionMS, 0))
			v := s.snapshot()
			return response{State: &v}
		case "next", "previous":
			step := 1
			if r.Action == "previous" {
				step = -1
			}
			i := s.state.Index + step
			if i < 0 || i >= len(s.state.Queue) {
				s.mu.Unlock()
				return response{Error: "достигнут край очереди"}
			}
			s.state.Index = i
			s.state.PositionMS = 0
			s.mu.Unlock()
			s.start(0)
			v := s.snapshot()
			return response{State: &v}
		}
		s.saveLocked()
		s.mu.Unlock()
	default:
		return response{Error: "неизвестная команда"}
	}
	v := s.snapshot()
	return response{State: &v}
}
func (s *Server) start(offset int64) {
	s.startGuarded(offset, 0, nil)
}
func (s *Server) startGuarded(offset int64, attempt int, expected *uint64) {
	s.mu.Lock()
	if expected != nil && s.generation != *expected {
		s.mu.Unlock()
		return
	}
	if s.closing || len(s.state.Queue) == 0 {
		s.mu.Unlock()
		return
	}
	s.stopLocked()
	g := s.generation
	t := s.state.Queue[s.state.Index]
	s.state.Status = "buffering"
	s.state.Error = ""
	s.state.PositionMS = offset
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	s.prepCancel = cancel
	s.saveLocked()
	s.mu.Unlock()
	go func() {
		defer cancel()
		src, err := s.catalog.resolve(ctx, t.ID)
		var win *seekWindow
		if err == nil && offset > 0 {
			var werr error
			if win, werr = s.catalog.seekWindow(ctx, src, offset); werr != nil {
				err = fmt.Errorf("не удалось перемотать: %v", werr)
			}
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.generation != g || s.closing {
			return
		}
		if err != nil {
			var unavailable unavailableTrack
			if offset == 0 && errors.As(err, &unavailable) && s.state.Index+1 < len(s.state.Queue) {
				log.Printf("skipping unavailable track=%s reason=%s", t.ID, logSafe(err.Error()))
				s.state.Index++
				s.state.PositionMS = 0
				s.saveLocked()
				// Never extend radio or wrap here: an inaccessible queue must
				// terminate.
				go s.startGuarded(0, 0, &g)
				return
			}
			s.state.Status = "error"
			s.state.Error = err.Error()
			s.saveLocked()
			return
		}
		tokenBytes := make([]byte, 16)
		rand.Read(tokenBytes)
		if s.state.Index >= 0 && s.state.Index < len(s.state.Queue) && s.state.Queue[s.state.Index].ID == t.ID {
			if n := applyStreamMeta(s.state.Queue[s.state.Index], src); n != s.state.Queue[s.state.Index] {
				s.state.Queue[s.state.Index] = n
				t = n
				s.saveLocked()
			}
		}
		s.startPulsarLocked(g, t.ID, hex.EncodeToString(tokenBytes), src, win, attempt)
	}()
}
func (s *Server) failLocked(message string) {
	s.state.Status = "error"
	s.state.Error = message
	s.saveLocked()
}
func (s *Server) relay(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" && r.Method != "HEAD" {
		w.WriteHeader(405)
		return
	}
	s.mu.Lock()
	p := s.player
	if p == nil || r.URL.Path != "/audio/"+p.token {
		s.mu.Unlock()
		http.NotFound(w, r)
		return
	}
	src := p.source
	win := p.window
	s.mu.Unlock()
	p.fetched.Store(true)
	if r.Method == "GET" && r.Header.Get("Range") == "" && strings.HasPrefix(src.mime, "audio/mp4") && src.length > 0 {
		s.relayADTS(w, r, p, src, win)
		return
	}
	if win != nil {
		if r.Method == "HEAD" {
			w.Header().Set("Content-Type", src.mime)
			w.Header().Set("Accept-Ranges", "bytes")
			w.Header().Set("Content-Length", strconv.FormatInt(win.length(src), 10))
			w.WriteHeader(http.StatusOK)
			return
		}
		s.relayWindow(w, r, p, src, win)
		return
	}
	if r.Method == "HEAD" {
		w.Header().Set("Content-Type", src.mime)
		w.Header().Set("Accept-Ranges", "bytes")
		if src.length > 0 {
			w.Header().Set("Content-Length", strconv.FormatInt(src.length, 10))
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	start, end, err := byteRange(r.Header.Get("Range"), src.length)
	if err != nil {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", src.length))
		w.WriteHeader(416)
		return
	}
	s.relayAudio(w, r, p, src, start, end)
}

func byteRange(header string, length int64) (int64, int64, error) {
	if length <= 0 {
		return 0, 0, errors.New("unknown length")
	}
	if header == "" {
		return 0, length - 1, nil
	}
	if !strings.HasPrefix(header, "bytes=") || strings.Contains(header, ",") {
		return 0, 0, errors.New("invalid range")
	}
	pair := strings.SplitN(strings.TrimPrefix(header, "bytes="), "-", 2)
	if len(pair) != 2 {
		return 0, 0, errors.New("invalid range")
	}
	start, end := int64(0), length-1
	var err error
	if pair[0] == "" {
		n, e := strconv.ParseInt(pair[1], 10, 64)
		if e != nil || n <= 0 {
			return 0, 0, errors.New("invalid suffix")
		}
		start = length - n
		if start < 0 {
			start = 0
		}
	} else {
		start, err = strconv.ParseInt(pair[0], 10, 64)
		if err != nil || start < 0 || start >= length {
			return 0, 0, errors.New("invalid start")
		}
		if pair[1] != "" {
			end, err = strconv.ParseInt(pair[1], 10, 64)
			if err != nil || end < start {
				return 0, 0, errors.New("invalid end")
			}
			if end >= length {
				end = length - 1
			}
		}
	}
	return start, end, nil
}
func stationCertPool(paths ...string) *x509.CertPool {
	roots, _ := x509.SystemCertPool()
	if roots == nil {
		roots = x509.NewCertPool()
	}
	for _, path := range paths {
		if pem, err := os.ReadFile(path); err == nil {
			roots.AppendCertsFromPEM(pem)
		}
	}
	return roots
}
func newCatalog() *Catalog {
	roots := stationCertPool("/system/vendor/quasar/certs/ca-certificates.crt", "/system/vendor/quasar/ca-certificates.crt")
	h := &http.Client{Timeout: 40 * time.Second, Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}, MaxIdleConns: 4, MaxIdleConnsPerHost: 2, IdleConnTimeout: 30 * time.Second, ResponseHeaderTimeout: 15 * time.Second}}
	return &Catalog{http: h, cache: map[string]catalogEntry{}, streams: map[string]streamInfo{}}
}
func serve() error {
	debug.SetMemoryLimit(48 << 20)
	debug.SetGCPercent(50)
	if e := os.MkdirAll(dataDir(), 0700); e != nil {
		return e
	}
	if e := os.MkdirAll(runDir(), 0700); e != nil {
		return e
	}
	// An existing live socket belongs to another service instance.
	if c, e := net.DialTimeout("unix", socketPath(), time.Second); e == nil {
		c.Close()
		return errors.New("ytm-alice уже запущен")
	}
	os.Remove(socketPath())
	listener, e := net.Listen("unix", socketPath())
	if e != nil {
		return e
	}
	defer listener.Close()
	defer os.Remove(socketPath())
	os.Chmod(socketPath(), 0600)
	os.WriteFile(filepath.Join(runDir(), "pid"), []byte(strconv.Itoa(os.Getpid())), 0600)
	defer os.Remove(filepath.Join(runDir(), "pid"))
	s := &Server{state: State{Version: version, Status: "stopped", Queue: []Track{}}, catalog: newCatalog()}
	if err := s.loadSettings(); err != nil {
		return err
	}
	if err := s.catalog.loadAuth(); err != nil {
		log.Print(err)
	}
	if raw, e := os.ReadFile(filepath.Join(dataDir(), "state.json")); e == nil {
		json.Unmarshal(raw, &s.state)
		s.state.Version = version
		s.state.Status = "stopped"
		s.state.PositionMS = 0
		s.state.Error = ""
		if s.state.Index < 0 || s.state.Index >= len(s.state.Queue) {
			s.state.Index = 0
		}
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	go s.enrichCastQueue(ctx)
	closeWeb, webErr := s.startWeb()
	if webErr != nil {
		log.Print("web panel unavailable: ", webErr)
	} else {
		defer closeWeb()
	}
	httpServer := &http.Server{Addr: relayAddr, Handler: http.HandlerFunc(s.relay), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second}
	errCh := make(chan error, 1)
	go func() { errCh <- httpServer.ListenAndServe() }()
	// Pulsar owns the LEDs, the mixer and Bluetooth; Alice is not running.
	pulsarDone := make(chan struct{})
	s.pulsar = &pulsarClient{onState: s.onPulsarState}
	pulsarAPI.Store(s.pulsar)
	go func() {
		defer close(pulsarDone)
		s.pulsar.run(ctx)
	}()
	go s.castLoop(ctx)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go s.controlConnection(ctx, conn)
		}
	}()
	log.Print("ytm-pulsar ", version, " ready; Pulsar player, local control, direct YouTube")
	select {
	case <-ctx.Done():
	case e = <-errCh:
	}
	s.mu.Lock()
	s.closing = true
	s.stopLocked()
	s.state.Status = "stopped"
	s.saveLocked()
	s.mu.Unlock()
	cancel()
	select {
	case <-pulsarDone:
	case <-time.After(time.Second):
	}
	httpServer.Close()
	return e
}
func call(r request) (response, error) {
	c, e := net.DialTimeout("unix", socketPath(), 2*time.Second)
	if e != nil {
		return response{}, errors.New("ytm-alice не запущен: ytm-alice daemon start")
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(50 * time.Second))
	if e = json.NewEncoder(c).Encode(r); e != nil {
		return response{}, e
	}
	var out response
	e = json.NewDecoder(io.LimitReader(c, maxControlResponseBytes)).Decode(&out)
	return out, e
}
func printResponse(out response, asJSON bool) {
	if asJSON {
		json.NewEncoder(os.Stdout).Encode(out)
		return
	}
	if out.Error != "" {
		fmt.Fprintln(os.Stderr, out.Error)
		return
	}
	if out.Settings != nil {
		raw, _ := json.MarshalIndent(out.Settings, "", "  ")
		fmt.Println(string(raw))
		return
	}
	if out.TVLogin != nil && out.Auth == nil {
		printTVLogin(out.TVLogin)
		return
	}
	if out.Auth != nil {
		printAuth(out.Auth)
		return
	}
	if out.Playlists != nil {
		for i, p := range out.Playlists {
			fmt.Printf("%2d. %s\n    ytm-alice playlist %s\n", i+1, p.Title, p.ID)
		}
		return
	}
	if out.State != nil {
		s := out.State
		fmt.Printf("YouTube Music · %s · %s\n", s.Version, s.Status)
		if len(s.Queue) > 0 && s.Index >= 0 && s.Index < len(s.Queue) {
			t := s.Queue[s.Index]
			fmt.Printf("▶ %s — %s\n  %d/%d · %d:%02d / %d:%02d\n", t.Artists, t.Title, s.Index+1, len(s.Queue), s.PositionMS/60000, s.PositionMS/1000%60, t.DurationMS/60000, t.DurationMS/1000%60)
		}
		if s.Error != "" {
			fmt.Println("Ошибка:", s.Error)
		}
	} else {
		for i, t := range out.Tracks {
			fmt.Printf("%2d. %s — %s\n", i+1, t.Artists, t.Title)
		}
	}
}
func help() {
	fmt.Println("ytm-pulsar " + version + ` · YouTube Music для Яндекс Станций с Pulsar

Звук играет медиаплеер Pulsar (ESPHome API). Проверено на Станции Лайт 2 (plum).

  play "запрос или ссылка"   включить трек, альбом или плейлист
  search "запрос"            поиск; select N — выбрать результат
  jump N                     включить трек из текущей очереди
  login                      импорт JSON-сессии из stdin (клиент ПК: Firefox)
  login tv                   вход по коду через YouTube TV с автообновлением входа
  auth status | auth check   состояние / проверка входа
  logout                     удалить сессию YouTube Music
  liked | playlists          лайки / личные плейлисты
  playlist ID                включить личный плейлист
  play "мои лайки"            включить понравившиеся
  web status                 адреса локальной HTTP-панели
  status | queue             состояние и очередь
  pause | resume | toggle | stop  управление воспроизведением
  seek СЕКУНДЫ              перейти к позиции (ближайший аудиофрагмент)
  next | previous            следующий / предыдущий трек
  recommendations            персональные рекомендации (нужен вход)
  surprise                   случайные треки выбранных жанров
  surprise жанр,жанр         один или несколько жанров; пусто — все
  clear                      остановить и очистить очередь
  shuffle-on | shuffle-off   перемешивание очереди
  config get                 постоянные настройки
  config set КЛЮЧ ЗНАЧЕНИЕ    изменить настройку; порт — после перезапуска
  ui                         интерактивное меню
  --json КОМАНДА              машиночитаемый ответ

Без аргументов открывается меню; фоновый режим — serve.
Автозапуск: ytm-alice daemon install.`)
}
func ui() error {
	reader := bufio.NewScanner(os.Stdin)
	fmt.Println("ytm-alice · / запрос — поиск, p запрос — включить, номер — выбрать")
	fmt.Println("n/b — дальше/назад, space — пауза/продолжить, s — стоп, q — выход")
	for {
		out, e := call(request{Action: "status"})
		if e != nil {
			return e
		}
		printResponse(out, false)
		fmt.Print("ytm> ")
		if !reader.Scan() {
			return reader.Err()
		}
		line := reader.Text()
		trim := strings.TrimSpace(line)
		var r request
		switch {
		case trim == "q":
			return nil
		case strings.HasPrefix(trim, "/"):
			r = request{Action: "search", Query: strings.TrimSpace(trim[1:])}
		case strings.HasPrefix(trim, "p "):
			r = request{Action: "play", Query: strings.TrimSpace(trim[2:])}
		case trim == "n":
			r.Action = "next"
		case trim == "b":
			r.Action = "previous"
		case trim == "s":
			r.Action = "stop"
		case trim == "":
			r.Action = "pause"
			if out.State != nil && out.State.Status == "paused" {
				r.Action = "resume"
			}
		default:
			n, e := strconv.Atoi(trim)
			if e != nil {
				continue
			}
			r = request{Action: "select", Index: n}
		}
		out, e = call(r)
		if e != nil {
			return e
		}
		printResponse(out, false)
	}
}
func main() {
	args := os.Args[1:]
	asJSON := false
	for i := 0; i < len(args); {
		if args[i] == "--json" {
			asJSON = true
			args = append(args[:i], args[i+1:]...)
		} else {
			i++
		}
	}
	if len(args) == 0 {
		if info, e := os.Stdin.Stat(); e == nil && info.Mode()&os.ModeCharDevice != 0 && !asJSON {
			args = []string{"ui"}
		} else {
			args = []string{"status"}
		}
	}
	if args[0] == "serve" {
		if e := serve(); e != nil {
			fmt.Fprintln(os.Stderr, e)
			os.Exit(1)
		}
		return
	}
	switch args[0] {
	case "config":
		r, err := configRequest(args[1:])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		out, err := call(r)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		printResponse(out, asJSON)
		if out.Error != "" {
			os.Exit(1)
		}
		return
	case "web":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "web status")
			os.Exit(2)
		}
		if err := webCLI(args[1]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	case "auth":
		if len(args) != 2 || (args[1] != "status" && args[1] != "check") {
			fmt.Fprintln(os.Stderr, "auth status|check")
			os.Exit(2)
		}
		args = []string{"auth-" + args[1]}
	case "logout":
		args = []string{"auth-logout"}
	case "login":
		if len(args) == 2 && args[1] == "tv" {
			os.Exit(tvLoginCLI(asJSON))
		}
		var auth Credentials
		if json.NewDecoder(io.LimitReader(os.Stdin, maxControlRequestBytes)).Decode(&auth) != nil {
			fmt.Fprintln(os.Stderr, "нужен JSON сессии из stdin; на ПК: ytm-alice login")
			os.Exit(2)
		}
		out, err := call(request{Action: "auth-import", Auth: &auth})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		printResponse(out, asJSON)
		if out.Error != "" {
			os.Exit(1)
		}
		return
	}
	switch args[0] {
	case "--rpc", "rpc":
		if e := rpcLoop(os.Stdin, os.Stdout, call); e != nil {
			fmt.Fprintln(os.Stderr, e)
			os.Exit(1)
		}
		return
	case "daemon":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "daemon start|stop|restart|install|uninstall")
			os.Exit(2)
		}
		if e := daemon(args[1]); e != nil {
			fmt.Fprintln(os.Stderr, e)
			os.Exit(1)
		}
		return
	case "--help", "-h", "help":
		help()
		return
	case "--version", "version":
		fmt.Println(version)
		return
	case "ui":
		if e := ui(); e != nil {
			fmt.Fprintln(os.Stderr, e)
			os.Exit(1)
		}
		return
	case "probe":
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
		defer cancel()
		c := newCatalog()
		if err := c.loadAuth(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		t, e := c.lookup(ctx, strings.Join(args[1:], " "))
		if e != nil {
			fmt.Fprintln(os.Stderr, e)
			os.Exit(1)
		}
		fmt.Println("Found:", len(t), t[0].Title)
		src, e := c.resolve(ctx, t[0].ID)
		if e != nil {
			fmt.Fprintln(os.Stderr, e)
			os.Exit(1)
		}
		for _, offset := range []int64{0, src.length / 2} {
			end := offset + 65535
			if end >= src.length {
				end = src.length - 1
			}
			req, _ := http.NewRequestWithContext(ctx, "GET", src.url, nil)
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, end))
			resp, err := c.http.Do(req)
			if err != nil {
				fmt.Fprintln(os.Stderr, "stream network error")
				os.Exit(1)
			}
			n, readErr := io.Copy(io.Discard, io.LimitReader(resp.Body, 65536))
			resp.Body.Close()
			fmt.Println("Stream offset", offset, "HTTP", resp.StatusCode, "bytes", n)
			if resp.StatusCode != 206 || readErr != nil || n != end-offset+1 {
				os.Exit(1)
			}
		}
		return
	}
	action := args[0]
	switch action {
	case "p":
		action = "play"
	case "n":
		action = "next"
	case "b":
		action = "previous"
	case "s":
		action = "status"
	case "q":
		action = "queue"
	}
	r := request{Action: action, Query: strings.Join(args[1:], " ")}
	if action == "select" || action == "jump" {
		n, e := strconv.Atoi(r.Query)
		if e != nil || n < 1 {
			fmt.Fprintln(os.Stderr, "нужен номер трека, начиная с 1")
			os.Exit(2)
		}
		r.Index = n
	}
	if action == "seek" {
		v, e := strconv.ParseFloat(r.Query, 64)
		if e != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 7200 {
			fmt.Fprintln(os.Stderr, "нужна позиция в секундах")
			os.Exit(2)
		}
		r.PositionMS = int64(v * 1000)
	}
	out, e := call(r)
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
	if action == "queue" && out.State != nil {
		out.Tracks = out.State.Queue
		if !asJSON {
			out.State = nil
		}
	}
	printResponse(out, asJSON)
	if out.Error != "" {
		os.Exit(1)
	}
}
