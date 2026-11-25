package media

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"unicode/utf16"
)

// ErrNotMP3 is returned when a file has no ID3v2 tag and no MPEG audio
// frame where one should start.
var ErrNotMP3 = errors.New("not an MP3 stream")

// mp3Probe is how many bytes past the ID3v2 tag are read to find the first
// audio frame and its Xing or VBRI header.
const mp3Probe = 16 * 1024

// maxID3 bounds the ID3v2 tag size that will be read, against corrupt
// headers. Tags with large covers are a few MB.
const maxID3 = 32 << 20

// ParseMP3 reads the ID3v2 tag at the start of an MP3 file, and the first
// audio frame after it for the duration, using at most two reads.
func ParseMP3(fetch FetchFunc, size int64) (*Info, error) {
	first, err := fetch(0, min(int64(10+mp3Probe), size))
	if err != nil {
		return nil, err
	}
	info := &Info{Format: "mp3"}
	audioStart := int64(0)
	buf := first
	if len(first) >= 10 && bytes.Equal(first[:3], []byte("ID3")) {
		tagLen := int64(10 + syncsafe(first[6:10]))
		if first[5]&0x10 != 0 {
			tagLen += 10
		}
		if tagLen > maxID3 || tagLen > size {
			return nil, fmt.Errorf("ID3v2 tag of %d bytes: %w", tagLen, ErrNotMP3)
		}
		audioStart = tagLen
		if end := min(tagLen+mp3Probe, size); end > int64(len(first)) {
			more, err := fetch(int64(len(first)), end-int64(len(first)))
			if err != nil {
				return nil, err
			}
			buf = append(append([]byte(nil), first...), more...)
		}
		if int64(len(buf)) < tagLen {
			return nil, fmt.Errorf("short read of ID3v2 tag: %w", ErrNotMP3)
		}
		parseID3v2(buf[:tagLen], info)
	}
	if audioStart < int64(len(buf)) {
		parseMPEGDuration(buf[audioStart:], size-audioStart, info)
	}
	if len(info.Tags) == 0 && info.SampleRate == 0 {
		return nil, ErrNotMP3
	}
	return info, nil
}

func syncsafe(b []byte) int {
	return int(b[0]&0x7f)<<21 | int(b[1]&0x7f)<<14 | int(b[2]&0x7f)<<7 | int(b[3]&0x7f)
}

// removeUnsync undoes ID3 unsynchronisation: every 0xFF 0x00 becomes 0xFF.
func removeUnsync(b []byte) []byte {
	if !bytes.Contains(b, []byte{0xff, 0x00}) {
		return b
	}
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		out = append(out, b[i])
		if b[i] == 0xff && i+1 < len(b) && b[i+1] == 0x00 {
			i++
		}
	}
	return out
}

// id3Keys maps ID3v2.3/2.4 frame ids (and their v2.2 equivalents) to the
// Vorbis comment names the index uses.
var id3Keys = map[string]string{
	"TIT2": "TITLE", "TT2": "TITLE",
	"TPE1": "ARTIST", "TP1": "ARTIST",
	"TALB": "ALBUM", "TAL": "ALBUM",
	"TPE2": "ALBUMARTIST", "TP2": "ALBUMARTIST",
	"TCON": "GENRE", "TCO": "GENRE",
	"TRCK": "TRACKNUMBER", "TRK": "TRACKNUMBER",
	"TPOS": "DISCNUMBER", "TPA": "DISCNUMBER",
	"TDRC": "DATE", "TYER": "DATE", "TYE": "DATE",
	"TCOM": "COMPOSER", "TCM": "COMPOSER",
	"TCMP": "COMPILATION", "TCP": "COMPILATION",
}

