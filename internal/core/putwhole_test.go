package core

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/hash"
)

// failAfter gives n bytes of r, then fails.
type failAfter struct {
	r io.Reader
	n int
}

func (f *failAfter) Read(p []byte) (int, error) {
	if f.n <= 0 {
		return 0, errors.New("connection cut")
	}
	if len(p) > f.n {
		p = p[:f.n]
	}
	n, err := f.r.Read(p)
	f.n -= n
	return n, err
}

// A write cut short on a backend that writes in place (here a local
// disk, as SMB) leaves the old file whole and no temporary file behind;
// a write that ends replaces it, with no temporary file left either.
func TestPutWholeKeepsOldFile(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	f, err := openSource(ctx, source{Folder: dir})
	if err != nil {
		t.Fatal(err)
	}
	if !f.Features().PartialUploads {
		t.Skip("the local backend no longer writes in place")
	}
	remote := repoDir + "/member.json"
	if err := putBytes(ctx, f, remote, []byte(`{"state":"synced"}`)); err != nil {
		t.Fatal(err)
	}
	next := bytes.Repeat([]byte("x"), 4096)
	err = putWhole(ctx, f, remote, &failAfter{r: bytes.NewReader(next), n: 100}, int64(len(next)), time.Now())
	if err == nil {
		t.Fatal("a cut write succeeded")
	}
	got, err := os.ReadFile(filepath.Join(dir, remote))
	if err != nil || string(got) != `{"state":"synced"}` {
		t.Fatalf("after a cut write the file is %q (%v)", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, partialName(remote))); !os.IsNotExist(err) {
		t.Errorf("a cut write left its temporary file (%v)", err)
	}
	if err := putWhole(ctx, f, remote, bytes.NewReader(next), int64(len(next)), time.Now()); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, remote)); !bytes.Equal(got, next) {
		t.Errorf("the write didn't replace the file")
	}
	entries, _ := os.ReadDir(filepath.Join(dir, repoDir))
	if len(entries) != 1 {
		t.Errorf("files left in .medialayer: %v", entries)
	}
}

// A copy cut short leaves name.<8 hex>.partial, which no listing keeps.
func TestListingSkipsPartialUploads(t *testing.T) {
	for name, want := range map[string]bool{
		"01 Song.flac.0a1b2c3d.partial": true,
		partialName("member.json"):      true,
		"01 Song.flac.partial":          false,
		"01 Song.flac.0A1B2C3D.partial": false,
		"x.0a1b2c3.partial":             false,
		".0a1b2c3d.partial":             false,
		"01 Song.flac":                  false,
	} {
		if got := partialUpload(name); got != want {
			t.Errorf("partialUpload(%q) = %v, want %v", name, got, want)
		}
	}
	ctx := context.Background()
	dir := t.TempDir()
	write(t, filepath.Join(dir, "A/01.flac"), []byte("a"))
	write(t, filepath.Join(dir, "A/02.flac.0a1b2c3d.partial"), []byte("half"))
	f, err := openSource(ctx, source{Folder: dir})
	if err != nil {
		t.Fatal(err)
	}
	l, err := listLibrary(ctx, f, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.files) != 1 || l.files["A/01.flac"] == nil {
		t.Errorf("listed %v, want only A/01.flac", l.files)
	}
}

