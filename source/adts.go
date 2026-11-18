package main

// Pulsar's playbin has a network queue of about two seconds, and qtdemux takes
// a whole fragment of YouTube's fragmented MP4 (several seconds of audio) out
// of it at once. The queue then reports buffering 0 %, and Pulsar pauses the
// pipeline until it refills: an audible hiccup at every fragment. The same AAC
// frames served as an ADTS stream flow evenly through aacparse.

import (
	"encoding/binary"
	"errors"
	"net/http"
	"strconv"
)

const (
	adtsProbe = iota
	adtsOn
	adtsPassthrough

	maxMoov      = 1 << 20
	maxADTSFrame = 8191 - 7
)

var errADTS = errors.New("unsupported MP4 for ADTS")

// adtsWriter turns an MP4 byte stream (init segment, then moof/mdat
// fragments) into ADTS frames. Until the init segment is understood it keeps
// the original bytes and, if it is not AAC in MP4, passes everything through.
type adtsWriter struct {
	http.ResponseWriter
	mime    string
	total   int64 // length of the MP4 stream, for the passthrough headers
	mode    int
	started bool
	raw     []byte

	header  [3]byte // ADTS profile/frequency/channel bits
	hdr     []byte  // an incomplete box header
	box     []byte  // a moov or moof being collected
	want    int     // full size of that box
	skip    int64   // bytes of an ignored box still to drop
	inMdat  bool
	mdat    int64 // payload bytes left; -1: up to the end of the stream
	moof    int64 // size of the last moof
	offset  int64 // trun data offset from the moof start; -1 if absent
	lead    int64 // mdat bytes before the first sample
	sizes   []uint32
	sample  int
	frame   []byte
	out     []byte
}

// relayADTS serves the track, or the seek window, as ADTS. ADTS frames carry
// no timestamps, so a window needs no tfdt shift.
func (s *Server) relayADTS(w http.ResponseWriter, r *http.Request, p *nativePlayer, src streamInfo, win *seekWindow) {
	a := &adtsWriter{ResponseWriter: w, mime: src.mime, total: src.length}
	if win == nil {
		s.relayAudioRange(a, r, p, src, 0, src.length-1, true)
	} else {
		// The shift matters only if the writer falls back to MP4.
		a.total = win.length(src)
		if _, err := a.Write(win.head); err != nil {
			return
		}
		t := &tfdtWriter{ResponseWriter: a, pos: win.fragStart, frags: win.frags, shift: win.startTicks}
		s.relayAudioRange(t, r, p, src, win.fragStart, src.length-1, true)
		t.flush()
	}
	if !a.started && r.Context().Err() == nil {
		w.WriteHeader(http.StatusBadGateway)
	}
}

func (a *adtsWriter) Write(p []byte) (int, error) {
	n := len(p)
	if a.mode == adtsPassthrough {
		return n, a.pass(p)
	}
	if a.mode == adtsProbe {
		a.raw = append(a.raw, p...)
	}
	err := a.feed(p)
	if err == nil && a.mode == adtsProbe && len(a.raw) > maxMoov+maxMoof {
		err = errADTS
	}
	if err != nil {
		if a.mode != adtsProbe {
			return 0, err
		}
		a.mode = adtsPassthrough
		raw := a.raw
		a.raw = nil
		return n, a.pass(raw)
	}
	if len(a.out) == 0 {
		return n, nil
	}
	if !a.started {
		h := a.ResponseWriter.Header()
		h.Set("Content-Type", "audio/aac")
		h.Set("Cache-Control", "no-store")
		a.ResponseWriter.WriteHeader(http.StatusOK)
		a.started = true
	}
	_, err = a.ResponseWriter.Write(a.out)
	a.out = a.out[:0]
	if err != nil {
		return 0, err
	}
	return n, nil
}

func (a *adtsWriter) pass(p []byte) error {
	if !a.started {
		h := a.ResponseWriter.Header()
		h.Set("Content-Type", a.mime)
		h.Set("Accept-Ranges", "bytes")
		h.Set("Content-Length", strconv.FormatInt(a.total, 10))
		h.Set("Cache-Control", "no-store")
		a.ResponseWriter.WriteHeader(http.StatusOK)
		a.started = true
	}
	_, err := a.ResponseWriter.Write(p)
	return err
}

