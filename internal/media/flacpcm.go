package media

import (
	"bytes"
	"crypto/md5"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/mewkiz/flac"
)

// ErrAudioMismatch is returned when a FLAC file's decoded audio doesn't
// match the MD5 or the sample count its STREAMINFO block records: the
// file is damaged.
var ErrAudioMismatch = errors.New("decoded audio doesn't match the file's STREAMINFO")

// HashFLACAudio decodes the FLAC stream in r (a whole file, ID3v2 prefix
// allowed) and returns the SHA-256 of its decoded audio. Lossless files
// can hold the same audio in different encoded forms (another
// compression level or block size, a re-encode), so their hash is of
// the samples, not the encoded bytes; tags, pictures and padding never
// enter it.
//
// The hash covers a header, "flac-pcm" then the sample rate (4 bytes),
// channel count (1) and bits per sample (1), followed by every sample,
// interleaved by channel, little-endian and signed, in the fewest whole
// bytes that hold the bits per sample: the layout FLAC's own MD5 uses.
// The decoded audio is checked against STREAMINFO's MD5 and sample
// count where the encoder recorded them, and every sample against the
// bits per sample; a mismatch is ErrAudioMismatch. Bytes after the last
// sample STREAMINFO counts that aren't an audio frame (an ID3v1 tag some
// taggers append) are ignored; another frame there is a mismatch.
// Streams of more than 24 bits per sample aren't hashed: the decoder
// doesn't read them.
func HashFLACAudio(r io.Reader) ([]byte, error) {
	stream, err := flac.New(r)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotFLAC, err)
	}
	info := stream.Info
	if info.NChannels == 0 || info.BitsPerSample < 4 || info.BitsPerSample > 24 {
		return nil, fmt.Errorf("STREAMINFO says %d channels of %d bits: %w", info.NChannels, info.BitsPerSample, ErrNotFLAC)
	}
	width := (int(info.BitsPerSample) + 7) / 8
	lo, hi := int32(-1)<<(info.BitsPerSample-1), int32(1)<<(info.BitsPerSample-1)-1
	sum, check := sha256.New(), md5.New()
	head := make([]byte, 0, 14)
	head = append(head, "flac-pcm"...)
	head = binary.LittleEndian.AppendUint32(head, info.SampleRate)
	head = append(head, info.NChannels, info.BitsPerSample)
	sum.Write(head)

	var buf []byte
	var samples uint64
	for info.NSamples == 0 || samples < info.NSamples {
		f, err := stream.ParseNext()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("decode FLAC frame after %d samples: %w", samples, err)
		}
		if len(f.Subframes) != int(info.NChannels) || f.BitsPerSample != info.BitsPerSample ||
			(f.SampleRate != 0 && f.SampleRate != info.SampleRate) {
			return nil, fmt.Errorf("frame of %d channels, %d bits at %d Hz in a stream of %d, %d at %d: %w",
				len(f.Subframes), f.BitsPerSample, f.SampleRate, info.NChannels, info.BitsPerSample, info.SampleRate, ErrAudioMismatch)
		}
		n := len(f.Subframes[0].Samples)
		buf = buf[:0]
		for i := range n {
			for _, sub := range f.Subframes {
				if len(sub.Samples) != n {
					return nil, fmt.Errorf("subframes of different lengths: %w", ErrAudioMismatch)
				}
				if v := sub.Samples[i]; v < lo || v > hi {
					return nil, fmt.Errorf("sample %d out of range for %d bits: %w", v, info.BitsPerSample, ErrAudioMismatch)
				}
				s := uint32(sub.Samples[i])
				for b := range width {
					buf = append(buf, byte(s>>(8*b)))
				}
			}
		}
		sum.Write(buf)
		check.Write(buf)
		samples += uint64(n)
	}
	if info.NSamples != 0 && samples != info.NSamples {
		return nil, fmt.Errorf("decoded %d samples, STREAMINFO says %d: %w", samples, info.NSamples, ErrAudioMismatch)
	}
	if info.NSamples != 0 {
		// Audio past the count would otherwise go unhashed.
		if _, err := stream.ParseNext(); err == nil {
			return nil, fmt.Errorf("audio past the %d samples STREAMINFO counts: %w", info.NSamples, ErrAudioMismatch)
		}
	}
	if samples == 0 {
		return nil, ErrNoAudio
	}
	if info.MD5sum != [md5.Size]byte{} && !bytes.Equal(check.Sum(nil), info.MD5sum[:]) {
		return nil, fmt.Errorf("MD5: %w", ErrAudioMismatch)
	}
	return sum.Sum(nil), nil
}
