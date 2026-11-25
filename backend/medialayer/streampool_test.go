package medialayer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/vfs"
	"github.com/rclone/rclone/vfs/vfscommon"
)

// countingCloser records whether it was closed. Timers close parked
// streams from their own goroutine, hence the atomic.
type countingCloser struct {
	io.Reader
	closed atomic.Bool
}

func (c *countingCloser) Close() error { c.closed.Store(true); return nil }

func newCloser(r io.Reader) *countingCloser { return &countingCloser{Reader: r} }

// parkedCount returns how many streams the pool holds.
func (p *streamPool) parkedCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.count
}

// newTestReader returns a pooledReader over file for [pos, limit), taking a
// parked stream from pool when one fits, as openRemote does.
func newTestReader(pool *streamPool, op *opener, version string, pos, limit int64) *pooledReader {
	r := &pooledReader{pool: pool, key: "k", version: version, open: op.openAt, pos: pos, limit: limit}
	if s := pool.take("k", version, pos); s != nil {
		r.rc, r.streamEnd, r.reused = s.rc, s.end, true
		return r
	}
	rc, _ := op.openAt(pos, limit-pos)
	r.rc, r.streamEnd = rc, limit
	return r
}

// The NFS pattern: open at an offset, read one block, close, repeat at the
// next offset. After the first open every block continues the same stream.
func TestSequentialBlockReadsShareOneStream(t *testing.T) {
	file := randomBytes(200_000, 3)
	size := int64(len(file))
	op := &opener{file: file}
	pool := newStreamPool(time.Minute, 4)
	defer pool.close()

	const block = 32 * 1024
	var got []byte
	for pos := int64(0); pos < size; pos += block {
		r := newTestReader(pool, op, "v", pos, size)
		got = append(got, readN(t, r, int(min(block, size-pos)))...)
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(got, file) {
		t.Fatal("bytes read in blocks differ from the file")
	}
	if len(op.opens) != 1 {
		t.Fatalf("remote opened %d times (%v), want once", len(op.opens), op.opens)
	}
	if n := pool.parkedCount(); n != 0 {
		t.Fatalf("%d streams still parked after reading to the end", n)
	}
}

// A stream is only reused at its exact position and for the same version
// of the file.
func TestTakeMatchesPositionAndVersion(t *testing.T) {
	pool := newStreamPool(time.Minute, 4)
	defer pool.close()
	rc := newCloser(bytes.NewReader(nil))
	pool.put("k", "v1", rc, 100, 500)
	if s := pool.take("k", "v1", 101); s != nil {
		t.Fatal("stream reused at the wrong offset")
	}
	if s := pool.take("k", "v2", 100); s != nil {
		t.Fatal("stream reused for another version of the file")
	}
	if s := pool.take("k", "v1", 100); s == nil || s.rc != rc {
		t.Fatal("matching take did not return the parked stream")
	}
}

// A continued stream that ends before the caller's range (rclone's VFS
// asks for 128 MiB chunks, so on big files each block asks for a slightly
// later end) serves what it has, then the reader opens the rest.
func TestShortParkedStreamIsContinuedThenExtended(t *testing.T) {
	file := randomBytes(10_000, 5)
	op := &opener{file: file}
	pool := newStreamPool(time.Minute, 4)
	defer pool.close()
	pool.put("k", "v", io.NopCloser(bytes.NewReader(file[1000:3000])), 1000, 3000)

	r := newTestReader(pool, op, "v", 1000, 6000)
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Close()
	if !bytes.Equal(got, file[1000:6000]) {
		t.Fatal("extended read returned the wrong bytes")
	}
	if len(op.opens) != 1 || op.opens[0] != [2]int64{3000, 3000} {
		t.Fatalf("opens = %v, want one open for the rest at 3000", op.opens)
	}
}

// Parked streams are closed when they expire, when the pool is full, when
// the file is forgotten, and on shutdown.
func TestParkedStreamsAreClosed(t *testing.T) {
	pool := newStreamPool(20*time.Millisecond, 1)
	a := newCloser(bytes.NewReader(nil))
	b := newCloser(bytes.NewReader(nil))
	pool.put("a", "v", a, 0, 10)
	pool.put("b", "v", b, 0, 10) // evicts a
	if !a.closed.Load() {
		t.Fatal("oldest stream not closed when the pool was full")
	}
	time.Sleep(100 * time.Millisecond)
	if !b.closed.Load() || pool.parkedCount() != 0 {
		t.Fatal("parked stream not closed after it expired")
	}

	c := newCloser(bytes.NewReader(nil))
	pool.put("c", "v", c, 0, 10)
	pool.drop("c")
	if !c.closed.Load() {
		t.Fatal("drop did not close the parked stream")
	}

	d := newCloser(bytes.NewReader(nil))
	pool.put("d", "v", d, 0, 10)
	pool.close()
	if !d.closed.Load() {
		t.Fatal("close did not close the parked stream")
	}
	e := newCloser(bytes.NewReader(nil))
	pool.put("e", "v", e, 0, 10)
	if !e.closed.Load() || pool.parkedCount() != 0 {
		t.Fatal("stream parked after the pool was closed")
	}
}

// failingReader fails every read.
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

// A parked stream that went stale is replaced by a fresh request, and the
// caller still gets the right bytes.
func TestStaleParkedStreamFallsBackToFreshOpen(t *testing.T) {
	file := randomBytes(1000, 4)
	op := &opener{file: file}
	pool := newStreamPool(time.Minute, 4)
	defer pool.close()
	stale := newCloser(failingReader{})
	pool.put("k", "v", stale, 200, 1000)

	r := newTestReader(pool, op, "v", 200, 1000)
	got := readN(t, r, 300)
	_ = r.Close()
	if !bytes.Equal(got, file[200:500]) {
		t.Fatal("fallback read returned the wrong bytes")
	}
	if !stale.closed.Load() || len(op.opens) != 1 {
		t.Fatalf("stale closed=%v opens=%v, want stale closed and one fresh open", stale.closed.Load(), op.opens)
	}
}

// A reader that hit an error is never parked.
func TestFailedStreamIsNotParked(t *testing.T) {
	pool := newStreamPool(time.Minute, 4)
	defer pool.close()
	rc := newCloser(failingReader{})
	r := &pooledReader{pool: pool, key: "k", version: "v", rc: rc, pos: 0, limit: 1000, streamEnd: 1000,
		open: func(int64, int64) (io.ReadCloser, error) { return nil, errors.New("unused") }}
	if _, err := r.Read(make([]byte, 10)); err == nil {
		t.Fatal("want the read error")
	}
	_ = r.Close()
	if !rc.closed.Load() || pool.parkedCount() != 0 {
		t.Fatal("failed stream was parked")
	}
}

// Through rclone's VFS with the cache off, reading a file block by block
// with a fresh open for each block (what the NFS server does) reaches the
// cloud once for the bytes past the stored header, not once per block.
func TestVFSBlockReadsPastHeaderContinueOneStream(t *testing.T) {
	lib := newLibrary(t)
	lib.prime()
	if err := config.SetCacheDir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	ctx, ci := fs.AddConfig(context.Background())
	ci.BufferSize = 0
	opt := vfscommon.Opt
	opt.CacheMode = vfscommon.CacheModeOff
	v := vfs.New(ctx, lib.f, &opt)
	defer v.Shutdown()

	const block = 32 * 1024
	start := (int64(lib.audioStart)/block + 1) * block // first block wholly past the header
	stop := start + 20*block
	for pos := start; pos < stop; pos += block {
		h, err := v.OpenFile("Artist/Album/01 Track.flac", os.O_RDONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, block)
		if _, err := h.ReadAt(buf, pos); err != nil {
			t.Fatal(err)
		}
		if err := h.Close(); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(buf, lib.file[pos:pos+block]) {
			t.Fatalf("block at %d differs from the file", pos)
		}
	}
	st := lib.stats()
	if st["streams_continued"] < 18 {
		t.Fatalf("stats = %v, want nearly every block to continue the first stream", st)
	}
}

// With store_to_block, prime keeps the metadata rounded up to the block
// size, so an app that reads the whole first block through a buffer (as
// Python does on a macOS NFS mount, which reports 1 MiB blocks) is answered
// without touching the cloud. Records stored with less are primed again.
func TestStoreToBlockKeepsWholeFirstBlock(t *testing.T) {
	lib := newLibrary(t)
	lib.prime() // metadata only
	lib.storeTo = "1M"
	lib.f = lib.newFs()
	res := lib.prime()
	if res.Primed != 1 || res.BytesStored != 1<<20 {
		t.Fatalf("prime = %+v, want the short record primed again with 1 MiB", res)
	}
	if again := lib.prime(); again.AlreadyFresh != 1 {
		t.Fatalf("third prime = %+v, want the padded record already fresh", again)
	}

	if err := config.SetCacheDir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	ctx, ci := fs.AddConfig(context.Background())
	ci.BufferSize = 0
	opt := vfscommon.Opt
	opt.CacheMode = vfscommon.CacheModeOff
	v := vfs.New(ctx, lib.f, &opt)
	defer v.Shutdown()
	const block = 32 * 1024
	for pos := 0; pos < 1<<20; pos += block {
		h, err := v.OpenFile("Artist/Album/01 Track.flac", os.O_RDONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, block)
		if _, err := h.ReadAt(buf, int64(pos)); err != nil {
			t.Fatal(err)
		}
		_ = h.Close()
		if !bytes.Equal(buf, lib.file[pos:pos+block]) {
			t.Fatalf("block at %d differs from the file", pos)
		}
	}
	if st := lib.stats(); st["passed_through"] != 0 || st["read_past_header"] != 0 {
		t.Fatalf("stats = %v, want the first MiB served without the cloud", st)
	}
}
