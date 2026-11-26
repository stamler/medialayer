package media

import (
	"bufio"
	"bytes"
	"crypto/md5"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"io"
)

// ErrAudioMismatch is returned when a lossless file's decoded audio
// doesn't match what the file records about it (FLAC's STREAMINFO MD5 or
// sample count, say): the file is damaged.
var ErrAudioMismatch = errors.New("decoded audio doesn't match what the file records")

// pcmHash is the audio hash of decoded lossless audio. Lossless files can
// hold the same audio in different encoded forms (another codec, a
// compression level or block size, a re-encode), so their hash is of the
// samples, not the encoded bytes; tags, pictures and padding never enter
// it, and FLAC and ALAC files of the same audio hash the same.
//
// The hash covers a header, "pcm", a zero byte, then the sample rate (4
// bytes, little-endian), channel count (1) and bits per sample (1),
// followed by every sample, interleaved by channel in WAVE order, little-
// endian and signed, in the fewest whole bytes that hold the bits per
// sample: the layout FLAC's own MD5 uses. Every sample must fit the bits
// per sample.
type pcmHash struct {
	sum, md5 hash.Hash
	width    int
	lo, hi   int64
	bps      uint8
	samples  uint64
	buf      []byte
}

func newPCMHash(rate uint32, channels, bps uint8) (*pcmHash, error) {
	if channels == 0 || bps < 4 || bps > 32 {
		return nil, fmt.Errorf("%d channels of %d bits: %w", channels, bps, ErrAudioMismatch)
	}
	h := &pcmHash{sum: sha256.New(), md5: md5.New(), width: (int(bps) + 7) / 8, bps: bps,
		lo: -1 << (bps - 1), hi: 1<<(bps-1) - 1}
	head := append([]byte("pcm\x00"), 0, 0, 0, 0, channels, bps)
	binary.LittleEndian.PutUint32(head[4:], rate)
	h.sum.Write(head)
	return h, nil
}

// add hashes n samples of each channel, taking the channels in the order
// order gives (nil for as they are).
func (h *pcmHash) add(chans [][]int64, n int, order []int) error {
	h.buf = h.buf[:0]
	for i := range n {
		for c := range chans {
			if order != nil {
				c = order[c]
			}
			v := chans[c][i]
			if v < h.lo || v > h.hi {
				return fmt.Errorf("sample %d out of range for %d bits: %w", v, h.bps, ErrAudioMismatch)
			}
			for b := range h.width {
				h.buf = append(h.buf, byte(v>>(8*b)))
			}
		}
	}
	h.sum.Write(h.buf)
	h.md5.Write(h.buf)
	h.samples += uint64(n)
	return nil
}

// HashFLACAudio decodes the native FLAC stream in r (a whole file, ID3v2
// tags before it allowed) and returns its audio hash (see pcmHash). The
// decoded audio is checked against STREAMINFO's MD5 and sample count
// where the encoder recorded them; a mismatch is ErrAudioMismatch. An
// ID3v1 tag after the audio, which some taggers append, is ignored; so
// are any other bytes after the last sample STREAMINFO counts, if they
// aren't an audio frame. Another frame there is a mismatch. Every bit depth FLAC allows, 4 to 32, is decoded.
func HashFLACAudio(r io.Reader) ([]byte, error) {
	br, ok := r.(io.ByteReader)
	if !ok {
		br = bufio.NewReader(r)
	}
	d, err := NewFLACDecoder(br)
	if err != nil {
		return nil, err
	}
	return hashFLACFrames(d)
}

// hashFLACFrames hashes the frames d decodes, to the end of the stream.
func hashFLACFrames(d *FLACDecoder) ([]byte, error) {
	info := d.Info
	h, err := newPCMHash(info.SampleRate, info.Channels, info.BitsPerSample)
	if err != nil {
		return nil, err
	}
	for info.TotalSamples == 0 || h.samples < info.TotalSamples {
		n, err := d.Next()
		if errors.Is(err, io.EOF) || errors.Is(err, errMaybeID3v1) && d.restIsID3v1() {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("decode FLAC after %d samples: %w", h.samples, err)
		}
		if err := h.add(d.Samples, n, nil); err != nil {
			return nil, err
		}
	}
	if info.TotalSamples != 0 {
		if h.samples != info.TotalSamples {
			return nil, fmt.Errorf("decoded %d samples, STREAMINFO says %d: %w", h.samples, info.TotalSamples, ErrAudioMismatch)
		}
		// Audio past the count would otherwise go unhashed.
		if _, err := d.Next(); err == nil {
			return nil, fmt.Errorf("audio past the %d samples STREAMINFO counts: %w", info.TotalSamples, ErrAudioMismatch)
		}
	}
	if h.samples == 0 {
		return nil, ErrNoAudio
	}
	if info.MD5 != [md5.Size]byte{} && !bytes.Equal(h.md5.Sum(nil), info.MD5[:]) {
		return nil, fmt.Errorf("MD5: %w", ErrAudioMismatch)
	}
	return h.sum.Sum(nil), nil
}
