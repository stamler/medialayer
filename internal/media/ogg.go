package media

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// FLAC in Ogg (.oga, and .ogg when it holds FLAC rather than Vorbis or
// Opus). An Ogg stream is a run of pages, each a 27-byte header, a table
// of segment lengths and the segments, which join into packets. For
// FLAC, the first packet holds a mapping header and STREAMINFO, the next
// packets the other metadata blocks, and each packet after them one FLAC
// frame; the audio starts on a new page.

// ErrNotOggFLAC is returned for a file that isn't Ogg with FLAC audio.
var ErrNotOggFLAC = errors.New("not an Ogg file with FLAC audio")

// oggMaxPage is the largest possible page.
const oggMaxPage = 27 + 255 + 255*255

// oggPage is one page, its CRC checked.
type oggPage struct {
	flags   byte // 1: continues a packet, 2: first page, 4: last page
	granule int64
	serial  uint32
	seq     uint32
	segs    []byte
	body    []byte
}

// parseOggPage reads a page from the start of b, which must hold it all,
// and returns its length.
func parseOggPage(b []byte) (*oggPage, int, error) {
	if len(b) < 27 || string(b[:4]) != "OggS" || b[4] != 0 {
		return nil, 0, fmt.Errorf("no Ogg page: %w", ErrNotOggFLAC)
	}
	n := 27 + int(b[26])
	if len(b) < n {
		return nil, 0, io.ErrUnexpectedEOF
	}
	segs := b[27:n]
	for _, l := range segs {
		n += int(l)
	}
	if len(b) < n {
		return nil, 0, io.ErrUnexpectedEOF
	}
	crc := oggCRC(0, b[:22])
	crc = oggCRC(crc, []byte{0, 0, 0, 0})
	crc = oggCRC(crc, b[26:n])
	if crc != binary.LittleEndian.Uint32(b[22:]) {
		return nil, 0, fmt.Errorf("Ogg page CRC mismatch: %w", ErrNotOggFLAC)
	}
	return &oggPage{flags: b[5], granule: int64(binary.LittleEndian.Uint64(b[6:])),
		serial: binary.LittleEndian.Uint32(b[14:]), seq: binary.LittleEndian.Uint32(b[18:]),
		segs: segs, body: b[27+len(segs) : n]}, n, nil
}

// readOggPage reads the page at r's position.
func readOggPage(r *bufio.Reader) (*oggPage, error) {
	h, err := r.Peek(27)
	if err != nil {
		if errors.Is(err, io.EOF) && len(h) == 0 {
			return nil, io.EOF
		}
		return nil, io.ErrUnexpectedEOF
	}
	n := 27 + int(h[26])
	b, err := r.Peek(n)
	if err != nil {
		return nil, io.ErrUnexpectedEOF
	}
	for _, l := range b[27:] {
		n += int(l)
	}
	if b, err = r.Peek(n); err != nil {
		return nil, io.ErrUnexpectedEOF
	}
	p, _, err := parseOggPage(b)
	if err != nil {
		return nil, err
	}
	// The page's slices point into r's buffer; copy what is kept.
	p.segs, p.body = bytes.Clone(p.segs), bytes.Clone(p.body)
	_, _ = r.Discard(n)
	return p, nil
}

// oggStream reads one logical stream's pages: its packets, or its bytes
// joined across packets, which for FLAC's audio is the stream of frames.
type oggStream struct {
	r      *bufio.Reader
	serial uint32
	seq    uint32
	seqOK  bool // seq holds the last page's number
	page   *oggPage
	seg    int // next segment of page
	off    int // where segment seg starts in page.body
	pos    int // the next byte, for ReadByte
	ended  bool
	// pastBOS is set once a page that starts no stream is read: a
	// stream's first page after that starts a chained stream.
	pastBOS bool
}

// next moves to the stream's next page, skipping other streams' pages.
func (s *oggStream) next() error {
	if s.ended {
		return io.EOF
	}
	for {
		p, err := readOggPage(s.r)
		if errors.Is(err, io.EOF) {
			s.ended = true
			return io.EOF // a stream cut short ends without its last page
		}
		if err != nil {
			return err
		}
		if p.flags&2 == 0 {
			s.pastBOS = true
		}
		if p.serial != s.serial {
			if p.flags&2 != 0 && s.pastBOS {
				s.ended = true // a chained stream starts: ours is over
				return io.EOF
			}
			continue
		}
		if s.seqOK && p.seq != s.seq+1 {
			return fmt.Errorf("Ogg page %d after %d: pages lost: %w", p.seq, s.seq, ErrNotOggFLAC)
		}
		s.seq, s.seqOK = p.seq, true
		s.page, s.seg, s.off, s.pos = p, 0, 0, 0
		if p.flags&4 != 0 {
			s.ended = true
		}
		return nil
	}
}

// errBigPacket is a packet over the size asked for: read, but not kept.
var errBigPacket = errors.New("Ogg packet too big")

