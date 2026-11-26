package media

import (
	"bytes"
	"crypto/md5"
	"errors"
	"math/rand"
	"testing"

	"github.com/mewkiz/flac"
	"github.com/mewkiz/flac/frame"
	"github.com/mewkiz/flac/meta"
)

// pcm is 16-bit stereo noise, n samples a channel.
func pcm(n int, seed int64) [][]int32 {
	r := rand.New(rand.NewSource(seed))
	out := [][]int32{make([]int32, n), make([]int32, n)}
	for i := range n {
		out[0][i] = int32(r.Intn(1<<16) - 1<<15)
		out[1][i] = out[0][i]/2 + int32(r.Intn(1000))
	}
	return out
}

// encodeFLAC encodes 16-bit stereo audio at 44.1 kHz with the given
// block size and channel coding, a Vorbis comment holding title, and
// md5sum as STREAMINFO's MD5 (nil for none).
func encodeFLAC(t *testing.T, audio [][]int32, blockSize int, ch frame.Channels, title string, md5sum []byte) []byte {
	t.Helper()
	return encodeFLACBits(t, audio, 16, len(audio[0]), blockSize, ch, title, md5sum)
}

// encodeFLACBits is encodeFLAC with any bits per sample and channel
// count (ch must suit it), and nsamples as STREAMINFO's count.
func encodeFLACBits(t *testing.T, audio [][]int32, bps uint8, nsamples, blockSize int, ch frame.Channels, title string, md5sum []byte) []byte {
	t.Helper()
	n := len(audio[0])
	info := &meta.StreamInfo{
		BlockSizeMin: uint16(blockSize), BlockSizeMax: uint16(blockSize),
		SampleRate: 44100, NChannels: uint8(len(audio)), BitsPerSample: bps, NSamples: uint64(nsamples),
	}
	copy(info.MD5sum[:], md5sum)
	tags := &meta.Block{
		Header: meta.Header{Type: meta.TypeVorbisComment, Length: 1}, // any length: the encoder works it out
		Body:   &meta.VorbisComment{Vendor: "test", Tags: [][2]string{{"TITLE", title}}},
	}
	var b bytes.Buffer
	enc, err := flac.NewEncoder(&b, info, tags)
	if err != nil {
		t.Fatal(err)
	}
	for off := 0; off < n; off += blockSize {
		size := min(blockSize, n-off)
		f := &frame.Frame{Header: frame.Header{
			HasFixedBlockSize: true, BlockSize: uint16(size), SampleRate: 44100, Channels: ch, BitsPerSample: bps,
		}}
		for c := range audio {
			f.Subframes = append(f.Subframes, &frame.Subframe{
				SubHeader: frame.SubHeader{Pred: frame.PredVerbatim},
				Samples:   append([]int32(nil), audio[c][off:off+size]...),
				NSamples:  size,
			})
		}
		if err := enc.WriteFrame(f); err != nil {
			t.Fatal(err)
		}
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// pcmMD5 is FLAC's MD5 of 16-bit audio.
func pcmMD5(audio [][]int32) []byte {
	h := md5.New()
	for i := range audio[0] {
		for _, ch := range audio {
			h.Write([]byte{byte(ch[i]), byte(ch[i] >> 8)})
		}
	}
	return h.Sum(nil)
}

func hashFLAC(t *testing.T, file []byte) []byte {
	t.Helper()
	sum, err := HashFLACAudio(bytes.NewReader(file))
	if err != nil {
		t.Fatalf("HashFLACAudio: %v", err)
	}
	return sum
}

func TestHashFLACAudioIsOfTheDecodedSamples(t *testing.T) {
	audio := pcm(10_000, 1)
	sum := pcmMD5(audio)
	a := encodeFLAC(t, audio, 4096, frame.ChannelsLR, "One", sum)
	// Other block sizes, stereo coding, tags and no MD5: other bytes, the
	// same audio.
	b := encodeFLAC(t, audio, 1152, frame.ChannelsLeftSide, "A much longer title than before", nil)
	if bytes.Equal(a, b) {
		t.Fatal("the two encodings are the same bytes")
	}
	if !bytes.Equal(hashFLAC(t, a), hashFLAC(t, b)) {
		t.Fatal("the same audio encoded two ways hashes differently")
	}
	// An ID3v2 tag before and an ID3v1 tag after change nothing.
	tagged := cat(id3Tag(3, id3Frame(3, "TIT2", append([]byte{0}, "Song"...))), a, id3v1())
	if !bytes.Equal(hashFLAC(t, a), hashFLAC(t, tagged)) {
		t.Fatal("tags around the stream changed the hash")
	}
	// One changed sample is other audio.
	other := [][]int32{append([]int32(nil), audio[0]...), audio[1]}
	other[0][5000]++
	if bytes.Equal(hashFLAC(t, a), hashFLAC(t, encodeFLAC(t, other, 4096, frame.ChannelsLR, "One", nil))) {
		t.Fatal("a changed sample left the hash the same")
	}
}

func TestHashFLACAudioChecksSTREAMINFO(t *testing.T) {
	audio := pcm(5000, 2)
	bad := pcmMD5(audio)
	bad[0] ^= 1
	if _, err := HashFLACAudio(bytes.NewReader(encodeFLAC(t, audio, 4096, frame.ChannelsLR, "x", bad))); !errors.Is(err, ErrAudioMismatch) {
		t.Fatalf("a wrong MD5 gave %v, want ErrAudioMismatch", err)
	}
	// Cut short: fewer samples than STREAMINFO counts.
	file := encodeFLAC(t, audio, 1024, frame.ChannelsLR, "x", nil)
	if _, err := HashFLACAudio(bytes.NewReader(file[:len(file)/2])); err == nil {
		t.Fatal("a cut file hashed")
	}
	if _, err := HashFLACAudio(bytes.NewReader([]byte("not a flac file at all"))); !errors.Is(err, ErrNotFLAC) {
		t.Fatalf("not FLAC gave %v", err)
	}
}

// noise is n samples a channel of full-range noise at bps bits, with
// the extremes in the first samples.
func noise(channels, n int, bps uint, seed int64) [][]int32 {
	r := rand.New(rand.NewSource(seed))
	lo, span := -(int64(1) << (bps - 1)), int64(1)<<bps
	out := make([][]int32, channels)
	for c := range out {
		out[c] = make([]int32, n)
		for i := range n {
			out[c][i] = int32(lo + r.Int63n(span))
		}
		out[c][0], out[c][1] = int32(lo), int32(lo+span-1)
	}
	return out
}

func TestHashFLACAudioDepthsAndChannels(t *testing.T) {
	for _, c := range []struct {
		name     string
		bps      uint8
		channels int
		codings  []frame.Channels
	}{
		{"8-bit mono", 8, 1, []frame.Channels{frame.ChannelsMono}},
		{"20-bit stereo", 20, 2, []frame.Channels{frame.ChannelsLR, frame.ChannelsMidSide}},
		{"24-bit stereo", 24, 2, []frame.Channels{frame.ChannelsLR, frame.ChannelsSideRight, frame.ChannelsLeftSide}},
		{"16-bit 5.1", 16, 6, []frame.Channels{frame.ChannelsLRCLfeLsRs}},
	} {
		t.Run(c.name, func(t *testing.T) {
			audio := noise(c.channels, 3000, uint(c.bps), int64(c.bps))
			var want []byte
			for i, ch := range c.codings {
				got := hashFLAC(t, encodeFLACBits(t, audio, c.bps, 3000, 512*(i+1), ch, "x", nil))
				if want == nil {
					want = got
				} else if !bytes.Equal(got, want) {
					t.Fatalf("coding %v hashes differently", ch)
				}
			}
			if c.channels > 1 {
				swapped := append([][]int32{audio[1], audio[0]}, audio[2:]...)
				if bytes.Equal(want, hashFLAC(t, encodeFLACBits(t, swapped, c.bps, 3000, 512, c.codings[0], "x", nil))) {
					t.Fatal("swapped channels hash the same")
				}
			}
		})
	}
}

func TestHashFLACAudioRejectsAudioPastTheCount(t *testing.T) {
	audio := pcm(4096, 3)
	// STREAMINFO counts the first 2,048 samples; a second frame follows.
	file := encodeFLACBits(t, audio, 16, 2048, 2048, frame.ChannelsLR, "x", nil)
	if _, err := HashFLACAudio(bytes.NewReader(file)); !errors.Is(err, ErrAudioMismatch) {
		t.Fatalf("audio past the count gave %v, want ErrAudioMismatch", err)
	}
}
