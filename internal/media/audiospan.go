package media

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strconv"
)

// ErrNoAudio is returned when a file holds no audio bytes past its tags.
var ErrNoAudio = errors.New("no audio past the tags")

// spanWindow is how much is read at a time while finding the audio.
const spanWindow = 64 * 1024

// Span is where a file's audio is: bytes [Start, End).
type Span struct {
	Start, End int64
}

// AudioSpan finds the bytes of a lossy file that hold its audio: the
// range left once the tags at both ends are skipped. format is "mp3".
// Retagging a file (new tags, pictures or padding) leaves these bytes as
// they were, so a hash of them, audio_sha256 in the index, tells a tag
// edit from a change to the audio. Lossless files hash their decoded
// samples instead (see HashFLACAudio).
//
// At the start it skips ID3v2 tags; at the end an ID3v1 tag, an APEv2 tag
// and a Lyrics3 v2 tag, in any order. Only a few small reads are made,
// whatever the size of the tags: a large tag is stepped over, not read.
func AudioSpan(fetch FetchFunc, size int64, format string) (Span, error) {
	r := &spanReader{fetch: fetch, size: size}
	var s Span
	var err error
	if s.Start, err = r.skipID3v2(0); err != nil {
		return Span{}, err
	}
	switch format {
	case "mp3":
	default:
		return Span{}, fmt.Errorf("no audio span for format %q", format)
	}
	if s.End, err = r.skipTrailers(s.Start, size); err != nil {
		return Span{}, err
	}
	if s.End <= s.Start {
		return Span{}, ErrNoAudio
	}
	return s, nil
}

// spanReader reads small pieces of a file through fetch, a window at a
// time.
type spanReader struct {
	fetch FetchFunc
	size  int64
	win   []byte
	at    int64 // offset of win in the file
}

// bytes returns n bytes at pos, or io.ErrUnexpectedEOF if the file ends
// first.
func (r *spanReader) bytes(pos, n int64) ([]byte, error) {
	if pos < 0 || pos+n > r.size {
		return nil, io.ErrUnexpectedEOF
	}
	if pos >= r.at && pos+n <= r.at+int64(len(r.win)) {
		return r.win[pos-r.at : pos-r.at+n], nil
	}
	want := min(max(n, spanWindow), r.size-pos)
	b, err := r.fetch(pos, want)
	if err != nil {
		return nil, err
	}
	if int64(len(b)) < n {
		return nil, fmt.Errorf("read %d of %d bytes at offset %d: %w", len(b), n, pos, io.ErrUnexpectedEOF)
	}
	r.win, r.at = b, pos
	return b[:n], nil
}

// bytesBefore returns the n bytes that end at end, reading a window that
// ends there, as tags at the end of a file are found walking back.
func (r *spanReader) bytesBefore(end, n int64) ([]byte, error) {
	if end-n < 0 {
		return nil, io.ErrUnexpectedEOF
	}
	if end-n >= r.at && end <= r.at+int64(len(r.win)) {
		return r.win[end-n-r.at : end-r.at], nil
	}
	from := max(end-max(n, spanWindow), 0)
	b, err := r.fetch(from, end-from)
	if err != nil {
		return nil, err
	}
	if int64(len(b)) < end-from {
		return nil, fmt.Errorf("read %d of %d bytes at offset %d: %w", len(b), end-from, from, io.ErrUnexpectedEOF)
	}
	r.win, r.at = b, from
	return b[int64(len(b))-n:], nil
}

// skipID3v2 steps over any ID3v2 tags at pos, one after another as some
// taggers leave them, and returns where they end.
func (r *spanReader) skipID3v2(pos int64) (int64, error) {
	for range maxBlocks {
		if pos+10 > r.size {
			return pos, nil
		}
		b, err := r.bytes(pos, 10)
		if err != nil {
			return 0, err
		}
		if !bytes.Equal(b[:3], []byte("ID3")) {
			return pos, nil
		}
		n := int64(10 + syncsafe(b[6:10]))
		if b[5]&0x10 != 0 { // footer present
			n += 10
		}
		if pos+n > r.size {
			return 0, fmt.Errorf("ID3v2 tag runs past the end of the file: %w", ErrNoAudio)
		}
		pos += n
	}
	return 0, fmt.Errorf("more than %d ID3v2 tags", maxBlocks)
}

// skipTrailers steps back over the tags at the end of the file, from end,
// no further than start, and returns where the audio ends.
func (r *spanReader) skipTrailers(start, end int64) (int64, error) {
	for range maxBlocks {
		n, err := r.trailer(start, end)
		if err != nil {
			return 0, err
		}
		if n == 0 {
			return end, nil
		}
		end -= n
	}
	return 0, fmt.Errorf("more than %d tags at the end of the file", maxBlocks)
}

// trailer returns the length of the tag that ends at end, or 0 if none
// does.
func (r *spanReader) trailer(start, end int64) (int64, error) {
	room := end - start
	if room < 32 {
		return 0, nil
	}
	// An APEv2 tag ends in a 32-byte footer; its size counts the footer and
	// the items, and a header before them when flagged.
	tail, err := r.bytesBefore(end, min(room, 128))
	if err != nil {
		return 0, err
	}
	if foot := tail[len(tail)-32:]; bytes.Equal(foot[:8], []byte("APETAGEX")) {
		n := int64(binary.LittleEndian.Uint32(foot[12:16]))
		if binary.LittleEndian.Uint32(foot[20:24])&0x80000000 != 0 {
			n += 32
		}
		if n < 32 || n > room {
			return 0, fmt.Errorf("APE tag of %d bytes doesn't fit the file: %w", n, ErrNoAudio)
		}
		return n, nil
	}
	if room >= 128 && bytes.Equal(tail[len(tail)-128:len(tail)-125], []byte("TAG")) {
		return 128, nil // ID3v1
	}
	// Lyrics3 v2 ends in a six-digit size and "LYRICS200"; the size counts
	// from "LYRICSBEGIN" to just before those 15 bytes.
	if foot := tail[len(tail)-15:]; bytes.Equal(foot[6:], []byte("LYRICS200")) {
		n, err := strconv.ParseInt(string(foot[:6]), 10, 64)
		if err != nil || n < 11 || n+15 > room {
			return 0, nil // not a Lyrics3 tag after all
		}
		b, err := r.bytes(end-15-n, 11)
		if err != nil {
			return 0, err
		}
		if bytes.Equal(b, []byte("LYRICSBEGIN")) {
			return n + 15, nil
		}
	}
	return 0, nil
}