// packet returns the next whole packet; one over max bytes is read to
// its end, and its first 4 bytes returned with errBigPacket.
func (s *oggStream) packet(max int) ([]byte, error) {
	var out []byte
	started, big := false, false
	for {
		if s.page == nil || s.seg == len(s.page.segs) {
			last := s.page
			if err := s.next(); err != nil {
				if errors.Is(err, io.EOF) && started {
					return nil, io.ErrUnexpectedEOF
				}
				return nil, err
			}
			if cont := s.page.flags&1 != 0; cont != started && last != nil {
				return nil, fmt.Errorf("Ogg packet continuation mismatch: %w", ErrNotOggFLAC)
			}
		}
		l := int(s.page.segs[s.seg])
		started = true
		if !big {
			out = append(out, s.page.body[s.off:s.off+l]...)
		}
		if len(out) > max && !big {
			out, big = out[:min(len(out), 4)], true
		}
		s.seg++
		s.off += l
		if l < 255 {
			s.pos = s.off
			if big {
				return out, errBigPacket
			}
			return out, nil
		}
	}
}

// ReadByte reads the stream's bytes, page after page.
func (s *oggStream) ReadByte() (byte, error) {
	for s.page == nil || s.pos == len(s.page.body) {
		if err := s.next(); err != nil {
			return 0, err
		}
	}
	c := s.page.body[s.pos]
	s.pos++
	return c, nil
}

// skipContinued drops the end of a packet the current page continues
// from the one before, so that reading starts at a packet: a frame.
// It reports false if no packet starts on the page.
func (s *oggStream) skipContinued() bool {
	if s.page.flags&1 == 0 {
		return true
	}
	for s.seg < len(s.page.segs) {
		l := int(s.page.segs[s.seg])
		s.seg++
		s.off += l
		if l < 255 {
			s.pos = s.off
			return s.pos < len(s.page.body)
		}
	}
	return false
}

// OggFLACHeader is what an Ogg FLAC stream's header packets hold.
type OggFLACHeader struct {
	Info   StreamInfo
	Serial uint32
	// Meta is the metadata as a native FLAC file starts: "fLaC" and the
	// metadata blocks, STREAMINFO first.
	Meta []byte
}

// readOggFLACHeader reads the header packets from the start of r and
// returns the stream positioned at the first audio page. Header packets
// over max bytes in all are an error.
func readOggFLACHeader(r *bufio.Reader, max int) (*OggFLACHeader, *oggStream, error) {
	// The first pages of an Ogg file start its streams: find FLAC's.
	var s *oggStream
	for s == nil {
		p, err := readOggPage(r)
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = ErrNotOggFLAC
			}
			return nil, nil, err
		}
		if p.flags&2 == 0 {
			return nil, nil, fmt.Errorf("no FLAC stream: %w (%w)", ErrNotOggFLAC, ErrUnsupported)
		}
		if bytes.HasPrefix(p.body, []byte("\x7fFLAC")) {
			s = &oggStream{r: r, serial: p.serial, seq: p.seq, seqOK: true, page: p}
		}
	}
	first, err := s.packet(1 << 10)
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, errBigPacket) {
			err = fmt.Errorf("no FLAC mapping header: %w", ErrNotOggFLAC)
		}
		return nil, nil, err
	}
	// 0x7F "FLAC", version 1.0, the header packet count, "fLaC", then
	// the STREAMINFO block.
	if len(first) != 51 || first[5] != 1 || string(first[9:13]) != "fLaC" || first[13]&0x7f != 0 ||
		first[14] != 0 || first[15] != 0 || first[16] != 34 {
		return nil, nil, fmt.Errorf("bad Ogg FLAC mapping header: %w", ErrNotOggFLAC)
	}
	h := &OggFLACHeader{Serial: s.serial}
	if h.Info, err = decodeStreamInfo(first[17:]); err != nil {
		return nil, nil, err
	}
	h.Meta = append([]byte("fLaC"), first[13:]...)
	// The mapping header counts the header packets after it, or is 0
	// when it doesn't; then the last metadata block's flag ends them.
	count := int(binary.BigEndian.Uint16(first[7:]))
	last := count == 0 && first[13]&0x80 != 0
	lastAt := 4
	blocks := []int{4} // where each block starts in Meta
	for n := 0; !last && (count == 0 || n < count); n++ {
		if n >= maxBlocks {
			return nil, nil, fmt.Errorf("more than %d FLAC metadata blocks: %w", maxBlocks, ErrNotOggFLAC)
		}
		p, err := s.packet(max - len(h.Meta))
		if errors.Is(err, errBigPacket) { // a cover over the header limit: left out, as for native FLAC
			last = count == 0 && len(p) > 0 && p[0]&0x80 != 0
			continue
		}
		if errors.Is(err, io.EOF) {
			err = fmt.Errorf("file ends in the header packets: %w", ErrNotOggFLAC)
		}
		if err != nil {
			return nil, nil, err
		}
		if len(p) < 4 || int(p[1])<<16|int(p[2])<<8|int(p[3]) != len(p)-4 || p[0]&0x7f == 0 || p[0]&0x7f == 127 {
			return nil, nil, fmt.Errorf("bad FLAC metadata packet: %w", ErrNotOggFLAC)
		}
		last = count == 0 && p[0]&0x80 != 0
		lastAt = len(h.Meta)
		blocks = append(blocks, lastAt)
		h.Meta = append(h.Meta, p...)
	}
	for _, i := range blocks { // the flag only on the last block
		h.Meta[i] &^= 0x80
	}
	h.Meta[lastAt] |= 0x80
	if s.page != nil && s.seg != len(s.page.segs) {
		return nil, nil, fmt.Errorf("audio on the last header page: %w", ErrNotOggFLAC)
	}
	return h, s, nil
}

