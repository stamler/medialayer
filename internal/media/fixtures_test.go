package media

import (
	"bufio"
	"bytes"
	"encoding/hex"
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
	// A flipped bit in the audio fails a frame's CRC.
	bad := bytes.Clone(file)
	bad[len(bad)-2000] ^= 0x10
	if _, err := HashFLACAudio(bytes.NewReader(bad)); err == nil {
		t.Fatal("a damaged frame hashed")
	}
}