// parseID3v2 adds the tags and pictures of a whole ID3v2 tag (header
// included) to info. It is lenient: a malformed frame ends the walk, and
// what was read before it is kept.
func parseID3v2(tag []byte, info *Info) {
	version := tag[3]
	flags := tag[5]
	body := tag[10:]
	if flags&0x10 != 0 && len(body) >= 10 {
		body = body[:len(body)-10] // footer
	}
	if version < 4 && flags&0x80 != 0 {
		body = removeUnsync(body)
	}
	if flags&0x40 != 0 && version >= 3 && len(body) >= 4 { // extended header
		n := int(binary.BigEndian.Uint32(body))
		if version == 4 {
			n = syncsafe(body[:4])
		} else {
			n += 4
		}
		if n > len(body) {
			return
		}
		body = body[n:]
	}

	idLen, hdrLen := 4, 10
	if version == 2 {
		idLen, hdrLen = 3, 6
	}
	for len(body) >= hdrLen && body[0] != 0 {
		id := string(body[:idLen])
		var size int
		var fflags uint16
		switch version {
		case 2:
			size = int(body[3])<<16 | int(body[4])<<8 | int(body[5])
		case 3:
			size = int(binary.BigEndian.Uint32(body[4:8]))
			fflags = binary.BigEndian.Uint16(body[8:10])
		default:
			size = syncsafe(body[4:8])
			fflags = binary.BigEndian.Uint16(body[8:10])
		}
		if size < 0 || hdrLen+size > len(body) {
			return
		}
		data := body[hdrLen : hdrLen+size]
		body = body[hdrLen+size:]

		if version == 3 && fflags&0x00c0 != 0 { // compressed or encrypted
			continue
		}
		if version == 4 {
			if fflags&0x000c != 0 { // compressed or encrypted
				continue
			}
			if fflags&0x0040 != 0 && len(data) > 0 { // grouping id
				data = data[1:]
			}
			if fflags&0x0001 != 0 && len(data) >= 4 { // data length indicator
				data = data[4:]
			}
			if fflags&0x0002 != 0 {
				data = removeUnsync(data)
			}
		}
		addID3Frame(id, data, info)
	}
}

func addID3Frame(id string, data []byte, info *Info) {
	if len(data) == 0 {
		return
	}
	switch {
	case id == "APIC" || id == "PIC":
		if p, ok := parseAPIC(id == "PIC", data); ok {
			info.Pictures = append(info.Pictures, p)
		}
	case id == "TXXX" || id == "TXX":
		parts := decodeID3Text(data[0], data[1:])
		if len(parts) >= 2 && parts[0] != "" {
			key := strings.ToUpper(parts[0])
			for _, v := range parts[1:] {
				info.Tags = append(info.Tags, Tag{key, v})
			}
		}
	default:
		key, ok := id3Keys[id]
		if !ok {
			return
		}
		for _, v := range decodeID3Text(data[0], data[1:]) {
			if v == "" {
				continue
			}
			var total string
			switch key {
			case "GENRE":
				v = id3Genre(v)
			case "TRACKNUMBER", "DISCNUMBER":
				// "3/12" becomes TRACKNUMBER=3 and TRACKTOTAL=12, as Vorbis
				// comments store them.
				v, total, _ = strings.Cut(v, "/")
			}
			info.Tags = append(info.Tags, Tag{key, v})
			if total != "" {
				totalKey := "TRACKTOTAL"
				if key == "DISCNUMBER" {
					totalKey = "DISCTOTAL"
				}
				info.Tags = append(info.Tags, Tag{totalKey, total})
			}
		}
	}
}