// ParseOggFLAC reads an Ogg FLAC file's tags, cover art and stream
// details from its header packets, read through fetch. The length comes
// from STREAMINFO or else the last page's position.
func ParseOggFLAC(fetch FetchFunc, size, max int64) (*Info, error) {
	h, start, err := fetchOggFLACHeader(fetch, size, max)
	if err != nil {
		return nil, err
	}
	info, err := ParseFLAC(h.Meta)
	if err != nil {
		return nil, err
	}
	info.Format = "oga"
	if h.Info.TotalSamples == 0 {
		base := firstFrameNumber(fetch, start, h.Info, true, h.Serial)
		if n := oggLength(fetch, size, h.Serial, base); n > 0 {
			info.DurationMS = int64(n) * 1000 / int64(h.Info.SampleRate)
		}
	}
	return info, nil
}

// fetchOggFLACHeader reads an Ogg FLAC file's header packets through
// fetch, and returns where the audio pages start.
func fetchOggFLACHeader(fetch FetchFunc, size, max int64) (*OggFLACHeader, int64, error) {
	fr := &fetchReader{fetch: fetch, size: size}
	r := bufio.NewReaderSize(fr, oggMaxPage+1)
	h, _, err := readOggFLACHeader(r, int(max))
	if err != nil {
		return nil, 0, err
	}
	return h, fr.off - int64(len(fr.buf)) - int64(r.Buffered()), nil
}

// oggLength is the stream's length in samples from its last page's
// position, which counts from the first frame's number, base.
func oggLength(fetch FetchFunc, size int64, serial uint32, base uint64) uint64 {
	g := uint64(max(lastGranule(fetch, size, serial), 0))
	if g <= base {
		return 0
	}
	return g - base
}

// lastGranule finds the position of the stream's last page in the last
// 64 KB of the file: the samples to its end. It returns 0 if none.
func lastGranule(fetch FetchFunc, size int64, serial uint32) int64 {
	start := max(size-oggMaxPage, 0)
	b, err := fetch(start, size-start)
	if err != nil {
		return 0
	}
	for i := len(b) - 27; i >= 0; i-- {
		if b[i] != 'O' {
			continue
		}
		if p, _, err := parseOggPage(b[i:]); err == nil && p.serial == serial && p.granule > 0 {
			if p.granule >= 1<<36 { // more than STREAMINFO could count
				return 0
			}
			return p.granule
		}
	}
	return 0
}

// HashOggFLAC decodes the Ogg FLAC stream in r (a whole file) and returns
// its audio hash (see pcmHash): the same as a native FLAC file of the
// same audio, with the same checks.
func HashOggFLAC(r io.Reader) ([]byte, error) {
	h, s, err := readOggFLACHeader(bufio.NewReaderSize(r, oggMaxPage+1), 16<<20)
	if err != nil {
		return nil, err
	}
	return hashFLACFrames(newFLACFrameDecoder(h.Info, s))
}

// fetchReader reads a file in order through a FetchFunc, in growing
// pieces: few requests for a small header, and not many for a big one.
type fetchReader struct {
	fetch FetchFunc
	size  int64
	off   int64 // of buf's end
	n     int64 // the last piece's size
	buf   []byte
}

func (f *fetchReader) Read(p []byte) (int, error) {
	if len(f.buf) == 0 {
		if f.off >= f.size {
			return 0, io.EOF
		}
		f.n = min(max(f.n*2, 64<<10), 4<<20)
		b, err := f.fetch(f.off, min(f.n, f.size-f.off))
		if err != nil {
			return 0, err
		}
		if len(b) == 0 {
			return 0, io.ErrUnexpectedEOF
		}
		f.buf, f.off = b, f.off+int64(len(b))
	}
	n := copy(p, f.buf)
	f.buf = f.buf[n:]
	return n, nil
}

var oggCRCTable = func() (t [256]uint32) {
	for i := range t {
		c := uint32(i) << 24
		for range 8 {
			if c&0x80000000 != 0 {
				c = c<<1 ^ 0x04c11db7
			} else {
				c <<= 1
			}
		}
		t[i] = c
	}
	return
}()

func oggCRC(crc uint32, b []byte) uint32 {
	for _, c := range b {
		crc = crc<<8 ^ oggCRCTable[byte(crc>>24)^c]
	}
	return crc
}
