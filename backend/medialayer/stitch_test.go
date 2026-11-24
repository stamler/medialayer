package medialayer

import (
	"bytes"
	"io"
	"testing"
)

// opener serves ranges of file and records the opens it was asked for.
type opener struct {
	file  []byte
	opens [][2]int64
}

func (op *opener) openAt(offset, limit int64) (io.ReadCloser, error) {
	op.opens = append(op.opens, [2]int64{offset, limit})
	end := int64(len(op.file))
	if limit >= 0 {
		end = min(offset+limit, end)
	}
	return io.NopCloser(bytes.NewReader(op.file[offset:end])), nil
}

func TestStitchInsideHeaderNeverOpensRemote(t *testing.T) {
	file := randomBytes(10_000, 1)
	op := &opener{file: file}
	r := newStitchReader(file[:4000], 100, 3000, op.openAt, nil)
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, file[100:3000]) {
		t.Fatalf("got %d bytes, want bytes 100-3000", len(got))
	}
	if len(op.opens) != 0 {
		t.Fatalf("remote opened %v, want no opens", op.opens)
	}
}

func TestStitchLargeRangeOnlyOpensRemoteWhenReadPastHeader(t *testing.T) {
	file := randomBytes(10_000, 1)
	op := &opener{file: file}
	// rclone's VFS asks for a large chunk even when the app wants tags only.
	r := newStitchReader(file[:4000], 0, int64(len(file)), op.openAt, nil)
	buf := make([]byte, 4000)
	if _, err := io.ReadFull(r, buf); err != nil {
		t.Fatal(err)
	}
	if len(op.opens) != 0 {
		t.Fatalf("remote opened %v while reading the header", op.opens)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStitchCrossingHeaderOpensRemoteOnceAtTheRightOffset(t *testing.T) {
	file := randomBytes(10_000, 1)
	op := &opener{file: file}
	var fallbacks int
	r := newStitchReader(file[:4000], 3000, 9000, op.openAt, func() { fallbacks++ })
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, file[3000:9000]) {
		t.Fatalf("got %d bytes, want bytes 3000-9000", len(got))
	}
	if len(op.opens) != 1 || op.opens[0] != [2]int64{4000, 5000} {
		t.Fatalf("remote opens = %v, want one open at 4000 for 5000 bytes", op.opens)
	}
	if fallbacks != 1 {
		t.Fatalf("onOpen called %d times, want 1", fallbacks)
	}
}

func TestStitchShortRemoteIsAnError(t *testing.T) {
	file := randomBytes(10_000, 1)
	op := &opener{file: file[:6000]} // remote ends early
	r := newStitchReader(file[:4000], 0, 10_000, op.openAt, nil)
	if _, err := io.ReadAll(r); err != io.ErrUnexpectedEOF {
		t.Fatalf("err = %v, want io.ErrUnexpectedEOF", err)
	}
}
