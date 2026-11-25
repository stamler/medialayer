package medialayer

import (
	"bytes"
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
