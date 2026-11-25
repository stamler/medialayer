package medialayer

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/rclone/rclone/backend/local"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/fs/object"
	"github.com/rclone/rclone/vfs"
	"github.com/rclone/rclone/vfs/vfscommon"
)

// library is a local folder standing in for a cloud remote.
type library struct {
	t          *testing.T
	dir        string
	file       []byte // the FLAC test file
	audioStart int
	storeTo    string // store_to_block for newFs; most tests keep metadata only
	f          *Fs
}

func newLibrary(t *testing.T) *library {
	t.Helper()
	dir := t.TempDir()
	file, audioStart := makeFLAC(t, 0, typicalBlocks(300_000), randomBytes(2_000_000, 9))
	if err := os.MkdirAll(filepath.Join(dir, "Artist", "Album"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Artist", "Album", "01 Track.flac"), file, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Artist", "Album", "notes.txt"), []byte("liner notes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Artist", "Album", "fake.flac"), []byte("not really flac"), 0o644); err != nil {
		t.Fatal(err)
	}
	lib := &library{t: t, dir: dir, file: file, audioStart: audioStart, storeTo: "0"}
	lib.f = lib.newFs()
	return lib
}

func (lib *library) newFs() *Fs {
	lib.t.Helper()
	ri, err := fs.Find("medialayer")
	if err != nil {
		lib.t.Fatal(err)
	}
	// fs.ConfigMap fills in the registered defaults, as rclone does for a
	// configured remote.
	m := fs.ConfigMap(ri.Prefix, ri.Options, "mltest", configmap.Simple{
		"remote":         lib.dir,
		"store_to_block": lib.storeTo,
		"header_dir":     filepath.Join(lib.dir, "..", filepath.Base(lib.dir)+"-headers"),
	})
	f, err := NewFs(context.Background(), "mltest", "", m)
	if err != nil {
		lib.t.Fatal(err)
	}
	return f.(*Fs)
}

func (lib *library) prime() *primeResult {
	lib.t.Helper()
	out, err := lib.f.Command(context.Background(), "prime", nil, nil)
	if err != nil {
		lib.t.Fatal(err)
	}
	return out.(*primeResult)
}

func (lib *library) open(options ...fs.OpenOption) io.ReadCloser {
	lib.t.Helper()
	o, err := lib.f.NewObject(context.Background(), "Artist/Album/01 Track.flac")
	if err != nil {
		lib.t.Fatal(err)
	}
	rc, err := o.Open(context.Background(), options...)
	if err != nil {
		lib.t.Fatal(err)
	}
	return rc
}

func (lib *library) stats() map[string]int64 {
	out, _ := lib.f.Command(context.Background(), "stats", nil, nil)
	return out.(map[string]int64)
}

func readN(t *testing.T, r io.Reader, n int) []byte {
	t.Helper()
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		t.Fatal(err)
	}
	return buf
}

func TestPrimeStoresHeadersAndSkipsOthers(t *testing.T) {
	lib := newLibrary(t)
	res := lib.prime()
	if res.Primed != 1 || res.NotMedia != 1 || res.Failed != 0 {
		t.Fatalf("prime = %+v, want 1 primed, 1 not FLAC, 0 failed", res)
	}
	if res.BytesStored != int64(lib.audioStart) {
		t.Fatalf("stored %d bytes, want %d", res.BytesStored, lib.audioStart)
	}
	again := lib.prime()
	if again.Primed != 0 || again.AlreadyFresh != 1 {
		t.Fatalf("second prime = %+v, want the file already fresh", again)
	}
}

func TestTagReadIsServedLocally(t *testing.T) {
	lib := newLibrary(t)
	lib.prime()

	// A tag reader opening with a huge range but only reading the header.
	rc := lib.open(&fs.RangeOption{Start: 0, End: 128<<20 - 1})
	got := readN(t, rc, lib.audioStart)
	if err := rc.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, lib.file[:lib.audioStart]) {
		t.Fatal("header bytes differ from the file")
	}
	st := lib.stats()
	if st["served_locally"] != 1 || st["read_past_header"] != 0 {
		t.Fatalf("stats = %v, want one local open and no remote read", st)
	}
}

func TestFullReadMatchesFile(t *testing.T) {
	lib := newLibrary(t)
	lib.prime()

	rc := lib.open()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()
	if !bytes.Equal(got, lib.file) {
		t.Fatalf("read %d bytes, differs from the %d-byte file", len(got), len(lib.file))
	}
	if st := lib.stats(); st["read_past_header"] != 1 {
		t.Fatalf("stats = %v, want exactly one remote read for the audio", st)
	}
}

func TestSeekIntoAudioGoesStraightToRemote(t *testing.T) {
	lib := newLibrary(t)
	lib.prime()

	start := int64(lib.audioStart + 1000)
	rc := lib.open(&fs.SeekOption{Offset: start})
	got := readN(t, rc, 5000)
	_ = rc.Close()
	if !bytes.Equal(got, lib.file[start:start+5000]) {
		t.Fatal("audio bytes differ from the file")
	}
	if st := lib.stats(); st["passed_through"] != 1 || st["served_locally"] != 0 {
		t.Fatalf("stats = %v, want one pass-through open", st)
	}
}

