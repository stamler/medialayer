package media

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
)

// MP4 files (.m4a, .mp4, .m4b) holding ALAC or AAC audio: their tags and
// stream details for the index, read from the "moov" box without
// touching the audio, and their audio for the hash.

// ErrNotMP4 is returned for a file that isn't an MP4 holding ALAC or AAC.
var ErrNotMP4 = errors.New("not an MP4 file with ALAC or AAC audio")

// maxPackets bounds a track's packets, against corrupt files: more than
// two days of AAC.
const maxPackets = 1 << 23

// maxMoov bounds the "moov" box read, against corrupt files; it holds the
// sample tables and the cover art, a few MB in a long file.
const maxMoov = 64 << 20

// MP4Track is the audio track of an MP4 file.
type MP4Track struct {
	Codec      string // "alac" or "aac"
	SampleRate uint32
	Channels   int
	BitDepth   int        // 0 for AAC
	ALAC       ALACConfig // for ALAC
	Timescale  uint32
	Duration   uint64 // in Timescale units
	// Samples are the track's packets in order.
	Samples []MP4Sample
	// esds is AAC's decoder configuration, part of its hash.
	esds []byte
}

// MP4Sample is one packet's place in the file.
type MP4Sample struct {
	Offset int64
	Size   uint32
}

// mp4Box is a box found by walkBoxes.
type mp4Box struct {
	typ  string
	body []byte
}

// boxes splits b into its child boxes. A malformed box ends the list.
func boxes(b []byte) []mp4Box {
	var out []mp4Box
	for len(b) >= 8 {
		n := uint64(binary.BigEndian.Uint32(b))
		typ, hdr := string(b[4:8]), uint64(8)
		switch n {
		case 0:
			n = uint64(len(b))
		case 1:
			if len(b) < 16 {
				return out
			}
			n, hdr = binary.BigEndian.Uint64(b[8:]), 16
		}
		if n < hdr || n > uint64(len(b)) {
			return out
		}
		out = append(out, mp4Box{typ, b[hdr:n]})
		b = b[n:]
	}
	return out
}

func child(b []byte, typ string) []byte {
	for _, c := range boxes(b) {
		if c.typ == typ {
			return c.body
		}
	}
	return nil
}

// boxPath follows a chain of child boxes.
func boxPath(b []byte, types ...string) []byte {
	for _, t := range types {
		if b = child(b, t); b == nil {
			return nil
		}
	}
	return b
}

// ReadMP4Moov finds an MP4 file's "moov" box, at its start or end, with a
// few small reads, and returns its body.
func ReadMP4Moov(fetch FetchFunc, size int64) ([]byte, error) {
	var pos int64
	first := true
	for pos+8 <= size {
		h, err := fetch(pos, 16)
		if err != nil {
			return nil, err
		}
		if len(h) < 8 {
			return nil, ErrNotMP4
		}
		n, hdr := int64(binary.BigEndian.Uint32(h)), int64(8)
		typ := string(h[4:8])
		if first && typ != "ftyp" {
			return nil, ErrNotMP4
		}
		first = false
		switch n {
		case 0:
			n = size - pos
		case 1:
			if len(h) < 16 {
				return nil, ErrNotMP4
			}
			n, hdr = int64(binary.BigEndian.Uint64(h[8:])), 16
		}
		if n < hdr || pos+n > size {
			return nil, fmt.Errorf("box %q runs past the end of the file: %w", typ, ErrNotMP4)
		}
		if typ == "moov" {
			if n-hdr > maxMoov {
				return nil, fmt.Errorf("moov of %d bytes: %w", n, ErrNotMP4)
			}
			b, err := fetch(pos+hdr, n-hdr)
			if err != nil {
				return nil, err
			}
			if int64(len(b)) < n-hdr {
				return nil, fmt.Errorf("short read of moov: %w", io.ErrUnexpectedEOF)
			}
			return b, nil
		}
		pos += n
	}
	return nil, fmt.Errorf("no moov box: %w", ErrNotMP4)
}

// fullBox drops a full box's version and flags.
func fullBox(b []byte) []byte {
	if len(b) < 4 {
		return nil
	}
	return b[4:]
}

