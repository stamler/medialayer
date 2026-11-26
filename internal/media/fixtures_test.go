package media

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixtureHashes are the expected audio hashes of the files in testdata,
// as scripts/make-audio-fixtures.sh worked them out from ffmpeg's
// decoding.
func fixtureHashes(t *testing.T) map[string]string {
	t.Helper()
	f, err := os.Open("testdata/audio-hashes.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if name, sum, ok := strings.Cut(sc.Text(), " "); ok {
			out[name] = sum
		}
	}
	return out
}

func TestFLACFixturesMatchFFmpeg(t *testing.T) {
	n := 0
	for name, want := range fixtureHashes(t) {
		if filepath.Ext(name) != ".flac" {
			continue
		}
		n++
		t.Run(name, func(t *testing.T) {
			f, err := os.Open(filepath.Join("testdata", name))
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			got, err := HashFLACAudio(f) // also checks the MD5 ffmpeg wrote
			if err != nil {
				t.Fatal(err)
			}
			if hex.EncodeToString(got) != want {
				t.Fatalf("hash %x, ffmpeg's audio gives %s", got, want)
			}
		})
	}
	if n < 6 {
		t.Fatalf("only %d FLAC fixtures", n)
	}
}

func TestFLACDecoderChecksAndTags(t *testing.T) {
	file, err := os.ReadFile("testdata/flac16.flac")
	if err != nil {
		t.Fatal(err)
	}
	want := fixtureHashes(t)["flac16.flac"]
	// Two ID3v2 tags before the stream, one with a footer flag.
	tag := id3Tag(3, id3Frame(3, "TIT2", append([]byte{0}, "Song"...)))
	footer := bytes.Clone(tag)
	footer[5] |= 0x10
	footer = append(footer, make([]byte, 10)...)
	for name, f := range map[string][]byte{"two tags": cat(tag, footer, file), "ID3v1 after": cat(file, id3v1())} {
		got, err := HashFLACAudio(bytes.NewReader(f))
		if err != nil || hex.EncodeToString(got) != want {
			t.Errorf("%s: %x, %v", name, got, err)
		}
	}
	// FLACHeader and ParseFLAC find the stream after both tags too.
	two := cat(tag, footer, file)
	header, _, err := FLACHeader(fetchAll(two), int64(len(two)), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := ParseFLAC(header); err != nil || info.SampleRate != 44100 {
		t.Fatalf("after two ID3v2 tags: %+v, %v", info, err)
	}
	// With no sample count in STREAMINFO, an ID3v1 tag still ends the
	// audio, and other bytes there are damage.
	unknown := bytes.Clone(file)
	si := unknown[8:]
	si[13] &= 0xf0
	clear(si[14:18])
	if got, err := HashFLACAudio(bytes.NewReader(cat(unknown, id3v1()))); err != nil || hex.EncodeToString(got) != want {
		t.Errorf("no sample count, ID3v1 after: %x, %v", got, err)
	}
	if _, err := HashFLACAudio(bytes.NewReader(cat(unknown, id3v1()[:100]))); err == nil {
		t.Error("no sample count, a cut ID3v1 tag after: hashed")
	}
	// STREAMINFO must come first, at 34 bytes.
	long := cat(file[:5], []byte{0, 0, 35}, file[8:42], []byte{0}, file[42:])
	if _, err := HashFLACAudio(bytes.NewReader(long)); !errors.Is(err, ErrNotFLAC) {
		t.Errorf("STREAMINFO of 35 bytes: %v", err)
	}
	// Full-scale 32-bit stereo whose side channel ffmpeg wrapped at 32
	// bits, which RFC 9639 doesn't allow: no hash, rather than wrapping
	// the samples back as ffmpeg does.
	wrapped, err := os.ReadFile("testdata/flac32-wrapped-side.flac")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := HashFLACAudio(bytes.NewReader(wrapped)); !errors.Is(err, ErrAudioMismatch) {
		t.Errorf("wrapped side channel: %v", err)
	}
	// A flipped bit in the audio fails a frame's CRC.
	bad := bytes.Clone(file)
	bad[len(bad)-2000] ^= 0x10
	if _, err := HashFLACAudio(bytes.NewReader(bad)); err == nil {
		t.Fatal("a damaged frame hashed")
	}
}

func fileFuncs(t *testing.T, name string) (FetchFunc, OpenFunc, int64) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	open := func(off, n int64) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(b[off : off+n])), nil
	}
	return fetchAll(b), open, int64(len(b))
}

