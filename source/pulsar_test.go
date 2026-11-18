package main

import (
	"bufio"
	"bytes"
	"testing"
)

func TestEsphomeMediaURL(t *testing.T) {
	body := esphomeMediaURL("http://127.0.0.1:18883/audio/ab")
	key, ok := protobufUint(body, 1)
	if !ok || key != 1 {
		t.Fatalf("key = %d ok=%v", key, ok)
	}
	if _, ok := protobufUint(body, 6); !ok {
		t.Fatal("has_media_url missing")
	}
	frame := appendPlain(esphomeMediaCmd, body)
	msgType, msg, err := readPlain(bufio.NewReader(bytes.NewReader(frame)))
	if err != nil {
		t.Fatal(err)
	}
	if msgType != esphomeMediaCmd || !bytes.Equal(msg, body) {
		t.Fatalf("frame type %d len %d", msgType, len(msg))
	}
}

func TestMediaStateField(t *testing.T) {
	// key fixed32=1, state varint=2 (playing), volume omitted, muted false omitted.
	msg := appendFixed32(nil, 1, 1)
	msg = appendVarintField(msg, 2, mediaStatePlaying)
	st, ok := protobufUint(msg, 2)
	if !ok || st != mediaStatePlaying {
		t.Fatalf("state %d ok=%v", st, ok)
	}
}
