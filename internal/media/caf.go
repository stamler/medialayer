package media

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"
)

// CAF files (.caf, Apple's Core Audio Format) holding ALAC: their tags and
// stream details for the index, and their audio for the hash. A CAF file
// is a "caff" header followed by chunks, each a type, a 64-bit size and a
// body: "desc" describes the stream, "kuki" holds ALAC's configuration,
// "info" the tags, "pakt" each packet's size and "data" the packets.

// ErrNotCAF is returned for a file that isn't a CAF holding ALAC.
var ErrNotCAF = errors.New("not a CAF file with ALAC audio")

// cafHead is how much of a CAF file is read at once to find its chunks;
// everything but the audio and its packet table is usually in it.
const cafHead = 64 << 10

// cafFarReads bounds the reads past the head that finding the chunks
// takes, against crafted files with many small chunks.
const cafFarReads = 16

// cafFile is what a CAF file's chunks say about its audio.
type cafFile struct {
	alac        ALACConfig
	validFrames int64
	info        []byte // the "info" chunk's body, nil if none or too big
	pakt        []byte
	data        int64 // where the packets start
	dataEnd     int64
}

// readCAF finds a CAF file's chunks through fetch and reads all but the
// audio, and the tags if withInfo is set.
func readCAF(fetch FetchFunc, size int64, withInfo bool) (*cafFile, error) {
	head, err := fetch(0, min(size, cafHead))
	if err != nil {
		return nil, err
	}
	if len(head) < 8 || string(head[:4]) != "caff" || binary.BigEndian.Uint16(head[4:]) != 1 {
		return nil, ErrNotCAF
	}
	// at reads n bytes at off, from the head where it holds them.
	far := 0
	at := func(off, n int64) ([]byte, error) {
		if off+n <= int64(len(head)) {
			return head[off : off+n], nil
		}
		if far++; far > cafFarReads {
			return nil, fmt.Errorf("chunks spread too far: %w", ErrNotCAF)
		}
		b, err := fetch(off, n)
		if err != nil {
			return nil, err
		}
		if int64(len(b)) < n {
			return nil, fmt.Errorf("short read at %d: %w", off, ErrNotCAF)
		}
		return b, nil
	}
	f := &cafFile{data: -1}
	var desc, kuki []byte
	pos := int64(8)
	for chunks := 0; pos+12 <= size; chunks++ {
		if chunks > 1000 {
			return nil, fmt.Errorf("too many chunks: %w", ErrNotCAF)
		}
		if desc != nil && kuki != nil && f.pakt != nil && f.data >= 0 &&
			(!withInfo || f.info != nil || pos+12 > int64(len(head))) {
			break // all that's needed, but tags in the head
		}
		h, err := at(pos, 12)
		if err != nil {
			return nil, err
		}
		typ, n := string(h[:4]), int64(binary.BigEndian.Uint64(h[4:]))
		body := pos + 12
		if n == -1 && typ == "data" { // runs to the end of the file
			n = size - body
		}
		if n < 0 || n > size-body {
			return nil, fmt.Errorf("chunk %q runs past the end of the file: %w", typ, ErrNotCAF)
		}
		read := func(limit int64) ([]byte, error) {
			if n > limit {
				return nil, fmt.Errorf("%q chunk of %d bytes: %w", typ, n, ErrNotCAF)
			}
			return at(body, n)
		}
		switch typ {
		case "desc":
			if desc, err = read(1 << 10); err == nil && len(desc) >= 32 && string(desc[8:12]) != "alac" {
				return nil, fmt.Errorf("audio codec %q: %w (%w)", desc[8:12], ErrNotCAF, ErrUnsupported)
			}
		case "kuki":
			kuki, err = read(1 << 10)
		case "info":
			if withInfo && n <= 1<<20 { // more is no tags MediaLayer keeps
				f.info, err = read(1 << 20)
			}
		case "pakt":
			f.pakt, err = read(maxMoov)
		case "data":
			if n < 4 {
				return nil, fmt.Errorf("short data chunk: %w", ErrNotCAF)
			}
			f.data, f.dataEnd = body+4, body+n // after the edit count
		}
		if err != nil {
			return nil, err
		}
		pos = body + n
	}
	if len(desc) < 32 {
		return nil, fmt.Errorf("no stream description: %w", ErrNotCAF)
	}
	if id := string(desc[8:12]); id != "alac" {
		return nil, fmt.Errorf("audio codec %q: %w (%w)", id, ErrNotCAF, ErrUnsupported)
	}
	if f.alac, err = parseALACConfig(kuki); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotCAF, err)
	}
	if rate := math.Float64frombits(binary.BigEndian.Uint64(desc)); rate != float64(f.alac.SampleRate) {
		return nil, fmt.Errorf("stream description says %v Hz, ALAC's configuration %d: %w", rate, f.alac.SampleRate, ErrNotCAF)
	}
	if fpp := binary.BigEndian.Uint32(desc[20:]); fpp != f.alac.FrameLength {
		return nil, fmt.Errorf("%d frames a packet, ALAC's configuration says %d: %w", fpp, f.alac.FrameLength, ErrNotCAF)
	}
	if len(f.pakt) >= 24 {
		f.validFrames = int64(binary.BigEndian.Uint64(f.pakt[8:]))
	}
	return f, nil
}