func (a *adtsWriter) feed(p []byte) error {
	for len(p) > 0 {
		switch {
		case a.skip > 0:
			k := min(a.skip, int64(len(p)))
			a.skip -= k
			p = p[k:]
		case a.inMdat:
			k := int64(len(p))
			if a.mdat >= 0 {
				k = min(k, a.mdat)
				a.mdat -= k
				a.inMdat = a.mdat > 0
			}
			if err := a.samples(p[:k]); err != nil {
				return err
			}
			p = p[k:]
		case a.want > 0:
			k := min(a.want-len(a.box), len(p))
			a.box = append(a.box, p[:k]...)
			p = p[k:]
			if len(a.box) == a.want {
				a.want = 0
				if err := a.parseBox(a.box); err != nil {
					return err
				}
			}
		default:
			need := 8
			if len(a.hdr) >= 4 && binary.BigEndian.Uint32(a.hdr) == 1 {
				need = 16
			}
			k := min(need-len(a.hdr), len(p))
			a.hdr = append(a.hdr, p[:k]...)
			p = p[k:]
			if len(a.hdr) == 8 && binary.BigEndian.Uint32(a.hdr) == 1 {
				continue
			}
			if len(a.hdr) == need {
				if err := a.startBox(); err != nil {
					return err
				}
				a.hdr = a.hdr[:0]
			}
		}
	}
	return nil
}

func (a *adtsWriter) startBox() error {
	hlen := int64(len(a.hdr))
	size := int64(binary.BigEndian.Uint32(a.hdr))
	if size == 1 {
		size = int64(binary.BigEndian.Uint64(a.hdr[8:]))
	}
	kind := string(a.hdr[4:8])
	if a.mode == adtsProbe && kind != "ftyp" && kind != "moov" {
		return errADTS
	}
	if size == 0 && kind != "mdat" || size != 0 && size < hlen {
		return errADTS
	}
	switch kind {
	case "moov", "moof":
		limit := int64(maxMoov)
		if kind == "moof" {
			limit = maxMoof
		}
		if size > limit || hlen != 8 {
			return errADTS
		}
		a.want = int(size)
		a.box = append(a.box[:0], a.hdr...)
	case "mdat":
		if a.mode != adtsOn || a.sizes == nil {
			return errADTS
		}
		a.inMdat, a.mdat = true, -1
		if size != 0 {
			a.mdat = size - hlen
			a.inMdat = a.mdat > 0
		}
		a.lead = 0
		if a.offset >= 0 {
			a.lead = a.offset - a.moof - hlen
		}
		if a.lead < 0 {
			return errADTS
		}
		a.sample = 0
		a.frame = a.frame[:0]
	default:
		a.skip = size - hlen
	}
	return nil
}

func (a *adtsWriter) parseBox(b []byte) error {
	if string(b[4:8]) == "moov" {
		h, err := adtsHeaderBits(b)
		if err != nil {
			return err
		}
		a.header = h
		a.mode = adtsOn
		a.raw = nil
		return nil
	}
	if a.mode != adtsOn {
		return errADTS
	}
	sizes, offset, err := moofSamples(b)
	if err != nil {
		return err
	}
	a.sizes, a.offset, a.moof = sizes, offset, int64(len(b))
	return nil
}

// samples cuts mdat payload bytes into the AAC frames listed by the moof.
// Bytes past the last listed sample are dropped.
func (a *adtsWriter) samples(p []byte) error {
	for len(p) > 0 {
		if a.lead > 0 {
			k := min(a.lead, int64(len(p)))
			a.lead -= k
			p = p[k:]
			continue
		}
		if a.sample >= len(a.sizes) {
			return nil
		}
		size := int(a.sizes[a.sample])
		k := min(size-len(a.frame), len(p))
		a.frame = append(a.frame, p[:k]...)
		p = p[k:]
		if len(a.frame) < size {
			continue
		}
		total := size + 7
		a.out = append(a.out, 0xFF, 0xF1,
			a.header[0],
			a.header[1]|byte(total>>11),
			byte(total>>3),
			byte(total<<5)|0x1F,
			0xFC)
		a.out = append(a.out, a.frame...)
		a.frame = a.frame[:0]
		a.sample++
	}
	return nil
}

// adtsHeaderBits reads the AudioSpecificConfig of the first sample entry.
// Only plain AAC (Main, LC, SSR, LTP) fits an ADTS header without loss.
func adtsHeaderBits(moov []byte) ([3]byte, error) {
	var h [3]byte
	stsd := findBox(moov[8:], "trak", "mdia", "minf", "stbl", "stsd")
	if len(stsd) < 16 {
		return h, errADTS
	}
	entry := stsd[8:]
	size := int(binary.BigEndian.Uint32(entry))
	if string(entry[4:8]) != "mp4a" || size < 36 || size > len(entry) {
		return h, errADTS
	}
	esds := findBox(entry[36:size], "esds")
	if len(esds) < 4 {
		return h, errADTS
	}
	asc := decoderConfig(esds[4:])
	if len(asc) < 2 {
		return h, errADTS
	}
	object := asc[0] >> 3
	freq := (asc[0]&7)<<1 | asc[1]>>7
	channels := asc[1] >> 3 & 0xF
	if object < 1 || object > 4 || freq > 12 || channels < 1 || channels > 7 {
		return h, errADTS
	}
	h[0] = (object-1)<<6 | freq<<2 | channels>>2
	h[1] = channels & 3 << 6
	return h, nil
}

