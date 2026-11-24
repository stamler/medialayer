package medialayer

import (
	"bytes"
	"errors"
	"math/rand"
	"testing"
)

// flacBlock is one metadata block for building test files.
type flacBlock struct {
	typ  byte
	data []byte
}

// makeFLAC builds a FLAC-shaped file: an optional ID3v2 tag of id3Size
// payload bytes, "fLaC", the blocks (the last one flagged), then audio.
// It returns the file and the offset where the audio starts.
func makeFLAC(t *testing.T, id3Size int, blocks []flacBlock, audio []byte) ([]byte, int) {
	t.Helper()
	var b bytes.Buffer
	if id3Size > 0 {
		b.WriteString("ID3")
		b.Write([]byte{4, 0, 0}) // version 2.4, no flags
		b.Write([]byte{
			byte(id3Size >> 21 & 0x7f), byte(id3Size >> 14 & 0x7f),
			byte(id3Size >> 7 & 0x7f), byte(id3Size & 0x7f),
		})
		b.Write(bytes.Repeat([]byte{0}, id3Size))
	}
	b.WriteString("fLaC")
	for i, blk := range blocks {
		typ := blk.typ
		if i == len(blocks)-1 {
			typ |= 0x80
		}
		n := len(blk.data)
		b.Write([]byte{typ, byte(n >> 16), byte(n >> 8), byte(n)})
		b.Write(blk.data)
	}
	audioStart := b.Len()
	b.Write(audio)
	return b.Bytes(), audioStart
}

func randomBytes(n int, seed int64) []byte {
	buf := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(buf)
	return buf
}

// typicalBlocks is STREAMINFO, VORBIS_COMMENT, a picture of pictureSize
// bytes and PADDING, as a tagger would write them.
func typicalBlocks(pictureSize int) []flacBlock {
	return []flacBlock{
		{0, randomBytes(34, 1)},
		{4, []byte("\x0d\x00\x00\x00reference lib\x01\x00\x00\x00\x0b\x00\x00\x00TITLE=Track")},
		{6, randomBytes(pictureSize, 2)},
		{1, make([]byte, 8192)},
	}
}

// fetcherFor serves reads from file and counts them.
func fetcherFor(file []byte, calls *int) fetchFunc {
	return func(offset, length int64) ([]byte, error) {
		*calls++
		end := min(offset+length, int64(len(file)))
		if offset > end {
			return nil, nil
		}
		return append([]byte(nil), file[offset:end]...), nil
	}
}

func TestFLACHeaderSmallMetadataInOneRead(t *testing.T) {
	file, audioStart := makeFLAC(t, 0, typicalBlocks(10_000), randomBytes(500_000, 3))
	var calls int
	header, complete, err := flacHeader(fetcherFor(file, &calls), int64(len(file)), 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	if !complete || !bytes.Equal(header, file[:audioStart]) {
		t.Fatalf("got %d bytes complete=%v, want the %d bytes before the audio", len(header), complete, audioStart)
	}
	if calls != 1 {
		t.Errorf("made %d reads, want 1 for metadata that fits the first read", calls)
	}
}

func TestFLACHeaderLargePictureNeedsOneMoreRead(t *testing.T) {
	file, audioStart := makeFLAC(t, 0, typicalBlocks(700_000), randomBytes(500_000, 3))
	var calls int
	header, complete, err := flacHeader(fetcherFor(file, &calls), int64(len(file)), 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	if !complete || !bytes.Equal(header, file[:audioStart]) {
		t.Fatalf("got %d bytes complete=%v, want the %d bytes before the audio", len(header), complete, audioStart)
	}
	if calls != 2 {
		t.Errorf("made %d reads, want 2 (start, then the rest of the picture and the next block)", calls)
	}
}

func TestFLACHeaderSkipsID3v2Prefix(t *testing.T) {
	file, audioStart := makeFLAC(t, 3000, typicalBlocks(1000), randomBytes(100_000, 3))
	var calls int
	header, complete, err := flacHeader(fetcherFor(file, &calls), int64(len(file)), 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	if !complete || !bytes.Equal(header, file[:audioStart]) {
		t.Fatalf("got %d bytes complete=%v, want the %d bytes before the audio", len(header), complete, audioStart)
	}
}

func TestFLACHeaderTruncatesAtMaxLen(t *testing.T) {
	file, _ := makeFLAC(t, 0, typicalBlocks(700_000), randomBytes(500_000, 3))
	const maxLen = 300_000
	var calls int
	header, complete, err := flacHeader(fetcherFor(file, &calls), int64(len(file)), maxLen)
	if err != nil {
		t.Fatal(err)
	}
	if complete {
		t.Fatal("complete = true for metadata larger than maxLen")
	}
	if !bytes.Equal(header, file[:maxLen]) {
		t.Fatalf("got %d bytes, want the first %d bytes of the file", len(header), maxLen)
	}
}

func TestFLACHeaderRejectsOtherFiles(t *testing.T) {
	tests := map[string][]byte{
		"empty":        nil,
		"text":         []byte("hello, this is not audio at all"),
		"mp3 id3 only": append([]byte("ID3\x04\x00\x00\x00\x00\x00\x05"), []byte("12345\xff\xfbaudio")...),
	}
	for name, file := range tests {
		t.Run(name, func(t *testing.T) {
			var calls int
			_, _, err := flacHeader(fetcherFor(file, &calls), int64(len(file)), 16<<20)
			if !errors.Is(err, errNotFLAC) {
				t.Fatalf("err = %v, want errNotFLAC", err)
			}
		})
	}
}

func TestFLACHeaderRejectsBlockPastEndOfFile(t *testing.T) {
	file, _ := makeFLAC(t, 0, typicalBlocks(1000), nil)
	file = file[:len(file)-100] // cut into the padding block
	var calls int
	_, _, err := flacHeader(fetcherFor(file, &calls), int64(len(file)), 16<<20)
	if !errors.Is(err, errNotFLAC) {
		t.Fatalf("err = %v, want errNotFLAC", err)
	}
}

// A read that comes back short is a failure to retry, not a non-FLAC file.
func TestFLACHeaderShortReadIsAnError(t *testing.T) {
	file, _ := makeFLAC(t, 0, typicalBlocks(1000), randomBytes(100_000, 3))
	short := func(offset, length int64) ([]byte, error) {
		return file[offset : offset+length/2], nil
	}
	_, _, err := flacHeader(short, int64(len(file)), 16<<20)
	if err == nil || errors.Is(err, errNotFLAC) {
		t.Fatalf("err = %v, want a read error that is not errNotFLAC", err)
	}
}