// parseAPIC reads an APIC (v2.3/2.4) or PIC (v2.2) frame.
func parseAPIC(v22 bool, data []byte) (Picture, bool) {
	var p Picture
	enc := data[0]
	rest := data[1:]
	if v22 {
		if len(rest) < 4 {
			return p, false
		}
		switch strings.ToUpper(string(rest[:3])) {
		case "PNG":
			p.MIME = "image/png"
		default:
			p.MIME = "image/jpeg"
		}
		rest = rest[3:]
	} else {
		i := bytes.IndexByte(rest, 0)
		if i < 0 {
			return p, false
		}
		p.MIME = strings.ToLower(string(rest[:i]))
		rest = rest[i+1:]
	}
	if len(rest) < 1 {
		return p, false
	}
	p.Type = int(rest[0])
	rest = rest[1:]
	// Skip the description, terminated by one NUL (Latin-1, UTF-8) or two
	// aligned NULs (UTF-16).
	if enc == 1 || enc == 2 {
		i := 0
		for ; i+1 < len(rest); i += 2 {
			if rest[i] == 0 && rest[i+1] == 0 {
				break
			}
		}
		if i+1 >= len(rest) {
			return p, false
		}
		rest = rest[i+2:]
	} else {
		i := bytes.IndexByte(rest, 0)
		if i < 0 {
			return p, false
		}
		rest = rest[i+1:]
	}
	if len(rest) == 0 {
		return p, false
	}
	if p.MIME == "" || !strings.Contains(p.MIME, "/") {
		p.MIME = "image/" + strings.TrimPrefix(p.MIME, "image/")
		if p.MIME == "image/" || p.MIME == "image/jpg" {
			p.MIME = "image/jpeg"
		}
	}
	p.Data = rest
	return p, true
}

// decodeID3Text decodes a text frame body and splits it at NULs, which
// separate multiple values in ID3v2.4 (and the description from the value
// in TXXX).
func decodeID3Text(enc byte, b []byte) []string {
	var s string
	switch enc {
	case 0:
		r := make([]rune, len(b))
		for i, c := range b {
			r[i] = rune(c)
		}
		s = string(r)
	case 1, 2:
		bigEndian := enc == 2
		if len(b) >= 2 && b[0] == 0xff && b[1] == 0xfe {
			bigEndian, b = false, b[2:]
		} else if len(b) >= 2 && b[0] == 0xfe && b[1] == 0xff {
			bigEndian, b = true, b[2:]
		}
		u := make([]uint16, 0, len(b)/2)
		for i := 0; i+1 < len(b); i += 2 {
			if bigEndian {
				u = append(u, uint16(b[i])<<8|uint16(b[i+1]))
			} else {
				u = append(u, uint16(b[i+1])<<8|uint16(b[i]))
			}
		}
		// Each value after a NUL may start with its own BOM.
		var parts []string
		for _, part := range splitUTF16(u) {
			if len(part) > 0 && (part[0] == 0xfeff || part[0] == 0xfffe) {
				if part[0] == 0xfffe { // byte-swapped BOM: swap the rest
					for i := range part {
						part[i] = part[i]>>8 | part[i]<<8
					}
				}
				part = part[1:]
			}
			parts = append(parts, string(utf16.Decode(part)))
		}
		return trimEmptyTail(parts)
	default:
		s = string(b)
	}
	return trimEmptyTail(strings.Split(s, "\x00"))
}

func splitUTF16(u []uint16) [][]uint16 {
	var out [][]uint16
	start := 0
	for i, c := range u {
		if c == 0 {
			out = append(out, u[start:i])
			start = i + 1
		}
	}
	return append(out, u[start:])
}