// findBox returns the payload of the box at path inside b.
func findBox(b []byte, path ...string) []byte {
	for _, name := range path {
		found := false
		for len(b) >= 8 {
			size := int(binary.BigEndian.Uint32(b))
			if size < 8 || size > len(b) {
				return nil
			}
			if string(b[4:8]) == name {
				b, found = b[8:size], true
				break
			}
			b = b[size:]
		}
		if !found {
			return nil
		}
	}
	return b
}

// descriptor splits an MPEG-4 descriptor (tag, expandable length, body).
func descriptor(b []byte) (tag byte, body, rest []byte, ok bool) {
	if len(b) < 2 {
		return 0, nil, nil, false
	}
	n := 0
	for i := 1; i < len(b) && i <= 4; i++ {
		n = n<<7 | int(b[i]&0x7F)
		if b[i]&0x80 == 0 {
			if n > len(b)-i-1 {
				return 0, nil, nil, false
			}
			return b[0], b[i+1 : i+1+n], b[i+1+n:], true
		}
	}
	return 0, nil, nil, false
}

// decoderConfig finds the DecoderSpecificInfo in an esds payload.
func decoderConfig(b []byte) []byte {
	tag, es, _, ok := descriptor(b)
	if !ok || tag != 0x03 || len(es) < 3 {
		return nil
	}
	flags := es[2]
	es = es[3:]
	if flags&0x80 != 0 {
		es = es[min(2, len(es)):]
	}
	if flags&0x40 != 0 && len(es) > 0 {
		es = es[min(1+int(es[0]), len(es)):]
	}
	if flags&0x20 != 0 {
		es = es[min(2, len(es)):]
	}
	for len(es) > 0 {
		tag, body, rest, ok := descriptor(es)
		if !ok {
			return nil
		}
		if tag == 0x04 && len(body) > 13 {
			for inner := body[13:]; len(inner) > 0; {
				tag, info, more, ok := descriptor(inner)
				if !ok {
					return nil
				}
				if tag == 0x05 {
					return info
				}
				inner = more
			}
			return nil
		}
		es = rest
	}
	return nil
}

// moofSamples lists the sample sizes of a single-track fragment and the trun
// data offset (from the moof start), or -1 when the samples follow the mdat
// header directly.
func moofSamples(moof []byte) ([]uint32, int64, error) {
	traf := findBox(moof[8:], "traf")
	tfhd := findBox(traf, "tfhd")
	trun := findBox(traf, "trun")
	if len(tfhd) < 8 || len(trun) < 8 {
		return nil, 0, errADTS
	}
	flags := binary.BigEndian.Uint32(tfhd) & 0xFFFFFF
	if flags&0x01 != 0 {
		return nil, 0, errADTS // explicit base offset
	}
	p := 8
	for _, f := range []uint32{0x02, 0x08} {
		if flags&f != 0 {
			p += 4
		}
	}
	var fallback uint32
	if flags&0x10 != 0 {
		if len(tfhd) < p+4 {
			return nil, 0, errADTS
		}
		fallback = binary.BigEndian.Uint32(tfhd[p:])
	}
	flags = binary.BigEndian.Uint32(trun) & 0xFFFFFF
	count := int(binary.BigEndian.Uint32(trun[4:]))
	p = 8
	offset := int64(-1)
	if flags&0x01 != 0 {
		if len(trun) < p+4 {
			return nil, 0, errADTS
		}
		offset = int64(int32(binary.BigEndian.Uint32(trun[p:])))
		p += 4
	}
	if flags&0x04 != 0 {
		p += 4
	}
	per := 0
	for _, f := range []uint32{0x100, 0x200, 0x400, 0x800} {
		if flags&f != 0 {
			per += 4
		}
	}
	if count <= 0 || per > 0 && count > (len(trun)-p)/per || per == 0 && count > 1<<16 {
		return nil, 0, errADTS
	}
	sizes := make([]uint32, count)
	for i := range sizes {
		size := fallback
		q := p
		if flags&0x100 != 0 {
			q += 4
		}
		if flags&0x200 != 0 {
			size = binary.BigEndian.Uint32(trun[q:])
		}
		if size == 0 || size > maxADTSFrame {
			return nil, 0, errADTS
		}
		sizes[i] = size
		p += per
	}
	return sizes, offset, nil
}
