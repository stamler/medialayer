package medialayer

import (
	"errors"
	"io"
	"sync"
)

// openAtFunc opens the underlying file at offset for limit bytes
// (limit < 0 means to the end of the file).
type openAtFunc func(offset, limit int64) (io.ReadCloser, error)

// stitchReader serves a byte range of a file whose first bytes are held
// locally. Bytes inside the local header come from memory; the cloud is only
// contacted, once, if the reader actually reads past the header.
//
// This matters because readers above us ask for much more than they use:
// rclone's VFS opens with large chunk ranges (128 MiB by default) even when
// an app only wants the tags. Serving the header and opening the cloud lazily
// turns such a tag read into zero network requests.
type stitchReader struct {
	header []byte // bytes [0, len(header)) of the file
	pos    int64  // absolute offset of the next byte to return
	end    int64  // absolute offset one past the last byte to return
	openAt openAtFunc
	onOpen func() // called when the cloud is contacted, for statistics

	mu     sync.Mutex
	rc     io.ReadCloser
	closed bool
}

func newStitchReader(header []byte, offset, end int64, openAt openAtFunc, onOpen func()) *stitchReader {
	return &stitchReader{header: header, pos: offset, end: end, openAt: openAt, onOpen: onOpen}
}

// Read implements io.Reader.
func (s *stitchReader) Read(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, errors.New("read on closed file")
	}
	if s.pos >= s.end {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	if s.pos < int64(len(s.header)) {
		stop := min(int64(len(s.header)), s.end)
		n := copy(p, s.header[s.pos:stop])
		s.pos += int64(n)
		return n, nil
	}
	if s.rc == nil {
		rc, err := s.openAt(s.pos, s.end-s.pos)
		if err != nil {
			return 0, err
		}
		s.rc = rc
		if s.onOpen != nil {
			s.onOpen()
		}
	}
	want := min(int64(len(p)), s.end-s.pos)
	n, err := s.rc.Read(p[:want])
	s.pos += int64(n)
	if err == io.EOF {
		if s.pos < s.end {
			return n, io.ErrUnexpectedEOF
		}
		return n, nilIfMore(n, err)
	}
	return n, err
}

// nilIfMore hides io.EOF when bytes were returned, so the caller sees the
// bytes first and io.EOF on the next call, as io.Reader allows either way.
func nilIfMore(n int, err error) error {
	if n > 0 && err == io.EOF {
		return nil
	}
	return err
}

// Close implements io.Closer.
func (s *stitchReader) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.rc != nil {
		return s.rc.Close()
	}
	return nil
}