// duration works out the stream's length in samples. ffmpeg counts every
// packet whole in the packet table, so the last packet's own header,
// which says when it holds fewer samples, is read for it.
func (f *cafFile) duration(fetch FetchFunc) int64 {
	samples, err := f.packets()
	if err != nil || len(samples) == 0 {
		return 0
	}
	full := int64(len(samples)) * int64(f.alac.FrameLength)
	n := full
	last := samples[len(samples)-1]
	if b, err := fetch(last.Offset, 7); err == nil && len(b) == 7 {
		// Element tag (3 bits), 4 + 12 unused, has-size, 2 bits of extra
		// bytes, not-compressed, then the sample count if has-size.
		h := binary.BigEndian.Uint64(append([]byte{0}, b...))
		if h>>(56-20)&1 != 0 {
			if k := int64(h >> 1 & 0xffffffff); k <= int64(f.alac.FrameLength) {
				n = full - int64(f.alac.FrameLength) + k
			}
		}
	}
	if f.validFrames > 0 && f.validFrames < n { // Apple's, exact
		n = f.validFrames
	}
	return n
}

// packets reads the packet table: each packet's size, as a variable-
// length integer (7 bits a byte, most significant first). ALAC packets
// all hold the frame length's samples, so the table doesn't list them.
func (f *cafFile) packets() ([]MP4Sample, error) {
	if f.data < 0 {
		return nil, fmt.Errorf("no audio: %w", ErrNotCAF)
	}
	if len(f.pakt) < 24 {
		return nil, fmt.Errorf("no packet table: %w", ErrNotCAF)
	}
	count := binary.BigEndian.Uint64(f.pakt)
	if count > maxPackets || count > uint64(len(f.pakt)) {
		return nil, fmt.Errorf("%d packets: %w", count, ErrNotCAF)
	}
	b := f.pakt[24:]
	out := make([]MP4Sample, 0, count)
	off := f.data
	for range count {
		var v uint64
		for i := 0; ; i++ {
			if len(b) == 0 || i == 5 {
				return nil, fmt.Errorf("bad packet table: %w", ErrNotCAF)
			}
			c := b[0]
			b = b[1:]
			v = v<<7 | uint64(c&0x7f)
			if c&0x80 == 0 {
				break
			}
		}
		if v == 0 || v > 1<<24 || off+int64(v) > f.dataEnd {
			return nil, fmt.Errorf("packet of %d bytes past the audio: %w", v, ErrNotCAF)
		}
		out = append(out, MP4Sample{Offset: off, Size: uint32(v)})
		off += int64(v)
	}
	return out, nil
}