// MP4AudioTrack finds the first audio track in moov and reads its
// details and sample table.
func MP4AudioTrack(moov []byte) (*MP4Track, error) {
	for _, trak := range boxes(moov) {
		if trak.typ != "trak" {
			continue
		}
		mdia := child(trak.body, "mdia")
		if hdlr := fullBox(child(mdia, "hdlr")); len(hdlr) < 8 || string(hdlr[4:8]) != "soun" {
			continue
		}
		t := &MP4Track{}
		if mdhd := child(mdia, "mdhd"); len(mdhd) >= 24 {
			if mdhd[0] == 1 && len(mdhd) >= 32 {
				t.Timescale, t.Duration = binary.BigEndian.Uint32(mdhd[20:]), binary.BigEndian.Uint64(mdhd[24:])
			} else {
				t.Timescale, t.Duration = binary.BigEndian.Uint32(mdhd[12:]), uint64(binary.BigEndian.Uint32(mdhd[16:]))
			}
		}
		stbl := boxPath(mdia, "minf", "stbl")
		if err := t.readEntry(fullBox(child(stbl, "stsd"))); err != nil {
			return nil, err
		}
		if err := t.readSampleTable(stbl); err != nil {
			return nil, err
		}
		return t, nil
	}
	return nil, fmt.Errorf("no audio track: %w", ErrNotMP4)
}

// readEntry reads the first sample description.
func (t *MP4Track) readEntry(stsd []byte) error {
	if len(stsd) < 4+8+28 {
		return fmt.Errorf("no sample description: %w", ErrNotMP4)
	}
	entries := boxes(stsd[4:])
	if len(entries) == 0 || len(entries[0].body) < 28 {
		return fmt.Errorf("no sample description: %w", ErrNotMP4)
	}
	e := entries[0]
	b := e.body
	// 6 reserved, 2 data reference index, then the sound description:
	// version, revision, vendor, channels, sample size, compression id,
	// packet size, rate (16.16), and more fields for versions 1 and 2.
	version := binary.BigEndian.Uint16(b[8:])
	t.Channels = int(binary.BigEndian.Uint16(b[16:]))
	t.SampleRate = binary.BigEndian.Uint32(b[24:]) >> 16
	rest := b[28:]
	switch version {
	case 1:
		if len(rest) < 16 {
			return ErrNotMP4
		}
		rest = rest[16:]
	case 2:
		if len(rest) < 36 {
			return ErrNotMP4
		}
		rest = rest[36:]
	}
	switch e.typ {
	case "alac":
		cookie := child(rest, "alac")
		if cookie == nil {
			return fmt.Errorf("ALAC with no magic cookie: %w", ErrNotMP4)
		}
		c, err := parseALACConfig(fullBox(cookie))
		if err != nil {
			return fmt.Errorf("%w: %v", ErrNotMP4, err)
		}
		t.Codec, t.ALAC = "alac", c
		t.SampleRate, t.Channels, t.BitDepth = c.SampleRate, int(c.Channels), int(c.BitDepth)
	case "mp4a":
		t.Codec = "aac"
		t.esds = child(rest, "esds")
		if t.esds == nil { // QuickTime keeps it in a "wave" box
			t.esds = child(child(rest, "wave"), "esds")
		}
	default:
		return fmt.Errorf("audio codec %q: %w", e.typ, ErrNotMP4)
	}
	return nil
}

