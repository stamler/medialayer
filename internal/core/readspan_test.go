package core

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// countingReader counts Read calls, as an SMB backend makes one round
// trip for each.
type countingReader struct {
	r     io.Reader
	reads int
}

func (c *countingReader) Read(p []byte) (int, error) {
	c.reads++
	return c.r.Read(p)
}

func TestReadSpanReadsOnce(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 300<<10)
	for _, tc := range []struct {
		n, want int64
	}{{64 << 10, 64 << 10}, {300 << 10, 300 << 10}, {400 << 10, 300 << 10}, {0, 0}} {
		c := &countingReader{r: bytes.NewReader(data)}
		b, err := readSpan(io.LimitReader(c, tc.n), tc.n)
		if err != nil || int64(len(b)) != tc.want {
			t.Fatalf("readSpan(%d) = %d bytes, %v", tc.n, len(b), err)
		}
		// One read for the data, and at most one more to see the end.
		if c.reads > 2 {
			t.Errorf("readSpan(%d) made %d reads", tc.n, c.reads)
		}
	}
	b, err := readSpan(bytes.NewReader(data), -1)
	if err != nil || len(b) != len(data) {
		t.Errorf("readSpan(-1) = %d bytes, %v", len(b), err)
	}
}

// cutReader gives its data, then fails as net/http does when a body
// ends before its length.
type cutReader struct{ r io.Reader }

func (c cutReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if err == io.EOF {
		return n, io.ErrUnexpectedEOF
	}
	return n, err
}

func TestReadSpanKeepsCutTransferError(t *testing.T) {
	b, err := readSpan(cutReader{bytes.NewReader([]byte("abc"))}, 10)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("readSpan of a cut transfer = %q, %v; want io.ErrUnexpectedEOF", b, err)
	}
}
