package main

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/flynn/noise"
)

// Playback through Pulsar's ESPHome media player. The existing HTTP relay
// stays: Pulsar's GStreamer fetches http://127.0.0.1:18883/audio/<token>
// on the station, so the signed YouTube URL never leaves this process.
//
// The Noise key is YTM_PULSAR_API_KEY or
// /data/pulsar/api_key (the file ctl.sh api-key writes). The API address
// defaults to 127.0.0.1:6053.

const (
	pulsarPlay  = 0
	pulsarPause = 1
	pulsarStop  = 2

	esphomeHelloReq        = 1
	esphomeHelloResp       = 2
	esphomeDisconnectReq   = 5
	esphomeDisconnectResp  = 6
	esphomePingReq         = 7
	esphomePingResp        = 8
	esphomeSubscribeStates = 20
	esphomeMediaState      = 64
	esphomeMediaCmd        = 65

	// Pulsar's only media player entity (pulsar-esphome key::MEDIA).
	pulsarMediaKey = 1

	mediaStateIdle    = 1
	mediaStatePlaying = 2
	mediaStatePaused  = 3
)

func relayHost() string {
	if h := os.Getenv("YTM_PULSAR_RELAY"); h != "" {
		return h
	}
	return relayAddr
}

// pulsarClient keeps one API connection for the whole service. Pulsar has a
// single media channel and at most four API clients: a connection per track
// could evict Home Assistant and let a late Stop on the old connection cut
// the next track.
type pulsarClient struct {
	mu      sync.Mutex
	conn    net.Conn
	session *noisePair
	ready   bool
	onState func(int)
	// Last volume Pulsar reported, as float32 bits; valid once haveVolume.
	volumeBits atomic.Uint32
	muted      atomic.Bool
	haveVolume atomic.Bool
}

// reportedVolume is Pulsar's media player volume in percent.
func (c *pulsarClient) reportedVolume() (int, bool, bool) {
	if !c.haveVolume.Load() {
		return 0, false, false
	}
	level := math.Float32frombits(c.volumeBits.Load())
	return int(math.Round(float64(level) * 100)), c.muted.Load(), true
}

var errPulsarOffline = errors.New("API Pulsar недоступен")

// pulsarAPI is the service's client, for helpers outside Server (cast volume).
var pulsarAPI atomic.Pointer[pulsarClient]

func (c *pulsarClient) run(ctx interface{ Done() <-chan struct{} }) {
	for {
		if err := c.session1(ctx); err != nil {
			log.Print("Pulsar API: ", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func (c *pulsarClient) session1(ctx interface{ Done() <-chan struct{} }) error {
	conn, session, err := dialPulsar()
	if err != nil {
		return err
	}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			c.mu.Lock()
			if c.conn == conn && c.ready {
				_ = writeEsphome(conn, session, esphomeDisconnectReq, nil)
			}
			c.mu.Unlock()
			conn.Close()
		case <-stop:
		}
	}()
	c.mu.Lock()
	c.conn, c.session, c.ready = conn, session, false
	err = writeEsphome(conn, session, esphomeHelloReq, esphomeHello())
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		if c.conn == conn {
			c.conn, c.session, c.ready = nil, nil, false
		}
		c.mu.Unlock()
		conn.Close()
	}()
	if err != nil {
		return err
	}
	reader := bufio.NewReader(conn)
	for {
		msgType, msg, err := readEsphome(reader, session)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return errors.New("соединение закрыто")
			}
			return err
		}
		switch msgType {
		case esphomeHelloResp:
			c.mu.Lock()
			c.ready = true
			err = writeEsphome(conn, session, esphomeSubscribeStates, nil)
			c.mu.Unlock()
			if err != nil {
				return err
			}
			log.Print("Pulsar API connected")
		case esphomePingReq:
			c.write(conn, esphomePingResp, nil)
		case esphomeDisconnectReq:
			c.write(conn, esphomeDisconnectResp, nil)
			return errors.New("Pulsar закрыл соединение")
		case esphomeMediaState:
			key, _ := protobufUint(msg, 1)
			if key != pulsarMediaKey {
				continue
			}
			volume, _ := protobufUint(msg, 3)
			muted, _ := protobufUint(msg, 4)
			c.volumeBits.Store(uint32(volume))
			c.muted.Store(muted != 0)
			c.haveVolume.Store(true)
			if st, ok := protobufUint(msg, 2); ok && c.onState != nil {
				c.onState(int(st))
			}
		}
	}
}

