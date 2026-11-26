package media

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
)

// Decoding a FLAC stream, native or in Ogg, from any point, for players
// that can't play it themselves (FLAC in Ogg, and 32-bit FLAC, on Apple's
// platforms): the stream is served to them decoded, and a seek finds the
// frame that holds a sample by bisecting the file on the sample numbers
// that frame headers carry.

// FLACSource is a FLAC stream that can be decoded from any sample.
type FLACSource struct {
	Info  StreamInfo
	Total uint64 // samples in the stream
	ogg   bool
	// serial is the Ogg stream's.
	serial uint32
	// audioStart is where the first frame (or audio page) starts.
	audioStart, size int64
}

// ErrNoFrame is returned when no frame starts in the part of the file
// searched.
var ErrNoFrame = errors.New("no FLAC frame found")

// OpenFLACSource reads a FLAC file's header through fetch: a native FLAC
// file, or FLAC in Ogg if ogg is set. Its length must be known, from
// STREAMINFO or (in Ogg) the last page.
func OpenFLACSource(fetch FetchFunc, size, maxHeader int64, ogg bool) (*FLACSource, error) {
	s := &FLACSource{ogg: ogg, size: size}
	if ogg {
		fr := &fetchReader{fetch: fetch, size: size}
		r := bufio.NewReaderSize(fr, oggMaxPage+1)
		h, _, err := readOggFLACHeader(r, int(maxHeader))
		if err != nil {
			return nil, err
		}
		s.Info, s.serial = h.Info, h.Serial
		s.audioStart = fr.off - int64(len(fr.buf)) - int64(r.Buffered())
		s.Total = h.Info.TotalSamples
		if s.Total == 0 {
			s.Total = uint64(max(lastGranule(fetch, size, h.Serial), 0))
		}
	} else {
		header, complete, err := FLACHeader(fetch, size, maxHeader)
		if err != nil {
			return nil, err
		}
		if !complete {
			return nil, fmt.Errorf("FLAC metadata over %d bytes: %w", maxHeader, ErrNotFLAC)
		}
		d, err := NewFLACDecoder(bytes.NewReader(header))
		if err != nil {
			return nil, err
		}
		s.Info, s.Total, s.audioStart = d.Info, d.Info.TotalSamples, int64(len(header))
	}
	if s.Total == 0 {
		return nil, fmt.Errorf("a FLAC stream of unknown length: %w", ErrNotFLAC)
	}
	return s, nil
}

// FLACFrames decodes frames from a point in a FLACSource.
type FLACFrames struct {
	d    *FLACDecoder
	rc   io.ReadCloser
	next uint64 // the next frame's first sample
	seen bool
}

// Next decodes the next frame and returns its first sample's number and
// its length; Samples holds it.
func (f *FLACFrames) Next() (first uint64, n int, err error) {
	n, err = f.d.Next()
	if err != nil {
		return 0, 0, err
	}
	if !f.seen { // later frames follow on, whatever their headers say
		f.next, f.seen = f.d.First, true
	}
	first = f.next
	f.next += uint64(n)
	return first, n, nil
}

// Samples is the last frame's samples, by channel.
func (f *FLACFrames) Samples() [][]int64 { return f.d.Samples }

func (f *FLACFrames) Close() error { return f.rc.Close() }

// framesAt opens the file at off for length bytes and finds the first
// frame that starts there or after.
func (s *FLACSource) framesAt(open OpenFunc, off, length int64) (*FLACFrames, error) {
	rc, err := open(off, length)
	if err != nil {
		return nil, err
	}
	br := bufio.NewReaderSize(rc, oggMaxPage+1)
	var d *FLACDecoder
	if s.ogg {
		d, err = s.oggFrames(br)
	} else {
		d, err = s.nativeFrames(br)
	}
	if err != nil {
		_ = rc.Close()
		return nil, err
	}
	return &FLACFrames{d: d, rc: rc}, nil
}

// nativeFrames finds a frame in a native FLAC stream: a sync code and a
// header whose CRC-8 checks and that fits the stream.
func (s *FLACSource) nativeFrames(br *bufio.Reader) (*FLACDecoder, error) {
	for {
		c, err := br.ReadByte()
		if err != nil {
			return nil, ErrNoFrame
		}
		if c != 0xff {
			continue
		}
		_ = br.UnreadByte() // right after ReadByte, so it can't fail
		p, _ := br.Peek(16)
		probe := &FLACDecoder{Info: s.Info, br: newBitReader(bytes.NewReader(p))}
		if probe.checkHeader() == nil {
			return newFLACFrameDecoder(s.Info, br), nil
		}
		_, _ = br.Discard(1)
	}
}

