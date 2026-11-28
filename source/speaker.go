package main

import (
	"errors"
	"log"
	"os/exec"
	"strings"
)

// Both halves of the FS2105 amplifier on Yandex Station Lite 2.
// Switching only the left channel leaves the right side audible.
// Other models do not have these controls, so the switch stays unavailable.
var speakerChannels = []string{
	"FS2105 Fs2105 AMP ChL",
	"FS2105 Fs2105 AMP ChR",
}

var speakerRun = func(args ...string) ([]byte, error) {
	return exec.Command("amixer", args...).CombinedOutput()
}

// readSpeaker reports whether any amplifier channel is still on.
// known is false when amixer did not answer for either channel.
func readSpeaker() (on bool, known bool) {
	seen := 0
	for _, name := range speakerChannels {
		out, err := speakerRun("sget", name)
		if err != nil && len(out) == 0 {
			continue
		}
		enabled, ok := playbackSwitch(string(out))
		if !ok {
			continue
		}
		seen++
		if enabled {
			on = true
		}
	}
	return on, seen > 0
}

func setSpeaker(on bool) error {
	value := "off"
	if on {
		value = "on"
	}
	for _, name := range speakerChannels {
		out, err := speakerRun("sset", name, value)
		if err != nil {
			logSpeaker(name, out, err)
			return errors.New("не удалось переключить динамик")
		}
	}
	got, known := readSpeaker()
	if !known || got != on {
		return errors.New("динамик не переключился")
	}
	return nil
}

func logSpeaker(name string, out []byte, err error) {
	msg := strings.TrimSpace(string(out))
	if msg == "" {
		msg = err.Error()
	}
	if len(msg) > 180 {
		msg = msg[:180]
	}
	log.Print("speaker ", name, ": ", msg)
}

// playbackSwitch reads the Playback [on]/[off] mark from amixer output.
func playbackSwitch(text string) (bool, bool) {
	const mark = "Playback ["
	idx := strings.LastIndex(text, mark)
	if idx < 0 {
		return false, false
	}
	rest := text[idx+len(mark):]
	switch {
	case strings.HasPrefix(rest, "on]"):
		return true, true
	case strings.HasPrefix(rest, "off]"):
		return false, true
	default:
		return false, false
	}
}