// readSampleTable works out every packet's offset and size.
func (t *MP4Track) readSampleTable(stbl []byte) error {
	stsz := fullBox(child(stbl, "stsz"))
	stsc := fullBox(child(stbl, "stsc"))
	var chunks []int64
	if stco := fullBox(child(stbl, "stco")); len(stco) >= 4 {
		n := int(binary.BigEndian.Uint32(stco))
		if len(stco) < 4+4*n {
			return fmt.Errorf("short stco: %w", ErrNotMP4)
		}
		for i := range n {
			chunks = append(chunks, int64(binary.BigEndian.Uint32(stco[4+4*i:])))
		}
	} else if co64 := fullBox(child(stbl, "co64")); len(co64) >= 4 {
		n := int(binary.BigEndian.Uint32(co64))
		if len(co64) < 4+8*n {
			return fmt.Errorf("short co64: %w", ErrNotMP4)
		}
		for i := range n {
			chunks = append(chunks, int64(binary.BigEndian.Uint64(co64[4+8*i:])))
		}
	}
	if len(stsz) < 8 || len(stsc) < 4 || len(chunks) == 0 {
		return fmt.Errorf("no sample table: %w", ErrNotMP4)
	}
	fixed, count := binary.BigEndian.Uint32(stsz), int(binary.BigEndian.Uint32(stsz[4:]))
	if fixed == 0 && len(stsz) < 8+4*count {
		return fmt.Errorf("short stsz: %w", ErrNotMP4)
	}
	if count > maxPackets {
		return fmt.Errorf("%d samples: %w", count, ErrNotMP4)
	}
	size := func(i int) uint32 {
		if fixed != 0 {
			return fixed
		}
		return binary.BigEndian.Uint32(stsz[8+4*i:])
	}
	runs := int(binary.BigEndian.Uint32(stsc))
	if len(stsc) < 4+12*runs || runs == 0 {
		return fmt.Errorf("short stsc: %w", ErrNotMP4)
	}
	t.Samples = make([]MP4Sample, 0, count)
	s := 0
	for r := range runs {
		first := int(binary.BigEndian.Uint32(stsc[4+12*r:])) - 1
		per := int(binary.BigEndian.Uint32(stsc[8+12*r:]))
		last := len(chunks)
		if r+1 < runs {
			last = int(binary.BigEndian.Uint32(stsc[4+12*(r+1):])) - 1
		}
		if first < 0 || last > len(chunks) || first > last || per > count {
			return fmt.Errorf("bad stsc: %w", ErrNotMP4)
		}
		for c := first; c < last && s < count; c++ {
			off := chunks[c]
			for range per {
				if s == count {
					break
				}
				n := size(s)
				t.Samples = append(t.Samples, MP4Sample{off, n})
				off += int64(n)
				s++
			}
		}
	}
	if s != count {
		return fmt.Errorf("sample table maps %d of %d samples: %w", s, count, ErrNotMP4)
	}
	return nil
}

// ParseMP4 reads an MP4 file's tags, cover art and stream details from
// its moov box, read through fetch.
func ParseMP4(fetch FetchFunc, size int64) (*Info, error) {
	moov, err := ReadMP4Moov(fetch, size)
	if err != nil {
		return nil, err
	}
	t, err := MP4AudioTrack(moov)
	if err != nil {
		return nil, err
	}
	info := &Info{Format: "m4a", SampleRate: int(t.SampleRate), Channels: t.Channels, BitDepth: t.BitDepth}
	if t.Timescale > 0 {
		info.DurationMS = int64(t.Duration * 1000 / uint64(t.Timescale))
	}
	meta := boxPath(moov, "udta", "meta")
	if ilst := child(fullBox(meta), "ilst"); ilst != nil {
		parseILST(ilst, info)
	} else if ilst := child(meta, "ilst"); ilst != nil { // QuickTime: meta isn't a full box
		parseILST(ilst, info)
	}
	return info, nil
}

// mp4Tags maps iTunes item atoms to the index's tag names.
var mp4Tags = map[string]string{
	"\xa9nam": "TITLE", "\xa9ART": "ARTIST", "aART": "ALBUMARTIST", "\xa9alb": "ALBUM",
	"\xa9day": "DATE", "\xa9gen": "GENRE", "\xa9wrt": "COMPOSER", "\xa9cmt": "COMMENT",
	"\xa9grp": "GROUPING", "\xa9lyr": "LYRICS", "cprt": "COPYRIGHT", "\xa9too": "ENCODER",
	"sonm": "TITLESORT", "soar": "ARTISTSORT", "soaa": "ALBUMARTISTSORT", "soal": "ALBUMSORT",
	"soco": "COMPOSERSORT", "\xa9wrk": "WORK", "\xa9mvn": "MOVEMENTNAME", "desc": "DESCRIPTION",
	"\xa9con": "CONDUCTOR", "\xa9st3": "SUBTITLE",
}

// mp4Freeform maps "----" items (com.apple.iTunes names) that Picard
// writes to their Vorbis comment names; others are upper-cased.
var mp4Freeform = map[string]string{
	"MusicBrainz Track Id": "MUSICBRAINZ_RELEASETRACKID", "MusicBrainz Release Track Id": "MUSICBRAINZ_RELEASETRACKID",
	"MusicBrainz Album Id": "MUSICBRAINZ_ALBUMID", "MusicBrainz Artist Id": "MUSICBRAINZ_ARTISTID",
	"MusicBrainz Album Artist Id": "MUSICBRAINZ_ALBUMARTISTID", "MusicBrainz Release Group Id": "MUSICBRAINZ_RELEASEGROUPID",
	"MusicBrainz Work Id": "MUSICBRAINZ_WORKID", "MusicBrainz Disc Id": "MUSICBRAINZ_DISCID",
	"MusicBrainz Album Type": "RELEASETYPE", "MusicBrainz Album Status": "RELEASESTATUS",
	"MusicBrainz Album Release Country": "RELEASECOUNTRY", "MusicIP PUID": "MUSICIP_PUID",
}