// checkHeader reads a frame's sync code and header.
func (d *FLACDecoder) checkHeader() error {
	br := d.br
	br.crc, br.crc8, br.crc16 = true, 0, 0
	if v, err := br.bits(15); err != nil || v != 0x7ffc {
		return ErrNoFrame
	}
	_, _, _, err := d.header()
	return err
}

// oggFrames finds a page of the FLAC stream whose CRC checks, and the
// first packet, a frame, that starts on it or after.
func (s *FLACSource) oggFrames(br *bufio.Reader) (*FLACDecoder, error) {
	for {
		c, err := br.ReadByte()
		if err != nil {
			return nil, ErrNoFrame
		}
		if c != 'O' {
			continue
		}
		_ = br.UnreadByte()
		h, _ := br.Peek(27)
		if len(h) < 27 || string(h[:4]) != "OggS" {
			_, _ = br.Discard(1)
			continue
		}
		n := 27 + int(h[26])
		b, _ := br.Peek(n)
		if len(b) == n {
			for _, l := range b[27:] {
				n += int(l)
			}
			b, _ = br.Peek(n)
		}
		p, _, err := parseOggPage(b)
		if err != nil || p.serial != s.serial {
			_, _ = br.Discard(1)
			continue
		}
		p.segs, p.body = bytes.Clone(p.segs), bytes.Clone(p.body)
		_, _ = br.Discard(n)
		st := &oggStream{r: br, serial: s.serial, seq: p.seq, seqOK: true, page: p, ended: p.flags&4 != 0}
		for tries := 0; !st.skipContinued(); tries++ {
			if tries == 4 { // packets this long aren't frames
				return nil, ErrNoFrame
			}
			if err := st.next(); err != nil {
				return nil, ErrNoFrame
			}
		}
		return newFLACFrameDecoder(s.Info, st), nil
	}
}

// seekSpan is how close Seek's bisection comes before decoding on.
var seekSpan int64 = 256 << 10

// probeLen is how far past a probe point a frame is looked for. The
// read stops as soon as one is found, so it costs little to be long.
const probeLen = 4 << 20

// Seek returns frames starting at or before the frame that holds sample,
// read through open to the end of the file.
func (s *FLACSource) Seek(open OpenFunc, sample uint64) (*FLACFrames, error) {
	lo, hi := s.audioStart, s.size
	loS, hiS := uint64(0), s.Total
	for probes := 0; sample > 0 && hi-lo > seekSpan && probes < 32; probes++ {
		// Guess by the bit rate between the bounds, kept off the ends
		// so the bracket shrinks however wrong the guess is.
		frac := 0.5
		if hiS > loS {
			frac = float64(sample-loS) / float64(hiS-loS)
		}
		frac = min(max(frac, 0.1), 0.9)
		mid := lo + int64(float64(hi-lo)*frac)
		first, ok := s.probe(open, mid)
		switch {
		case !ok || first > sample:
			hi, hiS = mid, max(first, loS)
		case first+uint64(s.Info.MaxBlock) > sample:
			lo, loS = mid, first // the frame holds it, or the next
			hi = lo
		default:
			lo, loS = mid, first
		}
	}
	return s.framesAt(open, lo, s.size-lo)
}

// probe returns the first sample of the first frame at or after off.
func (s *FLACSource) probe(open OpenFunc, off int64) (uint64, bool) {
	f, err := s.framesAt(open, off, min(probeLen, s.size-off))
	if err != nil {
		return 0, false
	}
	defer func() { _ = f.Close() }()
	first, _, err := f.Next()
	return first, err == nil
}

// PCMLayout is how decoded FLAC is laid out as WAVE PCM: whole bytes of
// at least 16 bits (8-bit WAVE is unsigned), samples shifted up to fill
// them.
func (s *FLACSource) PCMLayout() (width int, shift uint) {
	width = max(2, (int(s.Info.BitsPerSample)+7)/8)
	return width, uint(width*8) - uint(s.Info.BitsPerSample)
}

// AppendPCM appends samples [from, to) of each channel, interleaved,
// little-endian, width bytes each, shifted up by shift bits.
func AppendPCM(dst []byte, chans [][]int64, from, to, width int, shift uint) []byte {
	for i := from; i < to; i++ {
		for _, c := range chans {
			v := c[i] << shift
			for b := range width {
				dst = append(dst, byte(v>>(8*b)))
			}
		}
	}
	return dst
}
