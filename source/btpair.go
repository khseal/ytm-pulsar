package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"log"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

const (
	keyAction    = 582 // KEY_VOICECOMMAND on the aw933xx touch pad
	btpairHold   = 3 * time.Second
	btpairWindow = "300"
)

// pressState turns one hold of the action button into a single pairing
// request. Volume keys and a short tap never qualify. Pulsar still sees the
// same events: a short press starts or cancels its dialog, and its long press
// only reports an event.
type pressState struct {
	down  time.Time
	fired bool
}

// level reports whether this hold has just crossed the pairing threshold.
// Pulsar grabs the event stream, so the watcher polls the key bitmap instead.
func (s *pressState) level(down bool, now time.Time) bool {
	if !down {
		s.down = time.Time{}
		s.fired = false
		return false
	}
	if s.down.IsZero() {
		s.down = now
	}
	return s.due(now)
}

func (s *pressState) due(now time.Time) bool {
	if s.fired || s.down.IsZero() || now.Sub(s.down) < btpairHold {
		return false
	}
	s.fired = true
	return true
}

func actionEventPath() string {
	events, _ := filepath.Glob("/sys/class/input/event*")
	for _, ev := range events {
		name, err := os.ReadFile(filepath.Join(ev, "device", "name"))
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(name)) == "aw933xx" {
			return "/dev/input/" + filepath.Base(ev)
		}
	}
	return ""
}

func watchBluetoothButton(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			log.Print("bluetooth button: ", r)
		}
	}()
	path := actionEventPath()
	if path == "" {
		log.Print("bluetooth button: touch pad not found")
		return
	}
	log.Print("bluetooth button: hold the action button for 3s (", path, ")")
	var state pressState
	busy := make(chan struct{}, 1)
	for ctx.Err() == nil {
		if e := readActionButton(ctx, path, &state, busy); e != nil && ctx.Err() == nil {
			log.Print("bluetooth button: ", e)
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
	}
}

// evIocGKey is EVIOCGKEY(size) on a 32-bit ARM process.
func evIocGKey(size int) uintptr {
	return uintptr(uint32(2)<<30 | uint32('E')<<8 | 0x18 | uint32(size)<<16)
}

func actionHeld(f *os.File) (bool, error) {
	var bits [96]byte
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), evIocGKey(len(bits)), uintptr(unsafe.Pointer(&bits[0])))
	if errno != 0 {
		return false, errno
	}
	return bits[keyAction/8]&(1<<(keyAction%8)) != 0, nil
}

func readActionButton(ctx context.Context, path string, state *pressState, busy chan struct{}) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	announced := false
	for ctx.Err() == nil {
		down, err := actionHeld(f)
		if err != nil {
			return err
		}
		if !announced {
			log.Print("bluetooth button: reading the key state directly")
			announced = true
		}
		if state.level(down, time.Now()) {
			requestBluetoothPairing(busy)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(100 * time.Millisecond):
		}
	}
	return nil
}

func requestBluetoothPairing(busy chan struct{}) {
	select {
	case busy <- struct{}{}:
		go func() {
			defer func() { <-busy }()
			toggleBluetoothDiscoverable()
		}()
	default:
	}
}

func toggleBluetoothDiscoverable() {
	show, err := exec.Command("/usr/bin/bluetoothctl", "show").Output()
	if err != nil {
		log.Print("bluetooth button: ", err)
		return
	}
	if strings.Contains(string(show), "Discoverable: yes") {
		if e := exec.Command("/usr/bin/bluetoothctl", "discoverable", "off").Run(); e != nil {
			log.Print("bluetooth button: ", e)
			return
		}
		log.Print("bluetooth button: discoverable off")
		playPairTone(false)
		return
	}
	if e := exec.Command("/usr/bin/bluetoothctl", "discoverable-timeout", btpairWindow).Run(); e != nil {
		log.Print("bluetooth button: ", e)
		return
	}
	if e := exec.Command("/usr/bin/bluetoothctl", "discoverable", "on").Run(); e != nil {
		log.Print("bluetooth button: ", e)
		return
	}
	log.Print("bluetooth button: discoverable for 5 min as Яндекс Лайт 2")
	playPairTone(true)
}

func playPairTone(open bool) {
	const rate = 48000
	freq := 523.0
	if open {
		freq = 880
	}
	samples := rate / 6
	buf := make([]byte, samples*4)
	for i := 0; i < samples; i++ {
		amp := 5000.0 * math.Sin(2*math.Pi*freq*float64(i)/rate)
		if open && i > samples/2 {
			amp = 5000.0 * math.Sin(2*math.Pi*1318*float64(i)/rate)
		}
		s := int16(amp)
		binary.LittleEndian.PutUint16(buf[i*4:], uint16(s))
		binary.LittleEndian.PutUint16(buf[i*4+2:], uint16(s))
	}
	cmd := exec.Command("/usr/bin/aplay", "-D", "speakers", "-q", "-f", "S16_LE", "-r", "48000", "-c", "2", "-t", "raw")
	cmd.Stdin = bytes.NewReader(buf)
	if e := cmd.Run(); e != nil {
		log.Print("bluetooth button tone: ", e)
	}
}
