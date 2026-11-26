package media

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestFLACSourceSeeks(t *testing.T) {
	defer func(old int64) { seekSpan = old }(seekSpan)
	seekSpan = 1 << 10 // bisect even in small files
	for _, name := range []string{"flac16.flac", "flac24.flac", "flac32.flac", "flac16-51.flac", "flac16.oga", "flac24-51.ogg",
		"flac16-cut.flac", "flac16-cut.oga", "flac16-mux.oga"} {
		t.Run(name, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join("testdata", name))
			if err != nil {
				t.Fatal(err)
			}
			ogg := filepath.Ext(name) != ".flac"
			src, err := OpenFLACSource(fetchAll(b), int64(len(b)), 1<<20, ogg)
			if err != nil {
				t.Fatal(err)
			}
			opens := 0
			open := func(off, n int64) (io.ReadCloser, error) {
				opens++
				return io.NopCloser(bytes.NewReader(b[off : off+n])), nil
			}
			// Every sample of every channel, decoded from the start.
			f, err := src.Seek(open, 0)
			if err != nil {
				t.Fatal(err)
			}
			ref := make([][]int64, src.Info.Channels)
			for {
				first, n, err := f.Next()
				if err != nil {
					break
				}
				if first != uint64(len(ref[0])) {
					t.Fatalf("frame at %d after %d samples", first, len(ref[0]))
				}
				for c := range ref {
					ref[c] = append(ref[c], f.Samples()[c][:n]...)
				}
			}
			if uint64(len(ref[0])) != src.Total {
				t.Fatalf("decoded %d samples of %d", len(ref[0]), src.Total)
			}
			total := src.Total
			for _, want := range []uint64{1, 4095, 4096, 4097, total / 3, total / 2, total - 4097, total - 1} {
				opens = 0
				f, err := src.Seek(open, want)
				if err != nil {
					t.Fatalf("seek to %d: %v", want, err)
				}
				for {
					first, n, err := f.Next()
					if err != nil {
						t.Fatalf("seek to %d: %v", want, err)
					}
					if first > want {
						t.Fatalf("seek to %d started at %d", want, first)
					}
					if first+uint64(n) <= want {
						continue
					}
					for c := range ref {
						if f.Samples()[c][want-first] != ref[c][want] {
							t.Fatalf("seek to %d: channel %d differs", want, c)
						}
					}
					break
				}
				if opens < 2 && want > 4096 {
					t.Errorf("seek to %d bisected with %d reads", want, opens)
				}
			}
		})
	}
}

func TestAppendPCM(t *testing.T) {
	got := AppendPCM(nil, [][]int64{{1, -2}, {0x1234, -1}}, 0, 2, 3, 4)
	want := []byte{0x10, 0, 0, 0x40, 0x23, 0x01, 0xe0, 0xff, 0xff, 0xf0, 0xff, 0xff}
	if !bytes.Equal(got, want) {
		t.Fatalf("% x, want % x", got, want)
	}
}
