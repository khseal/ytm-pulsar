package main

import (
	"bytes"
	"encoding/binary"
	"net/http/httptest"
	"testing"
)

func mp4Box(kind string, parts ...[]byte) []byte {
	body := bytes.Join(parts, nil)
	b := binary.BigEndian.AppendUint32(nil, uint32(8+len(body)))
	return append(append(b, kind...), body...)
}

func u32s(v ...uint32) []byte {
	var b []byte
	for _, x := range v {
		b = binary.BigEndian.AppendUint32(b, x)
	}
	return b
}

// testAACInit is ftyp+moov of a 44.1 kHz stereo AAC-LC track.
func testAACInit() []byte {
	asc := []byte{0x12, 0x10} // object 2 (LC), frequency index 4, 2 channels
	dsi := append([]byte{0x05, byte(len(asc))}, asc...)
	dcd := append([]byte{0x04, byte(13 + len(dsi)), 0x40, 0x15}, make([]byte, 11)...)
	dcd = append(dcd, dsi...)
	es := append([]byte{0x03, byte(3 + len(dcd)), 0, 1, 0}, dcd...)
	esds := mp4Box("esds", make([]byte, 4), es)
	mp4a := mp4Box("mp4a", make([]byte, 28), esds)
	stsd := mp4Box("stsd", u32s(0, 1), mp4a)
	trak := mp4Box("trak", mp4Box("mdia", mp4Box("minf", mp4Box("stbl", stsd))))
	return append(mp4Box("ftyp", []byte("dash")), mp4Box("moov", trak)...)
}

// testFragment is a moof listing frames and the mdat holding them.
func testFragment(frames ...[]byte) []byte {
	sizes := []uint32{}
	for _, f := range frames {
		sizes = append(sizes, uint32(len(f)))
	}
	build := func(offset uint32) []byte {
		trun := u32s(0x201, uint32(len(frames)), offset)
		trun = append(trun, u32s(sizes...)...)
		tfhd := u32s(0x020000, 1)
		return mp4Box("moof", mp4Box("traf", mp4Box("tfhd", tfhd), mp4Box("trun", trun)))
	}
	moof := build(0)
	moof = build(uint32(len(moof) + 8))
	return append(moof, mp4Box("mdat", bytes.Join(frames, nil))...)
}

func TestADTSWriterRepacksFragmentedAAC(t *testing.T) {
	frames := [][]byte{bytes.Repeat([]byte{1}, 300), bytes.Repeat([]byte{2}, 17), bytes.Repeat([]byte{3}, 900)}
	stream := testAACInit()
	stream = append(stream, mp4Box("sidx", make([]byte, 24))...)
	stream = append(stream, testFragment(frames[0], frames[1])...)
	stream = append(stream, testFragment(frames[2])...)

	rec := httptest.NewRecorder()
	a := &adtsWriter{ResponseWriter: rec, mime: "audio/mp4", total: int64(len(stream))}
	for rest := stream; len(rest) > 0; {
		k := min(7, len(rest))
		if _, err := a.Write(rest[:k]); err != nil {
			t.Fatal(err)
		}
		rest = rest[k:]
	}
	if got := rec.Header().Get("Content-Type"); got != "audio/aac" {
		t.Fatalf("Content-Type = %q", got)
	}
	out := rec.Body.Bytes()
	for i, f := range frames {
		if len(out) < 7+len(f) {
			t.Fatalf("frame %d: short output", i)
		}
		h := out[:7]
		if h[0] != 0xFF || h[1] != 0xF1 {
			t.Fatalf("frame %d: no ADTS sync: % x", i, h)
		}
		if profile, freq, ch := h[2]>>6, h[2]>>2&0xF, (h[2]&1)<<2|h[3]>>6; profile != 1 || freq != 4 || ch != 2 {
			t.Fatalf("frame %d: profile %d freq %d channels %d", i, profile, freq, ch)
		}
		if size := int(h[3]&3)<<11 | int(h[4])<<3 | int(h[5]>>5); size != 7+len(f) {
			t.Fatalf("frame %d: length %d, want %d", i, size, 7+len(f))
		}
		if !bytes.Equal(out[7:7+len(f)], f) {
			t.Fatalf("frame %d: payload differs", i)
		}
		out = out[7+len(f):]
	}
	if len(out) != 0 {
		t.Fatalf("%d extra bytes", len(out))
	}
}

func TestADTSWriterPassesOtherDataThrough(t *testing.T) {
	data := bytes.Repeat([]byte("not mp4 "), 100)
	rec := httptest.NewRecorder()
	a := &adtsWriter{ResponseWriter: rec, mime: "audio/mp4", total: int64(len(data))}
	for _, part := range [][]byte{data[:5], data[5:]} {
		if _, err := a.Write(part); err != nil {
			t.Fatal(err)
		}
	}
	if got := rec.Header().Get("Content-Type"); got != "audio/mp4" {
		t.Fatalf("Content-Type = %q", got)
	}
	if !bytes.Equal(rec.Body.Bytes(), data) {
		t.Fatal("passthrough changed the data")
	}
}