func (c *pulsarClient) write(conn net.Conn, msgType uint32, msg []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == conn {
		_ = writeEsphome(conn, c.session, msgType, msg)
	}
}

func (c *pulsarClient) send(msgType uint32, msg []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil || !c.ready {
		return errPulsarOffline
	}
	return writeEsphome(c.conn, c.session, msgType, msg)
}

func (c *pulsarClient) play(url string) error {
	return c.send(esphomeMediaCmd, esphomeMediaURL(url))
}

func (c *pulsarClient) command(command int) error {
	return c.send(esphomeMediaCmd, esphomeMediaCommand(command))
}

func (c *pulsarClient) volume(level float32) error {
	return c.send(esphomeMediaCmd, esphomeMediaVolume(level))
}

func (s *Server) startPulsarLocked(g uint64, trackID, token string, src streamInfo, win *seekWindow, attempt int) {
	// After a seek the audio starts at the fragment that contains the
	// requested position, but the position stays the requested one: Cast
	// senders compare it with their own and seek again on every mismatch.
	p := &nativePlayer{token: token, source: src, attempt: attempt, window: win, pulsar: s.pulsar}
	s.player = p
	target := "http://" + relayHost() + "/audio/" + token
	if src.live {
		// Segments of a live stream come and go; GStreamer's HLS demuxer
		// follows the manifest itself. The relay serves files of known length.
		target = src.url
		p.fetched.Store(true)
	}
	if err := s.pulsar.play(target); err != nil {
		s.player = nil
		s.failLocked("Pulsar: " + err.Error())
		return
	}
	go s.fillRadio(g, trackID)
}

// onPulsarState follows the media player of the current track. Idle counts
// as the end of the track only after Pulsar played it and the relay served
// its last byte: a Stop, a Home Assistant command or a failed stream also end
// in Idle.
//
// Pulsar answers every command with its state as it is at that moment, so
// the previous track's Playing and Idle can arrive after the next track was
// sent. Playing counts only once Pulsar has fetched this track from the relay.
func (s *Server) onPulsarState(st int) {
	s.mu.Lock()
	p := s.player
	if p == nil {
		s.mu.Unlock()
		return
	}
	g := s.generation
	now := time.Now()
	switch st {
	case mediaStatePlaying:
		if !p.fetched.Load() {
			break
		}
		p.sawPlaying = true
		if p.playingSince.IsZero() {
			p.playingSince = now
		}
		if s.state.Status != "playing" {
			s.state.Status = "playing"
			s.state.Error = ""
			s.saveLocked()
		}
	case mediaStatePaused:
		s.state.PositionMS = p.positionMS(s.state.PositionMS, now)
		p.playingSince = time.Time{}
		if p.sawPlaying && s.state.Status != "buffering" {
			s.state.Status = "paused"
		}
	case mediaStateIdle:
		if !p.sawPlaying {
			break
		}
		s.state.PositionMS = p.positionMS(s.state.PositionMS, now)
		s.player = nil
		if p.reachedEnd.Load() {
			s.state.Status = "buffering"
			id := s.state.Queue[s.state.Index].ID
			s.mu.Unlock()
			s.finishQueue(g, id)
			return
		}
		s.state.Status = "stopped"
		s.state.Error = "воспроизведение остановлено на колонке"
		s.saveLocked()
	}
	s.mu.Unlock()
}

func dialPulsar() (net.Conn, *noisePair, error) {
	addr := os.Getenv("YTM_PULSAR_API")
	if addr == "" {
		addr = "127.0.0.1:6053"
	}
	key, encrypted, err := pulsarKey()
	if err != nil {
		return nil, nil, err
	}
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", addr, err)
	}
	if !encrypted {
		return conn, nil, nil
	}
	session, err := pulsarHandshake(conn, key)
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	return conn, session, nil
}