// ParseCAF reads a CAF file's tags and stream details.
func ParseCAF(fetch FetchFunc, size int64) (*Info, error) {
	f, err := readCAF(fetch, size, true)
	if err != nil {
		return nil, err
	}
	c := f.alac
	info := &Info{Format: "caf", SampleRate: int(c.SampleRate), Channels: int(c.Channels), BitDepth: int(c.BitDepth)}
	info.DurationMS = f.duration(fetch) * 1000 / int64(c.SampleRate)
	parseCAFInfo(f.info, info)
	return info, nil
}

// cafTags maps CAF info keys, as Apple and ffmpeg write them, to the
// index's tag names. Other keys keep their own names, upper-cased.
var cafTags = map[string]string{
	"track number": "TRACKNUMBER", "track": "TRACKNUMBER", "year": "DATE",
	"comments": "COMMENT", "album artist": "ALBUMARTIST",
	"album_artist": "ALBUMARTIST", "disc": "DISCNUMBER", "disc number": "DISCNUMBER",
	"encoding application": "ENCODER", "encoder": "ENCODER", "sub-title": "SUBTITLE",
	"tempo": "BPM", "key signature": "KEY", "encoded_by": "ENCODEDBY",
}

// cafMaxTags bounds the tags read from one file.
const cafMaxTags = 1024

// cafSkip are info keys that describe the stream, not the music.
var cafSkip = map[string]bool{
	"approximate duration in seconds": true, "channel layout": true,
	"nominal bit rate": true, "source bit depth": true, "source encoder": true,
}

// parseCAFInfo reads an "info" chunk: an entry count, then each entry's
// key and value as NUL-terminated UTF-8 strings. Apple's "recorded date"
// is the date only when there is no "year".
func parseCAFInfo(b []byte, info *Info) {
	if len(b) < 4 {
		return
	}
	n := min(binary.BigEndian.Uint32(b), cafMaxTags)
	b = b[4:]
	str := func() (string, bool) {
		i := bytes.IndexByte(b, 0)
		if i < 0 {
			return "", false
		}
		s := string(b[:i])
		b = b[i+1:]
		return strings.TrimSpace(s), true
	}
	var recorded string
	hasDate := false
	for range n {
		k, ok := str()
		v, ok2 := str()
		if !ok || !ok2 {
			break
		}
		k = strings.ToLower(k)
		if k == "" || v == "" || cafSkip[k] {
			continue
		}
		if k == "recorded date" {
			recorded = v
			continue
		}
		key, ok := cafTags[k]
		if !ok {
			key = strings.ToUpper(strings.ReplaceAll(k, " ", ""))
		}
		hasDate = hasDate || key == "DATE"
		if key == "TRACKNUMBER" || key == "DISCNUMBER" {
			if num, total, ok := strings.Cut(v, "/"); ok {
				v = strings.TrimSpace(num)
				if total = strings.TrimSpace(total); total != "" {
					info.Tags = append(info.Tags, Tag{strings.TrimSuffix(key, "NUMBER") + "TOTAL", total})
				}
			}
		}
		info.Tags = append(info.Tags, Tag{key, v})
	}
	if recorded != "" && !hasDate {
		info.Tags = append(info.Tags, Tag{"DATE", recorded})
	}
}

// HashCAFAudio decodes a CAF file's ALAC audio and returns its audio
// hash (see pcmHash): the same as a FLAC or MP4 file of the same audio.
func HashCAFAudio(fetch FetchFunc, open OpenFunc, size int64) ([]byte, error) {
	f, err := readCAF(fetch, size, false)
	if err != nil {
		return nil, err
	}
	samples, err := f.packets()
	if err != nil {
		return nil, err
	}
	if len(samples) == 0 {
		return nil, ErrNoAudio
	}
	add, finish, _, err := alacHasher(f.alac)
	if err != nil {
		return nil, err
	}
	if err := readPackets(samples, open, size, add); err != nil {
		return nil, err
	}
	return finish()
}