// The precision an SMB folder keeps is seen in its probe's time.
func TestPrecisionSeen(t *testing.T) {
	p := probeTime
	for _, tc := range []struct {
		name string
		got  time.Time
		want time.Duration
	}{
		{"100 ns (NTFS, APFS)", p.Truncate(100), 0},
		{"milliseconds", p.Truncate(time.Millisecond), 0},
		{"10 ms (exFAT)", p.Truncate(10 * time.Millisecond), 10 * time.Millisecond},
		{"10 ms, rounded up", p.Truncate(10 * time.Millisecond).Add(10 * time.Millisecond), 100 * time.Millisecond},
		{"seconds (HFS+)", p.Truncate(time.Second), time.Second},
		{"seconds, rounded up", p.Truncate(time.Second).Add(time.Second), 2 * time.Second},
		{"2 seconds, down (FAT)", p.Truncate(2 * time.Second), 2 * time.Second},
		{"2 seconds, up (FAT)", p.Truncate(2 * time.Second).Add(2 * time.Second), 2 * time.Second},
		{"not kept", time.Now(), fs.ModTimeNotSupported},
	} {
		if got := precisionSeen(p, tc.got); got != tc.want {
			t.Errorf("%s: precisionSeen = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// noHashFs is a folder that keeps no hashes, as an SMB share.
type noHashFs struct{ fs.Info }

func (noHashFs) Hashes() hash.Set { return hash.NewHashSet() }

type noHashObject struct{ fs.Object }

func (o noHashObject) Fs() fs.Info { return noHashFs{o.Object.Fs()} }

// Without hashes, a snapshot is this device's when its size and time
// match the local file's, to the 2 seconds a coarse disk keeps.
func TestSnapshotWithoutHashes(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	local := filepath.Join(t.TempDir(), "000003.sqlite")
	write(t, local, []byte("index v3"))
	write(t, filepath.Join(dir, "000003.sqlite"), []byte("index v3"))
	f, err := openSource(ctx, source{Folder: dir})
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 11, 27, 12, 0, 0, 500_000_000, time.UTC)
	for _, tc := range []struct {
		name string
		off  time.Duration
		same bool
	}{
		{"same time", 0, true},
		{"kept to 2 seconds", -1500 * time.Millisecond, true},
		{"another device's", 40 * time.Second, false},
	} {
		if err := os.Chtimes(local, base, base); err != nil {
			t.Fatal(err)
		}
		at := base.Add(tc.off)
		if err := os.Chtimes(filepath.Join(dir, "000003.sqlite"), at, at); err != nil {
			t.Fatal(err)
		}
		o, err := f.NewObject(ctx, "000003.sqlite")
		if err != nil {
			t.Fatal(err)
		}
		n := noHashObject{o}
		if got := sameFile(ctx, n, local); got != tc.same {
			t.Errorf("%s: sameFile = %v, want %v", tc.name, got, tc.same)
		}
		if got := snapshotDiffers(ctx, n, local); got == tc.same {
			t.Errorf("%s: snapshotDiffers = %v, want %v", tc.name, got, !tc.same)
		}
	}
}

// A partial upload left on a secondary is removed, not trashed or
// counted; one on the primary is left alone and not copied.
func TestMirrorRemovesPartialUploads(t *testing.T) {
	ctx := context.Background()
	c, lib, sec := newMirrorCore(t)
	if err := c.sync(ctx); err != nil {
		t.Fatal(err)
	}
	onSec := filepath.Join(sec, "A/02.flac.0a1b2c3d.partial")
	onLib := filepath.Join(lib, "A/03.flac.0a1b2c3d.partial")
	write(t, onSec, []byte("half a file"))
	write(t, onLib, []byte("half a file"))
	if err := c.sync(ctx); err != nil {
		t.Fatal(err)
	}
	if s := mirrorOf(t, c, "sec1"); s.State != MirrorSynced || s.Deletes != 0 {
		t.Errorf("secondary = %+v, want synced with no deletes", s)
	}
	if _, err := os.Stat(onSec); !os.IsNotExist(err) {
		t.Errorf("the secondary's partial upload is still there (%v)", err)
	}
	if got := trashed(t, sec); len(got) != 0 {
		t.Errorf("trashed %v", got)
	}
	if _, err := os.Stat(onLib); err != nil {
		t.Errorf("the primary's partial upload went: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sec, "A/03.flac.0a1b2c3d.partial")); !os.IsNotExist(err) {
		t.Errorf("the primary's partial upload was copied (%v)", err)
	}
}