// trimEmptyTail drops the empty strings a trailing NUL terminator leaves.
func trimEmptyTail(parts []string) []string {
	for len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

// id3v1Genres are the standard ID3v1 genres, which TCON may give as "(17)"
// or "17".
var id3v1Genres = []string{
	"Blues", "Classic Rock", "Country", "Dance", "Disco", "Funk", "Grunge",
	"Hip-Hop", "Jazz", "Metal", "New Age", "Oldies", "Other", "Pop", "R&B",
	"Rap", "Reggae", "Rock", "Techno", "Industrial", "Alternative", "Ska",
	"Death Metal", "Pranks", "Soundtrack", "Euro-Techno", "Ambient",
	"Trip-Hop", "Vocal", "Jazz+Funk", "Fusion", "Trance", "Classical",
	"Instrumental", "Acid", "House", "Game", "Sound Clip", "Gospel", "Noise",
	"AlternRock", "Bass", "Soul", "Punk", "Space", "Meditative",
	"Instrumental Pop", "Instrumental Rock", "Ethnic", "Gothic", "Darkwave",
	"Techno-Industrial", "Electronic", "Pop-Folk", "Eurodance", "Dream",
	"Southern Rock", "Comedy", "Cult", "Gangsta", "Top 40", "Christian Rap",
	"Pop/Funk", "Jungle", "Native American", "Cabaret", "New Wave",
	"Psychedelic", "Rave", "Showtunes", "Trailer", "Lo-Fi", "Tribal",
	"Acid Punk", "Acid Jazz", "Polka", "Retro", "Musical", "Rock & Roll",
	"Hard Rock",
}

func id3Genre(v string) string {
	n := strings.TrimSuffix(strings.TrimPrefix(v, "("), ")")
	if strings.HasPrefix(v, "(") {
		// "(17)Rock" style: prefer the text after the reference.
		if i := strings.IndexByte(v, ')'); i >= 0 && i+1 < len(v) {
			return v[i+1:]
		}
		n = v[1:strings.IndexByte(v+")", ')')]
	}
	idx := 0
	for _, c := range n {
		if c < '0' || c > '9' {
			return v
		}
		idx = idx*10 + int(c-'0')
		if idx >= len(id3v1Genres) {
			return v
		}
	}
	if n == "" {
		return v
	}
	return id3v1Genres[idx]
}

// MPEG audio Layer III tables.
var (
	mp3BitratesV1 = [16]int{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 0}
	mp3BitratesV2 = [16]int{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, 0}
	mp3Rates      = [3]int{44100, 48000, 32000}
)

// parseMPEGDuration finds the first Layer III frame in b (the bytes after
// the ID3v2 tag) and sets the sample rate, channels and duration. audioLen
// is the number of bytes from b's start to the end of the file. A Xing or
// VBRI header gives an exact frame count; otherwise the bitrate is taken
// as constant.
func parseMPEGDuration(b []byte, audioLen int64, info *Info) {
	for i := 0; i+4 <= len(b); i++ {
		if b[i] != 0xff || b[i+1]&0xe0 != 0xe0 {
			continue
		}
		h := binary.BigEndian.Uint32(b[i:])
		version := h >> 19 & 3 // 0 = 2.5, 2 = 2, 3 = 1
		layer := h >> 17 & 3   // 1 = Layer III
		brIdx := h >> 12 & 0xf
		srIdx := h >> 10 & 3
		mode := h >> 6 & 3
		if version == 1 || layer != 1 || brIdx == 0 || brIdx == 15 || srIdx == 3 {
			continue
		}
		rate := mp3Rates[srIdx]
		bitrate := mp3BitratesV1[brIdx]
		samplesPerFrame := int64(1152)
		sideInfo := 32
		if mode == 3 {
			sideInfo = 17
		}
		switch version {
		case 2:
			rate /= 2
		case 0:
			rate /= 4
		}
		if version != 3 {
			bitrate = mp3BitratesV2[brIdx]
			samplesPerFrame = 576
			sideInfo = 17
			if mode == 3 {
				sideInfo = 9
			}
		}
		info.SampleRate = rate
		info.Channels = 2
		if mode == 3 {
			info.Channels = 1
		}
		frames := int64(0)
		if x := i + 4 + sideInfo; x+12 <= len(b) {
			tag := string(b[x : x+4])
			if tag == "Xing" || tag == "Info" {
				if flags := binary.BigEndian.Uint32(b[x+4:]); flags&1 != 0 {
					frames = int64(binary.BigEndian.Uint32(b[x+8:]))
				}
			}
		}
		if v := i + 4 + 32; frames == 0 && v+18 <= len(b) && string(b[v:v+4]) == "VBRI" {
			frames = int64(binary.BigEndian.Uint32(b[v+14:]))
		}
		switch {
		case frames > 0:
			info.DurationMS = frames * samplesPerFrame * 1000 / int64(rate)
		case bitrate > 0:
			info.DurationMS = (audioLen - int64(i)) * 8 / int64(bitrate)
		}
		return
	}
}