// parseILST adds an iTunes metadata list's tags and covers to info.
func parseILST(ilst []byte, info *Info) {
	for _, item := range boxes(ilst) {
		name := item.typ
		if name == "----" {
			n := fullBox(child(item.body, "name"))
			if n == nil {
				continue
			}
			key, ok := mp4Freeform[string(n)]
			if !ok {
				key = strings.ToUpper(strings.ReplaceAll(string(n), " ", "_"))
			}
			if key == "MUSICBRAINZ_RELEASETRACKID" && string(n) == "MusicBrainz Track Id" {
				key = "MUSICBRAINZ_TRACKID" // the recording, in Picard's MP4 mapping
			}
			for _, d := range dataAtoms(item.body) {
				if d.typ == 1 {
					info.Tags = append(info.Tags, Tag{key, string(d.value)})
				}
			}
			continue
		}
		for _, d := range dataAtoms(item.body) {
			v := d.value
			switch {
			case name == "covr":
				mime := "image/jpeg"
				switch {
				case d.typ == 14 || bytes.HasPrefix(v, []byte("\x89PNG")):
					mime = "image/png"
				case d.typ == 27:
					mime = "image/bmp"
				}
				info.Pictures = append(info.Pictures, Picture{Type: 3, MIME: mime, Data: v})
			case name == "trkn" || name == "disk":
				if len(v) >= 6 {
					n, total := binary.BigEndian.Uint16(v[2:]), binary.BigEndian.Uint16(v[4:])
					k, kt := "TRACKNUMBER", "TRACKTOTAL"
					if name == "disk" {
						k, kt = "DISCNUMBER", "DISCTOTAL"
					}
					if n > 0 {
						info.Tags = append(info.Tags, Tag{k, strconv.Itoa(int(n))})
					}
					if total > 0 {
						info.Tags = append(info.Tags, Tag{kt, strconv.Itoa(int(total))})
					}
				}
			case name == "gnre":
				if len(v) >= 2 {
					if g := int(binary.BigEndian.Uint16(v)); g > 0 && g <= len(id3v1Genres) {
						info.Tags = append(info.Tags, Tag{"GENRE", id3v1Genres[g-1]})
					}
				}
			case name == "cpil":
				if len(v) >= 1 && v[len(v)-1] != 0 {
					info.Tags = append(info.Tags, Tag{"COMPILATION", "1"})
				}
			case name == "tmpo":
				if len(v) >= 2 {
					info.Tags = append(info.Tags, Tag{"BPM", strconv.Itoa(int(binary.BigEndian.Uint16(v)))})
				}
			default:
				if key, ok := mp4Tags[name]; ok {
					if s, ok := d.text(); ok && s != "" {
						info.Tags = append(info.Tags, Tag{key, s})
					}
				}
			}
		}
	}
}

type dataAtom struct {
	typ   uint32 // well-known type: 1 UTF-8, 2 UTF-16, 13 JPEG, 14 PNG, 21 integer...
	value []byte
}

func (d dataAtom) text() (string, bool) {
	switch d.typ {
	case 1:
		return string(d.value), true
	case 2:
		u := make([]uint16, len(d.value)/2)
		for i := range u {
			u[i] = binary.BigEndian.Uint16(d.value[2*i:])
		}
		return string(utf16.Decode(u)), true
	}
	return "", false
}

// dataAtoms lists an item's "data" atoms.
func dataAtoms(item []byte) []dataAtom {
	var out []dataAtom
	for _, c := range boxes(item) {
		if c.typ == "data" && len(c.body) >= 8 {
			out = append(out, dataAtom{binary.BigEndian.Uint32(c.body) & 0xffffff, c.body[8:]})
		}
	}
	return out
}

// OpenFunc opens a reader of length bytes of a file from offset.
type OpenFunc func(offset, length int64) (io.ReadCloser, error)