func TestALACFixturesMatchFFmpeg(t *testing.T) {
	n := 0
	for name, want := range fixtureHashes(t) {
		if ext := filepath.Ext(name); ext != ".m4a" && ext != ".mp4" && ext != ".caf" {
			continue
		}
		n++
		t.Run(name, func(t *testing.T) {
			fetch, open, size := fileFuncs(t, name)
			hash := HashMP4Audio
			if filepath.Ext(name) == ".caf" {
				hash = HashCAFAudio
			}
			got, err := hash(fetch, open, size)
			if err != nil {
				t.Fatal(err)
			}
			if hex.EncodeToString(got) != want {
				t.Fatalf("hash %x, ffmpeg's audio gives %s", got, want)
			}
		})
	}
	if n < 7 {
		t.Fatalf("only %d ALAC fixtures", n)
	}
}

func TestParseMP4(t *testing.T) {
	fetch, _, size := fileFuncs(t, "alac16-tagged.m4a")
	info, err := ParseMP4(fetch, size)
	if err != nil {
		t.Fatal(err)
	}
	tags := map[string]string{}
	for _, tg := range info.Tags {
		tags[tg.Key] = tg.Value
	}
	for k, v := range map[string]string{"TITLE": "A Title", "ARTIST": "An Artist", "ALBUM": "An Album",
		"ALBUMARTIST": "Various", "TRACKNUMBER": "3", "TRACKTOTAL": "12", "DISCNUMBER": "1", "DISCTOTAL": "2",
		"GENRE": "Rock", "DATE": "2024"} {
		if tags[k] != v {
			t.Errorf("%s = %q, want %q (all: %v)", k, tags[k], v, tags)
		}
	}
	if info.Format != "m4a" || info.SampleRate != 44100 || info.BitDepth != 16 || info.Channels != 2 ||
		info.DurationMS < 290 || info.DurationMS > 310 {
		t.Errorf("info = %+v", info)
	}
	if len(info.Pictures) != 1 || info.Pictures[0].MIME != "image/png" {
		t.Errorf("pictures = %d", len(info.Pictures))
	}
	fetch, _, size = fileFuncs(t, "aac-tagged.m4a")
	if info, err = ParseMP4(fetch, size); err != nil || info.BitDepth != 0 || info.SampleRate != 44100 || len(info.Pictures) != 1 {
		t.Fatalf("AAC info = %+v, %v", info, err)
	}
}

func TestAACHashIgnoresTags(t *testing.T) {
	h := func(name string) []byte {
		fetch, open, size := fileFuncs(t, name)
		sum, err := HashMP4Audio(fetch, open, size)
		if err != nil {
			t.Fatal(err)
		}
		return sum
	}
	if !bytes.Equal(h("aac.m4a"), h("aac-tagged.m4a")) {
		t.Fatal("tags and a cover changed an AAC file's hash")
	}
	if !bytes.Equal(h("aac.m4a"), h("aac-remux.m4a")) {
		t.Error("a remux of AAC hashes differently")
	}
	fetch, open, size := fileFuncs(t, "video.mp4")
	if _, err := ParseMP4(fetch, size); !errors.Is(err, ErrUnsupported) {
		t.Errorf("a video: %v", err)
	}
	if _, err := HashMP4Audio(fetch, open, size); !errors.Is(err, ErrUnsupported) {
		t.Errorf("a video's hash: %v", err)
	}
	// The same video track named as the audio's chapter images: not a
	// video, so the audio is a track.
	moov, err := ReadMP4Moov(fetch, size)
	if err != nil {
		t.Fatal(err)
	}
	box := func(typ string, body []byte) []byte {
		return cat(binary.BigEndian.AppendUint32(nil, uint32(8+len(body))), []byte(typ), body)
	}
	var videoID uint32
	for _, b := range boxes(moov) {
		if hdlr := fullBox(boxPath(b.body, "mdia", "hdlr")); len(hdlr) >= 8 && string(hdlr[4:8]) == "vide" {
			videoID = binary.BigEndian.Uint32(child(b.body, "tkhd")[12:])
		}
	}
	var chaptered []byte
	for _, b := range boxes(moov) {
		body := b.body
		if hdlr := fullBox(boxPath(b.body, "mdia", "hdlr")); len(hdlr) >= 8 && string(hdlr[4:8]) == "soun" {
			body = cat(body, box("tref", box("chap", binary.BigEndian.AppendUint32(nil, videoID))))
		}
		chaptered = append(chaptered, box(b.typ, body)...)
	}
	if tr, err := MP4AudioTrack(chaptered); err != nil || tr.Codec != "aac" {
		t.Errorf("chapter images: %+v, %v", tr, err)
	}
	if bytes.Equal(h("aac.m4a"), h("alac16.m4a")) {
		t.Fatal("AAC and ALAC hash the same")
	}
}