func pulsarKey() ([]byte, bool, error) {
	raw := os.Getenv("YTM_PULSAR_API_KEY")
	if raw == "" {
		path := os.Getenv("YTM_PULSAR_API_KEY_FILE")
		if path == "" {
			path = "/data/pulsar/api_key"
		}
		b, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, false, nil
			}
			return nil, false, err
		}
		raw = string(b)
	}
	raw = strings.Trim(raw, " \t\r\n")
	if raw == "" {
		return nil, false, nil
	}
	key, err := base64.StdEncoding.DecodeString(raw)
	if err != nil || len(key) != 32 {
		return nil, false, errors.New("ключ Pulsar должен быть base64 от 32 байт")
	}
	for _, b := range key {
		if b != 0 {
			return key, true, nil
		}
	}
	return nil, false, nil
}

// noisePair is the transport split: send to Pulsar, recv from Pulsar.
// flynn/noise inserts the psk0 token itself when PresharedKey is set, so the
// pattern is NN and the handshake name becomes Noise_NNpsk0_...
type noisePair struct {
	send *noise.CipherState
	recv *noise.CipherState
}

func pulsarHandshake(conn net.Conn, key []byte) (*noisePair, error) {
	prologue := append([]byte("NoiseAPIInit"), 0, 0)
	hs, err := noise.NewHandshakeState(noise.Config{
		CipherSuite: noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashSHA256),
		Pattern: noise.HandshakePattern{
			Name: "NN",
			Messages: [][]noise.MessagePattern{
				{noise.MessagePatternE},
				{noise.MessagePatternE, noise.MessagePatternDHEE},
			},
		},
		Initiator:    true,
		Prologue:     prologue,
		PresharedKey: key,
		Random:       rand.Reader,
	})
	if err != nil {
		return nil, err
	}
	msg, _, _, err := hs.WriteMessage(nil, nil)
	if err != nil {
		return nil, err
	}
	out := appendNoise(nil, nil)
	out = appendNoise(out, append([]byte{0x00}, msg...))
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	defer conn.SetDeadline(time.Time{})
	if _, err := conn.Write(out); err != nil {
		return nil, err
	}
	if _, err := readNoiseBody(conn); err != nil {
		return nil, fmt.Errorf("server hello: %w", err)
	}
	reply, err := readNoiseBody(conn)
	if err != nil {
		return nil, fmt.Errorf("handshake: %w", err)
	}
	if len(reply) == 0 || reply[0] != 0 {
		reason := "отклонён"
		if len(reply) > 1 {
			reason = string(reply[1:])
		}
		return nil, errors.New("handshake: " + reason)
	}
	_, send, recv, err := hs.ReadMessage(nil, reply[1:])
	if err != nil {
		return nil, errors.New("ключ API не подошёл")
	}
	return &noisePair{send: send, recv: recv}, nil
}

func appendNoise(out []byte, payload []byte) []byte {
	out = append(out, 0x01)
	out = binary.BigEndian.AppendUint16(out, uint16(len(payload)))
	return append(out, payload...)
}

func readNoiseBody(r io.Reader) ([]byte, error) {
	var hdr [3]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	if hdr[0] != 0x01 {
		return nil, errors.New("ожидался кадр Noise")
	}
	body := make([]byte, binary.BigEndian.Uint16(hdr[1:]))
	_, err := io.ReadFull(r, body)
	return body, err
}

func esphomeHello() []byte {
	var b []byte
	b = appendString(b, 1, "ytm-pulsar")
	b = appendVarintField(b, 2, 1)
	b = appendVarintField(b, 3, 15)
	return b
}

func esphomeMediaURL(url string) []byte {
	b := appendFixed32(nil, 1, pulsarMediaKey)
	b = appendBool(b, 6, true)
	return appendString(b, 7, url)
}

func esphomeMediaCommand(command int) []byte {
	b := appendFixed32(nil, 1, pulsarMediaKey)
	b = appendBool(b, 2, true)
	return appendVarintField(b, 3, uint64(command))
}

func esphomeMediaVolume(level float32) []byte {
	b := appendFixed32(nil, 1, pulsarMediaKey)
	b = appendBool(b, 4, true)
	return appendFixed32(b, 5, math.Float32bits(level))
}

func writeEsphome(conn net.Conn, session *noisePair, msgType uint32, msg []byte) error {
	_ = conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	defer conn.SetWriteDeadline(time.Time{})
	if session == nil {
		_, err := conn.Write(appendPlain(msgType, msg))
		return err
	}
	plain := binary.BigEndian.AppendUint16(nil, uint16(msgType))
	plain = binary.BigEndian.AppendUint16(plain, uint16(len(msg)))
	plain = append(plain, msg...)
	box, err := session.send.Encrypt(nil, nil, plain)
	if err != nil {
		return err
	}
	_, err = conn.Write(appendNoise(nil, box))
	return err
}