// HashMP4Audio returns an MP4 file's audio hash: for ALAC, the hash of
// its decoded samples (see pcmHash); for AAC, the SHA-256 of its decoder
// configuration and every packet, in order. Tags, cover art and the
// file's layout (where the packets sit, free space) never enter it. The
// moov box is read through fetch, then the packets in one pass through
// open.
func HashMP4Audio(fetch FetchFunc, open OpenFunc, size int64) ([]byte, error) {
	moov, err := ReadMP4Moov(fetch, size)
	if err != nil {
		return nil, err
	}
	t, err := MP4AudioTrack(moov)
	if err != nil {
		return nil, err
	}
	if len(t.Samples) == 0 {
		return nil, ErrNoAudio
	}
	var add func([]byte) error
	var finish func() ([]byte, error)
	switch t.Codec {
	case "alac":
		add, finish, err = alacHasher(t.ALAC)
		if err != nil {
			return nil, err
		}
	default:
		h := sha256.New()
		h.Write([]byte("aac\x00"))
		if t.esds != nil && len(t.esds) < 1<<16 {
			var n [4]byte
			binary.LittleEndian.PutUint32(n[:], uint32(len(t.esds)))
			h.Write(n[:])
			h.Write(t.esds)
		}
		add = func(p []byte) error {
			var n [4]byte
			binary.LittleEndian.PutUint32(n[:], uint32(len(p)))
			h.Write(n[:])
			h.Write(p)
			return nil
		}
		finish = func() ([]byte, error) { return h.Sum(nil), nil }
	}
	if err := readPackets(t.Samples, open, size, add); err != nil {
		return nil, err
	}
	return finish()
}

// alacHasher decodes ALAC packets into a pcmHash.
func alacHasher(c ALACConfig) (add func([]byte) error, finish func() ([]byte, error), err error) {
	h, err := newPCMHash(c.SampleRate, c.Channels, c.BitDepth)
	if err != nil {
		return nil, nil, err
	}
	d := NewALACDecoder(c)
	add = func(p []byte) error {
		n, err := d.Decode(p)
		if err != nil {
			return fmt.Errorf("decode ALAC after %d samples: %w", h.samples, err)
		}
		return h.add(d.Samples, n, nil)
	}
	finish = func() ([]byte, error) {
		if h.samples == 0 {
			return nil, ErrNoAudio
		}
		return h.sum.Sum(nil), nil
	}
	return add, finish, nil
}

// readPackets reads the packets in file order through one reader, and
// passes them to add in stream order.
func readPackets(samples []MP4Sample, open OpenFunc, size int64, add func([]byte) error) error {
	order := make([]int, len(samples))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return samples[order[a]].Offset < samples[order[b]].Offset })
	inOrder := sort.SliceIsSorted(order, func(a, b int) bool { return order[a] < order[b] })
	first, last := samples[order[0]], samples[order[len(order)-1]]
	end := last.Offset + int64(last.Size)
	if first.Offset < 0 || end > size {
		return fmt.Errorf("packets outside the file: %w", ErrNotMP4)
	}
	rc, err := open(first.Offset, end-first.Offset)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	pos := first.Offset
	var held map[int][]byte // packets read ahead of their turn
	next := 0
	buf := make([]byte, 0, 1<<16)
	for _, i := range order {
		s := samples[i]
		if s.Offset < pos || s.Offset+int64(s.Size) > end {
			return fmt.Errorf("overlapping packets: %w", ErrNotMP4)
		}
		if _, err := io.CopyN(io.Discard, rc, s.Offset-pos); err != nil {
			return err
		}
		if int(s.Size) > cap(buf) {
			buf = make([]byte, s.Size)
		}
		p := buf[:s.Size]
		if _, err := io.ReadFull(rc, p); err != nil {
			return err
		}
		pos = s.Offset + int64(s.Size)
		if inOrder {
			if err := add(p); err != nil {
				return err
			}
			continue
		}
		if held == nil {
			held = map[int][]byte{}
		}
		held[i] = append([]byte(nil), p...)
		for {
			q, ok := held[next]
			if !ok {
				break
			}
			delete(held, next)
			if err := add(q); err != nil {
				return err
			}
			next++
		}
	}
	if len(held) > 0 {
		return fmt.Errorf("packets left unread: %w", ErrNotMP4)
	}
	return nil
}
