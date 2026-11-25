package medialayer

import (
	"errors"
	"io"
	"sync"
	"time"
)

// streamPool keeps remote read streams open for a short time after their
// reader is closed, so that the next read of the same file at the offset
// where the last one stopped continues the same stream.
//
// This is what makes reads through nfsmount usable. NFS is stateless: the
// rclone NFS server opens the file, reads one block (32 KiB on macOS) and
// closes it again for every READ request. With --vfs-cache-mode off each of
// those becomes a new ranged request to the cloud, so reading 1 MiB of a
// file costs 32 round trips, one after the other. Tauon's tag scan reads
// about that much past the FLAC metadata, and playback reads the whole file
// this way. Continuing a parked stream turns those into a single request.
type streamPool struct {
	ttl time.Duration // how long a parked stream is kept
	max int           // most streams parked at once

	mu     sync.Mutex
	parked map[string][]*parkedStream // by file key
	count  int
	closed bool
}

// parkedStream is an open remote stream whose next byte is at pos. It can
// give bytes up to end (exclusive), where its range request stopped.
type parkedStream struct {
	key     string
	version string // size and modification time of the file it was opened on
	rc      io.ReadCloser
	pos     int64
	end     int64
	parked  time.Time
	timer   *time.Timer
}

func newStreamPool(ttl time.Duration, max int) *streamPool {
	return &streamPool{ttl: ttl, max: max, parked: map[string][]*parkedStream{}}
}

// take removes and returns a parked stream for key, opened on this version
// of the file and positioned exactly at pos, or nil. A stream that ends
// before the caller's range does is still taken: the reader opens the rest
// when it gets there.
func (p *streamPool) take(key, version string, pos int64) *parkedStream {
	p.mu.Lock()
	defer p.mu.Unlock()
	list := p.parked[key]
	for i, s := range list {
		if s.version == version && s.pos == pos {
			p.removeLocked(key, i)
			s.timer.Stop()
			return s
		}
	}
	return nil
}

// put parks rc, positioned at pos and good up to end, for reuse. If the
// pool is full the oldest parked stream is closed to make room.
func (p *streamPool) put(key, version string, rc io.ReadCloser, pos, end int64) {
	s := &parkedStream{key: key, version: version, rc: rc, pos: pos, end: end, parked: time.Now()}
	var evicted *parkedStream
	p.mu.Lock()
	if p.closed || p.max <= 0 {
		p.mu.Unlock()
		_ = rc.Close()
		return
	}
	if p.count >= p.max {
		evicted = p.removeOldestLocked()
	}
	p.parked[key] = append(p.parked[key], s)
	p.count++
	s.timer = time.AfterFunc(p.ttl, func() { p.expire(s) })
	p.mu.Unlock()
	if evicted != nil {
		_ = evicted.rc.Close()
	}
}

// expire closes s if it is still parked.
func (p *streamPool) expire(s *parkedStream) {
	if p.unpark(s) {
		_ = s.rc.Close()
	}
}

// unpark removes s from the pool and reports whether it was there.
func (p *streamPool) unpark(s *parkedStream) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, x := range p.parked[s.key] {
		if x == s {
			p.removeLocked(s.key, i)
			return true
		}
	}
	return false
}

// drop closes every stream parked for key, after the file changed.
func (p *streamPool) drop(key string) {
	p.mu.Lock()
	list := p.parked[key]
	delete(p.parked, key)
	p.count -= len(list)
	p.mu.Unlock()
	for _, s := range list {
		s.timer.Stop()
		_ = s.rc.Close()
	}
}

// close closes every parked stream and stops parking new ones.
func (p *streamPool) close() {
	p.mu.Lock()
	p.closed = true
	all := p.parked
	p.parked = map[string][]*parkedStream{}
	p.count = 0
	p.mu.Unlock()
	for _, list := range all {
		for _, s := range list {
			s.timer.Stop()
			_ = s.rc.Close()
		}
	}
}

func (p *streamPool) removeLocked(key string, i int) {
	list := p.parked[key]
	list = append(list[:i:i], list[i+1:]...)
	if len(list) == 0 {
		delete(p.parked, key)
	} else {
		p.parked[key] = list
	}
	p.count--
}

func (p *streamPool) removeOldestLocked() *parkedStream {
	var oldest *parkedStream
	oldestIdx := -1
	for _, list := range p.parked {
		for i, s := range list {
			if oldest == nil || s.parked.Before(oldest.parked) {
				oldest, oldestIdx = s, i
			}
		}
	}
	if oldest == nil {
		return nil
	}
	p.removeLocked(oldest.key, oldestIdx)
	oldest.timer.Stop()
	return oldest
}

// pooledReader reads [pos, limit) of a file from a remote stream that may
// carry on to streamEnd. On Close a stream with bytes left is parked in the
// pool instead of being closed.
type pooledReader struct {
	pool      *streamPool
	key       string
	version   string     // file version, part of the pool key
	open      openAtFunc // opens a fresh stream at an offset
	rc        io.ReadCloser
	pos       int64 // absolute offset of the next byte
	limit     int64 // absolute offset one past the last byte to return
	streamEnd int64 // absolute offset one past the last byte rc can give
	reused    bool  // rc came from the pool and has not yet returned bytes
	failed    bool  // rc returned an error, so it must not be reused
	noPark    bool  // opened with extra options, so never parked
	closed    bool
	mu        sync.Mutex
}

// Read implements io.Reader.
func (r *pooledReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, errors.New("read on closed file")
	}
	if r.pos >= r.limit {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	if r.rc == nil || r.pos >= r.streamEnd {
		// No stream yet, or a continued stream ended before our range
		// does: open the rest.
		if err := r.reopen(); err != nil {
			return 0, err
		}
	}
	want := min(int64(len(p)), r.limit-r.pos, r.streamEnd-r.pos)
	n, err := r.rc.Read(p[:want])
	if n == 0 && err != nil && r.reused {
		// A parked stream can go stale (server timeout, cancelled
		// context). Fall back to a fresh request, once.
		if openErr := r.reopen(); openErr != nil {
			return 0, openErr
		}
		want = min(int64(len(p)), r.limit-r.pos)
		n, err = r.rc.Read(p[:want])
	}
	if n > 0 {
		r.reused = false
	}
	r.pos += int64(n)
	if err == io.EOF {
		if r.pos < r.streamEnd {
			r.failed = true
			return n, io.ErrUnexpectedEOF
		}
		// The stream gave all it had; a later Read opens the rest.
		r.failed = true
		if r.pos < r.limit {
			return n, nil
		}
		return n, nilIfMore(n, err)
	}
	if err != nil {
		r.failed = true
	}
	return n, err
}

// reopen replaces the current stream with a fresh request for the rest of
// the range.
func (r *pooledReader) reopen() error {
	if r.rc != nil {
		_ = r.rc.Close()
		r.rc = nil
	}
	r.reused, r.failed = false, false
	rc, err := r.open(r.pos, r.limit-r.pos)
	if err != nil {
		r.failed = true
		return err
	}
	r.rc, r.streamEnd = rc, r.limit
	return nil
}

// Close implements io.Closer.
func (r *pooledReader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	if r.rc == nil {
		return nil
	}
	if !r.failed && !r.noPark && r.pos < r.streamEnd {
		r.pool.put(r.key, r.version, r.rc, r.pos, r.streamEnd)
		return nil
	}
	return r.rc.Close()
}