// TestDamagedFilesDontPanic flips bytes all over the fixtures: decoding
// and parsing may fail, but must not panic or hang.
func TestDamagedFilesDontPanic(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for name := range fixtureHashes(t) {
		orig, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		for range 150 {
			b := bytes.Clone(orig)
			for range 1 + r.Intn(4) {
				i := r.Intn(len(b))
				if r.Intn(3) == 0 {
					i = r.Intn(min(len(b), 4096)) // headers and boxes
				}
				b[i] ^= byte(1 << r.Intn(8))
			}
			fetch := fetchAll(b)
			open := func(off, n int64) (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(b[off : off+n])), nil
			}
			switch filepath.Ext(name) {
			case ".flac":
				_, _ = HashFLACAudio(bytes.NewReader(b))
				_, _, _ = FLACHeader(fetch, int64(len(b)), 1<<20)
			case ".oga", ".ogg":
				_, _ = HashOggFLAC(bytes.NewReader(b))
				_, _ = ParseOggFLAC(fetch, int64(len(b)), 1<<20)
			case ".caf":
				_, _ = HashCAFAudio(fetch, open, int64(len(b)))
				_, _ = ParseCAF(fetch, int64(len(b)))
			default:
				_, _ = HashMP4Audio(fetch, open, int64(len(b)))
				_, _ = ParseMP4(fetch, int64(len(b)))
			}
		}
	}
}

