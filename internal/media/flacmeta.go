package media

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

// FLAC metadata block types.
const (
	flacStreamInfo    = 0
	flacVorbisComment = 4
	flacPicture       = 6
)

// ParseFLAC reads STREAMINFO, the Vorbis comments and the pictures from the
// bytes FLACHeader returned. A truncated header (complete = false) is
// parsed as far as it goes: blocks cut off at the end are skipped.
func ParseFLAC(header []byte) (*Info, error) {
	pos := 0
	if len(header) >= 10 && bytes.Equal(header[:3], []byte("ID3")) {
		pos = 10 + syncsafe(header[6:10])
		if header[5]&0x10 != 0 {
			pos += 10
		}
	}
	if len(header) < pos+4 || !bytes.Equal(header[pos:pos+4], []byte("fLaC")) {
		return nil, ErrNotFLAC
	}
	pos += 4

	info := &Info{Format: "flac"}
	seenStreamInfo := false
	for pos+4 <= len(header) {
		last := header[pos]&0x80 != 0
		typ := header[pos] & 0x7f
		length := int(header[pos+1])<<16 | int(header[pos+2])<<8 | int(header[pos+3])
		pos += 4
		if pos+length > len(header) {
			break // truncated header: the rest is not here
		}
		body := header[pos : pos+length]
		pos += length
		switch typ {
		case flacStreamInfo:
			if err := parseStreamInfo(body, info); err != nil {
				return nil, err
			}
			seenStreamInfo = true
		case flacVorbisComment:
			tags, err := parseVorbisComment(body)
			if err != nil {
				return nil, err
			}
			info.Tags = append(info.Tags, tags...)
		case flacPicture:
			pic, err := parseFLACPicture(body)
			if err != nil {
				return nil, err
			}
			info.Pictures = append(info.Pictures, pic)
		}
		if last {
			break
		}
	}
	if !seenStreamInfo {
		return nil, fmt.Errorf("no STREAMINFO block: %w", ErrNotFLAC)
	}
	return info, nil
}

func parseStreamInfo(b []byte, info *Info) error {
	if len(b) < 18 {
		return fmt.Errorf("STREAMINFO is %d bytes, want 34: %w", len(b), ErrNotFLAC)
	}
	// Bytes 10..17: 20 bits sample rate, 3 bits channels-1, 5 bits
	// bits-per-sample-1, 36 bits total samples.
	v := binary.BigEndian.Uint64(b[10:18])
	rate := int(v >> 44)
	info.SampleRate = rate
	info.Channels = int(v>>41&0x7) + 1
	info.BitDepth = int(v>>36&0x1f) + 1
	samples := int64(v & (1<<36 - 1))
	if rate > 0 && samples > 0 {
		info.DurationMS = samples * 1000 / int64(rate)
	}
	return nil
}

var errBadVorbis = errors.New("malformed Vorbis comment block")

func parseVorbisComment(b []byte) ([]Tag, error) {
	r := leReader{b: b}
	vendorLen, ok := r.u32()
	if !ok || !r.skip(int(vendorLen)) {
		return nil, errBadVorbis
	}
	count, ok := r.u32()
	if !ok {
		return nil, errBadVorbis
	}
	var tags []Tag
	for range count {
		n, ok := r.u32()
		if !ok {
			return nil, errBadVorbis
		}
		s, ok := r.bytes(int(n))
		if !ok {
			return nil, errBadVorbis
		}
		key, value, found := strings.Cut(string(s), "=")
		if !found || key == "" {
			continue // not a comment we can use; skip it like other readers do
		}
		tags = append(tags, Tag{Key: strings.ToUpper(key), Value: value})
	}
	return tags, nil
}

var errBadPicture = errors.New("malformed FLAC PICTURE block")

func parseFLACPicture(b []byte) (Picture, error) {
	r := beReader{leReader{b: b}}
	var p Picture
	typ, ok1 := r.u32()
	mimeLen, ok2 := r.u32()
	if !ok1 || !ok2 {
		return p, errBadPicture
	}
	mime, ok := r.bytes(int(mimeLen))
	if !ok {
		return p, errBadPicture
	}
	descLen, ok := r.u32()
	if !ok || !r.skip(int(descLen)) {
		return p, errBadPicture
	}
	w, ok1 := r.u32()
	h, ok2 := r.u32()
	_, ok3 := r.u32() // colour depth
	_, ok4 := r.u32() // palette size
	n, ok5 := r.u32()
	if !(ok1 && ok2 && ok3 && ok4 && ok5) {
		return p, errBadPicture
	}
	data, ok := r.bytes(int(n))
	if !ok {
		return p, errBadPicture
	}
	p.Type, p.MIME, p.Width, p.Height, p.Data = int(typ), string(mime), int(w), int(h), data
	return p, nil
}

// leReader and beReader read little- and big-endian fields from a byte
// slice, reporting false instead of panicking when it runs out.
type leReader struct {
	b   []byte
	pos int
}

func (r *leReader) u32() (uint32, bool) {
	if r.pos+4 > len(r.b) {
		return 0, false
	}
	v := binary.LittleEndian.Uint32(r.b[r.pos:])
	r.pos += 4
	return v, true
}

func (r *leReader) skip(n int) bool {
	if n < 0 || r.pos+n > len(r.b) {
		return false
	}
	r.pos += n
	return true
}

func (r *leReader) bytes(n int) ([]byte, bool) {
	start := r.pos
	if !r.skip(n) {
		return nil, false
	}
	return r.b[start:r.pos], true
}

type beReader struct{ leReader }

func (r *beReader) u32() (uint32, bool) {
	if r.pos+4 > len(r.b) {
		return 0, false
	}
	v := binary.BigEndian.Uint32(r.b[r.pos:])
	r.pos += 4
	return v, true
}