func TestChangedFileIsNotServedFromStaleHeader(t *testing.T) {
	lib := newLibrary(t)
	lib.prime()

	// Retag outside MediaLayer: same size, new bytes, new modification time.
	changed := append([]byte(nil), lib.file...)
	copy(changed[50:], []byte("RETAGGED"))
	path := filepath.Join(lib.dir, "Artist", "Album", "01 Track.flac")
	if err := os.WriteFile(path, changed, 0o644); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}

	rc := lib.open()
	got := readN(t, rc, 100)
	_ = rc.Close()
	if !bytes.Equal(got, changed[:100]) {
		t.Fatal("served stale header bytes for a changed file")
	}
	if st := lib.stats(); st["stale_records_seen"] != 1 || st["served_locally"] != 0 {
		t.Fatalf("stats = %v, want the stale record ignored", st)
	}
}

func TestWriteThroughOverlayDropsHeader(t *testing.T) {
	lib := newLibrary(t)
	lib.prime()
	ctx := context.Background()

	o, err := lib.f.NewObject(ctx, "Artist/Album/01 Track.flac")
	if err != nil {
		t.Fatal(err)
	}
	newContent := append([]byte(nil), lib.file...)
	copy(newContent[60:], []byte("EDITED"))
	src := object.NewStaticObjectInfo(o.Remote(), o.ModTime(ctx), int64(len(newContent)), true, nil, nil)
	if err := o.Update(ctx, bytes.NewReader(newContent), src); err != nil {
		t.Fatal(err)
	}
	if rec := lib.f.store.get(lib.f.key(o.Remote())); rec != nil {
		t.Fatal("header record survived an update through the overlay")
	}
}

// vfsTagRead reads the metadata of the test file the way a mounted app
// does: through rclone's VFS, which is what nfsmount and mount serve. It
// returns the overlay's statistics afterwards.
func vfsTagRead(t *testing.T, mode vfscommon.CacheMode, bufferSize fs.SizeSuffix) map[string]int64 {
	t.Helper()
	lib := newLibrary(t)
	lib.prime()
	if err := config.SetCacheDir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	ctx, ci := fs.AddConfig(context.Background())
	ci.BufferSize = bufferSize
	opt := vfscommon.Opt
	opt.CacheMode = mode
	v := vfs.New(ctx, lib.f, &opt)
	defer v.Shutdown()

	h, err := v.OpenFile("Artist/Album/01 Track.flac", os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, lib.audioStart)
	if _, err := h.ReadAt(buf, 0); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf, lib.file[:lib.audioStart]) {
		t.Fatal("tag bytes read through the VFS differ from the file")
	}
	return lib.stats()
}

// With --vfs-cache-mode off and --buffer-size 0, a tag scan through a mount
// never contacts the cloud. This is the configuration the spike uses for
// read-only Tauon scans.
func TestVFSReadOnlyTagScanStaysLocal(t *testing.T) {
	st := vfsTagRead(t, vfscommon.CacheModeOff, 0)
	if st["served_locally"] != 1 || st["read_past_header"] != 0 {
		t.Fatalf("stats = %v, want the tag read answered locally with no cloud read", st)
	}
}

// These configurations still answer the app from the header store, but
// rclone reads ahead past the header in the background: the async buffer
// (--buffer-size) in cache mode off, and the downloader's minimum 1 MiB
// window in cache mode full. So each file scanned costs one cloud read.
// The test pins that behaviour so a change in rclone is noticed.
func TestVFSReadAheadStillReachesCloud(t *testing.T) {
	cases := []struct {
		name string
		mode vfscommon.CacheMode
		buf  fs.SizeSuffix
	}{
		{"off with 16M buffer", vfscommon.CacheModeOff, 16 << 20},
		{"full without buffer", vfscommon.CacheModeFull, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := vfsTagRead(t, c.mode, c.buf)
			if st["served_locally"] != 1 || st["read_past_header"] != 1 {
				t.Fatalf("stats = %v, want a local answer plus one background cloud read", st)
			}
		})
	}
}

// A retag that keeps the size and modification time (taggers can preserve
// timestamps, and FLAC padding keeps the size) is caught when the record
// holds a content hash.
func TestHashCatchesRetagWithSameSizeAndTime(t *testing.T) {
	lib := newLibrary(t)
	ctx := context.Background()
	base, err := lib.f.Fs.NewObject(ctx, "Artist/Album/01 Track.flac")
	if err != nil {
		t.Fatal(err)
	}
	sum, err := base.Hash(ctx, hash.MD5)
	if err != nil {
		t.Fatal(err)
	}
	rec := &headerRecord{Size: base.Size(), ModTime: base.ModTime(ctx), HashType: hash.MD5, Hash: sum}
	if !lib.f.matches(ctx, rec, base) {
		t.Fatal("record with the right hash does not match")
	}
	rec.Hash = "0123456789abcdef0123456789abcdef"
	if lib.f.matches(ctx, rec, base) {
		t.Fatal("record with a different hash matches")
	}
	rec.HashType, rec.Hash = hash.None, ""
	rec.ModTime = rec.ModTime.Add(time.Nanosecond)
	if lib.f.matches(ctx, rec, base) {
		t.Fatal("record 1ns off the file's modification time matches")
	}
}

// Open must not change the caller's options: rclone may reuse them.
func TestOpenLeavesCallerOptionsAlone(t *testing.T) {
	lib := newLibrary(t)
	lib.prime()
	suffix := &fs.RangeOption{Start: -1, End: 100}
	options := []fs.OpenOption{suffix}
	rc := lib.open(options...)
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()
	if options[0] != fs.OpenOption(suffix) || suffix.Start != -1 || suffix.End != 100 {
		t.Fatalf("options changed to %v", options[0])
	}
	if !bytes.Equal(got, lib.file[len(lib.file)-100:]) {
		t.Fatal("suffix range returned the wrong bytes")
	}
}
