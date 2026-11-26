package media

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"testing"
)

func span(t *testing.T, file []byte, format string) []byte {
	t.Helper()
	calls := 0
	s, err := AudioSpan(fetcherFor(file, &calls), int64(len(file)), format)
	if err != nil {
		t.Fatalf("AudioSpan: %v", err)
	}
	if calls > 4 {
		t.Errorf("%d reads, want at most 4", calls)
	}
	return file[s.Start:s.End]
}

func id3v1() []byte {
	b := make([]byte, 128)
	copy(b, "TAGSome Title")
	return b
}

func apeTag(header bool, items string) []byte {
	foot := func(isHeader bool) []byte {
		b := make([]byte, 32)
		copy(b, "APETAGEX")
		binary.LittleEndian.PutUint32(b[8:], 2000)
		binary.LittleEndian.PutUint32(b[12:], uint32(len(items)+32))
		binary.LittleEndian.PutUint32(b[16:], 1)
		flags := uint32(0)
		if header {
			flags |= 0x80000000
		}
		if isHeader {
			flags |= 0x20000000
		}
		binary.LittleEndian.PutUint32(b[20:], flags)
		return b
	}
	var out []byte
	if header {
		out = append(out, foot(true)...)
	}
	out = append(out, items...)
	return append(out, foot(false)...)
}

func lyrics3(text string) []byte {
	body := "LYRICSBEGIN" + text
	return []byte(body + fmt.Sprintf("%06d", len(body)) + "LYRICS200")
}

func TestAudioSpanMP3SkipsTagsAtBothEnds(t *testing.T) {
	audio := mpegFrames(0, 100)
	plain := span(t, audio, "mp3")
	if !bytes.Equal(plain, audio) {
		t.Fatalf("untagged span is %d bytes, want %d", len(plain), len(audio))
	}
	tag := id3Tag(3, id3Frame(3, "TIT2", append([]byte{0}, "Song"...)))
	for name, file := range map[string][]byte{
		"ID3v2":             cat(tag, audio),
		"two ID3v2 tags":    cat(tag, tag, audio),
		"ID3v1":             cat(tag, audio, id3v1()),
		"APE then ID3v1":    cat(tag, audio, apeTag(true, "items items"), id3v1()),
		"ID3v1 then APE":    cat(audio, id3v1(), apeTag(false, "x")),
		"Lyrics3 and ID3v1": cat(tag, audio, lyrics3("[00:01]la la"), id3v1()),
	} {
		t.Run(name, func(t *testing.T) {
			if got := span(t, file, "mp3"); !bytes.Equal(got, audio) {
				t.Fatalf("span is %d bytes at %d, want the %d audio bytes", len(got), bytes.Index(file, got), len(audio))
			}
		})
	}
}

func cat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

func TestAudioSpanErrors(t *testing.T) {
	calls := 0
	for name, c := range map[string]struct {
		file   []byte
		format string
		want   error
	}{
		"only tags":          {cat(id3Tag(3), id3v1()), "mp3", ErrNoAudio},
		"ID3v2 past the end": {[]byte("ID3\x03\x00\x00\x00\x00\x7f\x7fshort"), "mp3", ErrNoAudio},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := AudioSpan(fetcherFor(c.file, &calls), int64(len(c.file)), c.format)
			if !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
		})
	}
	if _, err := AudioSpan(fetcherFor(nil, &calls), 0, "flac"); err == nil {
		t.Fatal("a lossless format gave a span")
	}
}