func TestParseCAF(t *testing.T) {
	fetch, _, size := fileFuncs(t, "alac16.caf")
	info, err := ParseCAF(fetch, size)
	if err != nil {
		t.Fatal(err)
	}
	if info.Format != "caf" || info.SampleRate != 44100 || info.BitDepth != 16 || info.Channels != 2 {
		t.Fatalf("stream %+v", info)
	}
	// ffmpeg counts whole packets in the packet table; the last packet
	// says how many samples it holds.
	if info.DurationMS != 300 {
		t.Fatalf("duration %d ms", info.DurationMS)
	}
	tags := map[string]string{}
	for _, tag := range info.Tags {
		tags[tag.Key] = tag.Value
	}
	for k, v := range map[string]string{"TITLE": "A Title", "ARTIST": "An Artist", "ALBUM": "An Album",
		"TRACKNUMBER": "3", "TRACKTOTAL": "12", "DATE": "2024"} {
		if tags[k] != v {
			t.Errorf("%s = %q, want %q (tags %v)", k, tags[k], v, info.Tags)
		}
	}
	fetch, _, size = fileFuncs(t, "alac24-51.caf")
	if info, err = ParseCAF(fetch, size); err != nil || info.BitDepth != 24 || info.Channels != 6 || info.SampleRate != 48000 {
		t.Fatalf("24-bit 5.1: %+v, %v", info, err)
	}
	if info.DurationMS != 100 {
		t.Errorf("24-bit 5.1: %d ms", info.DurationMS)
	}
	// Another codec in CAF is not a track; neither is a damaged one.
	b, err := os.ReadFile("testdata/alac16.caf")
	if err != nil {
		t.Fatal(err)
	}
	pcm := bytes.Clone(b)
	copy(pcm[bytes.Index(pcm, []byte("desc"))+12+8:], "lpcm")
	if _, err := ParseCAF(fetchAll(pcm), int64(len(pcm))); !errors.Is(err, ErrUnsupported) {
		t.Errorf("PCM in CAF: %v", err)
	}
	// The codec is checked as soon as it's known, before any chunks after.
	var trail []byte
	for range 40 {
		trail = append(trail, "uuid\x00\x00\x00\x00\x00\x01\x00\x00"...)
		trail = append(trail, make([]byte, 1<<16)...)
	}
	pcm = cat(pcm, trail)
	if _, err := ParseCAF(fetchAll(pcm), int64(len(pcm))); !errors.Is(err, ErrUnsupported) {
		t.Errorf("PCM in CAF with chunks after: %v", err)
	}
	// Tags: bounded, Apple's names, the year before the recorded date.
	var tagInfo Info
	parseCAFInfo(cat([]byte{0xff, 0xff, 0xff, 0xff}, []byte("recorded date\x002023-05-01\x00year\x002024\x00sub-title\x00Live\x00MUSICBRAINZ_TRACKID\x00abc\x00"),
		bytes.Repeat([]byte("x\x00y\x00"), 2000)), &tagInfo)
	if len(tagInfo.Tags) != cafMaxTags-1 || tagInfo.Tags[0] != (Tag{"DATE", "2024"}) || tagInfo.Tags[1] != (Tag{"SUBTITLE", "Live"}) ||
		tagInfo.Tags[2] != (Tag{"MUSICBRAINZ_TRACKID", "abc"}) {
		t.Errorf("tags %v ... (%d)", tagInfo.Tags[:3], len(tagInfo.Tags))
	}
	fetch, _, size = fileFuncs(t, "alac16.m4a")
	if _, err := ParseCAF(fetch, size); !errors.Is(err, ErrNotCAF) || errors.Is(err, ErrUnsupported) {
		t.Fatalf("an MP4 file read as CAF: %v", err)
	}
}

func TestOggFLAC(t *testing.T) {
	n := 0
	for name, want := range fixtureHashes(t) {
		if ext := filepath.Ext(name); ext != ".oga" && ext != ".ogg" {
			continue
		}
		n++
		b, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		got, err := HashOggFLAC(bytes.NewReader(b))
		if err != nil || hex.EncodeToString(got) != want {
			t.Errorf("%s: hash %x, %v; ffmpeg's audio gives %s", name, got, err, want)
		}
	}
	if n < 2 {
		t.Fatalf("only %d Ogg FLAC fixtures", n)
	}
	fetch, _, size := fileFuncs(t, "flac16.oga")
	info, err := ParseOggFLAC(fetch, size, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	tags := map[string]string{}
	for _, tag := range info.Tags {
		tags[tag.Key] = tag.Value
	}
	// ffmpeg leaves STREAMINFO's sample count out; the last page has it.
	if info.Format != "oga" || info.SampleRate != 44100 || info.BitDepth != 16 || info.DurationMS != 300 ||
		tags["TITLE"] != "A Title" || tags["ARTIST"] != "An Artist" {
		t.Errorf("flac16.oga: %+v", info)
	}
	b, err := os.ReadFile("testdata/flac16.oga")
	if err != nil {
		t.Fatal(err)
	}
	// A lost page is damage, not a shorter track.
	pages := []int{}
	for i := 0; i+4 <= len(b); i++ {
		if string(b[i:i+4]) == "OggS" {
			pages = append(pages, i)
		}
	}
	cut := cat(b[:pages[len(pages)-3]], b[pages[len(pages)-2]:])
	if _, err := HashOggFLAC(bytes.NewReader(cut)); err == nil {
		t.Error("a file missing a page hashed")
	}
	// Opus in Ogg is not a track MediaLayer reads.
	fetch, _, size = fileFuncs(t, "opus.ogg")
	if _, err := ParseOggFLAC(fetch, size, 1<<20); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Opus: %v", err)
	}
}
