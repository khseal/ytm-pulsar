package main

import "testing"

func TestPlaybackSwitchReadsAmplifierState(t *testing.T) {
	on, ok := playbackSwitch("Mono: 511 [100%] [152.50dB] Playback [off]\n")
	if !ok || on {
		t.Fatalf("off channel parsed as on=%v ok=%v", on, ok)
	}
	on, ok = playbackSwitch("Mono: 511 [100%] [152.50dB] Playback [on]\n")
	if !ok || !on {
		t.Fatalf("on channel parsed as on=%v ok=%v", on, ok)
	}
	if _, ok := playbackSwitch("no switch here"); ok {
		t.Fatal("missing switch was accepted")
	}
}

func TestSetSpeakerSwitchesBothChannels(t *testing.T) {
	previous := speakerRun
	t.Cleanup(func() { speakerRun = previous })
	var calls [][]string
	speakerRun = func(args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		state := "off"
		if len(args) == 3 && args[2] == "on" {
			state = "on"
		}
		if len(args) > 0 && args[0] == "sget" {
			state = "on"
			for _, earlier := range calls[:len(calls)-1] {
				if len(earlier) == 3 && earlier[0] == "sset" && earlier[1] == args[1] {
					state = earlier[2]
				}
			}
		}
		return []byte("Playback [" + state + "]"), nil
	}
	if err := setSpeaker(false); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 4 {
		t.Fatalf("calls = %v", calls)
	}
	if calls[0][0] != "sset" || calls[0][1] != speakerChannels[0] || calls[0][2] != "off" {
		t.Fatalf("left off = %v", calls[0])
	}
	if calls[1][0] != "sset" || calls[1][1] != speakerChannels[1] || calls[1][2] != "off" {
		t.Fatalf("right off = %v", calls[1])
	}
	on, known := readSpeaker()
	if !known || on {
		t.Fatalf("after off: on=%v known=%v", on, known)
	}
}