func readEsphome(r *bufio.Reader, session *noisePair) (uint32, []byte, error) {
	if session == nil {
		return readPlain(r)
	}
	body, err := readNoiseBody(r)
	if err != nil {
		return 0, nil, err
	}
	plain, err := session.recv.Decrypt(nil, nil, body)
	if err != nil {
		return 0, nil, err
	}
	if len(plain) < 4 {
		return 0, nil, errors.New("короткий кадр")
	}
	msgType := uint32(binary.BigEndian.Uint16(plain[0:2]))
	n := int(binary.BigEndian.Uint16(plain[2:4]))
	if n > len(plain)-4 {
		return 0, nil, errors.New("битая длина кадра")
	}
	return msgType, plain[4 : 4+n], nil
}

func appendPlain(msgType uint32, msg []byte) []byte {
	out := []byte{0x00}
	out = appendVarint(out, uint64(len(msg)))
	out = appendVarint(out, uint64(msgType))
	return append(out, msg...)
}

func readPlain(r *bufio.Reader) (uint32, []byte, error) {
	b, err := r.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	if b != 0x00 {
		return 0, nil, errors.New("ожидался открытый кадр API")
	}
	n, err := readVarint(r)
	if err != nil {
		return 0, nil, err
	}
	if n > 1<<20 {
		return 0, nil, errors.New("слишком длинный кадр API")
	}
	msgType, err := readVarint(r)
	if err != nil {
		return 0, nil, err
	}
	msg := make([]byte, n)
	if _, err := io.ReadFull(r, msg); err != nil {
		return 0, nil, err
	}
	return uint32(msgType), msg, nil
}

func appendVarint(out []byte, v uint64) []byte {
	for v >= 0x80 {
		out = append(out, byte(v)|0x80)
		v >>= 7
	}
	return append(out, byte(v))
}

func readVarint(r *bufio.Reader) (uint64, error) {
	var v uint64
	for shift := uint(0); shift < 70; shift += 7 {
		b, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		v |= uint64(b&0x7f) << shift
		if b < 0x80 {
			return v, nil
		}
	}
	return 0, errors.New("битый varint")
}

func appendVarintField(out []byte, field uint32, v uint64) []byte {
	out = appendVarint(out, uint64(field)<<3)
	return appendVarint(out, v)
}

func appendBool(out []byte, field uint32, v bool) []byte {
	if v {
		return appendVarintField(out, field, 1)
	}
	return appendVarintField(out, field, 0)
}

func appendFixed32(out []byte, field, v uint32) []byte {
	out = appendVarint(out, uint64(field)<<3|5)
	return binary.LittleEndian.AppendUint32(out, v)
}

func appendString(out []byte, field uint32, s string) []byte {
	out = appendVarint(out, uint64(field)<<3|2)
	out = appendVarint(out, uint64(len(s)))
	return append(out, s...)
}

func protobufUint(msg []byte, field uint32) (uint64, bool) {
	for i := 0; i < len(msg); {
		key, n := varintAt(msg[i:])
		if n == 0 {
			return 0, false
		}
		i += n
		num := uint32(key >> 3)
		switch key & 7 {
		case 0:
			v, n := varintAt(msg[i:])
			if n == 0 {
				return 0, false
			}
			i += n
			if num == field {
				return v, true
			}
		case 5:
			if i+4 > len(msg) {
				return 0, false
			}
			if num == field {
				return uint64(binary.LittleEndian.Uint32(msg[i:])), true
			}
			i += 4
		case 1:
			i += 8
		case 2:
			ln, n := varintAt(msg[i:])
			if n == 0 || uint64(len(msg)-i-n) < ln {
				return 0, false
			}
			i += n + int(ln)
		default:
			return 0, false
		}
	}
	return 0, false
}

func varintAt(b []byte) (uint64, int) {
	var v uint64
	for i, c := range b {
		if i == 10 {
			return 0, 0
		}
		v |= uint64(c&0x7f) << (7 * uint(i))
		if c < 0x80 {
			return v, i + 1
		}
	}
	return 0, 0
}
