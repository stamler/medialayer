package core

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
)

// A job opens each folder once; out of a job each open is fresh.
func TestOpenJobSourceReusesWithinAJob(t *testing.T) {
	ctx := context.Background()
	c := &Core{}
	s := source{Folder: t.TempDir()}
	a, err := c.openJobSource(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := c.openJobSource(ctx, s)
	if a == b {
		t.Fatal("out of a job, a folder was not opened afresh")
	}
	c.jobFs = map[string]fs.Fs{}
	a, _ = c.openJobSource(ctx, s)
	b, _ = c.openJobSource(ctx, source{Remote: localPrimary, Folder: s.Folder})
	if a != b {
		t.Fatal("within a job, the folder was opened twice")
	}
	if o, _ := c.openJobSource(ctx, source{Folder: t.TempDir()}); o == a {
		t.Fatal("another folder got the same client")
	}
}

// Make Primary opens each of the two folders once, though each is in
// turn the primary, a secondary and a lease holder.
func TestPromoteOpensEachFolderOnce(t *testing.T) {
	c, lib, sec := newMirrorCore(t)
	if err := c.sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.fsys = nil // as for a Proton primary, which is opened afresh each job
	c.mu.Unlock()
	var mu sync.Mutex
	opens := map[string]int{}
	orig := openSource
	openSource = func(ctx context.Context, s source) (fs.Fs, error) {
		mu.Lock()
		opens[s.Folder]++
		mu.Unlock()
		return orig(ctx, s)
	}
	t.Cleanup(func() { openSource = orig })
	if err := promote(t, c, "sec1"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if opens[lib] != 1 || opens[sec] != 1 || len(opens) != 2 {
		t.Errorf("opens = %v, want each of %s and %s once", opens, lib, sec)
	}
}

// syncJob runs a sync as the app does, as a job, and waits for it.
func syncJob(t *testing.T, c *Core) {
	t.Helper()
	if err := c.StartSync(); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	done := c.jobDone
	c.mu.Unlock()
	<-done
}

// countLists counts listings of the folder at dir from now on.
func countLists(t *testing.T, dir string) *atomic.Int64 {
	var n atomic.Int64
	orig := listLibrary
	listLibrary = func(ctx context.Context, f fs.Fs, missingOK bool) (*listing, error) {
		if filepath.Clean(f.Root()) == filepath.Clean(dir) {
			n.Add(1)
		}
		return orig(ctx, f, missingOK)
	}
	t.Cleanup(func() { listLibrary = orig })
	return &n
}

// Mirroring takes the indexer's listing of the primary, except in the
// sync that first writes a secondary's probe.
func TestMirrorReusesTheIndexersListing(t *testing.T) {
	c, lib, _ := newMirrorCore(t)
	n := countLists(t, lib)
	syncJob(t, c)
	if s := mirrorOf(t, c, "sec1"); s.State != MirrorSynced {
		t.Fatalf("secondary = %+v", s)
	}
	if n.Load() != 1 {
		t.Errorf("first sync listed the primary %d times for mirroring, want 1 (a new probe)", n.Load())
	}
	syncJob(t, c)
	if s := mirrorOf(t, c, "sec1"); s.State != MirrorSynced {
		t.Fatalf("secondary = %+v", s)
	}
	if n.Load() != 1 {
		t.Errorf("second sync listed the primary again for mirroring (%d listings in all)", n.Load())
	}
}

// A secondary inside the primary is refused in every sync: the first
// lists the primary after writing the probe, the later ones find the
// probe already in the indexer's listing.
func TestSyncRefusesNestedSecondaryAgain(t *testing.T) {
	c, lib, _ := newMirrorCore(t)
	c.mu.Lock()
	c.settings.Secondaries = []source{{ID: "sec1", Folder: filepath.Join(lib, "Backup")}}
	c.mu.Unlock()
	n := countLists(t, lib)
	for i := range 3 {
		syncJob(t, c)
		if s := mirrorOf(t, c, "sec1"); s.State != MirrorError || !strings.Contains(s.Error, "inside the primary") {
			t.Fatalf("sync %d: secondary = %+v, want refused as inside the primary", i+1, s)
		}
	}
	if n.Load() != 1 {
		t.Errorf("the primary was listed for mirroring %d times, want only in the first sync", n.Load())
	}
}

// A kept listing serves only its own folder, once, and while recent.
func TestTakeListing(t *testing.T) {
	ctx := context.Background()
	c := &Core{jobFs: map[string]fs.Fs{}}
	a, err := openSource(ctx, source{Folder: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	b, err := openSource(ctx, source{Folder: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	l := newListing()
	c.keepListing(a, l)
	if c.takeListing(b) != nil {
		t.Error("another folder took the listing")
	}
	c.keepListing(a, l)
	if c.takeListing(a) != l || c.takeListing(a) != nil {
		t.Error("the listing wasn't taken exactly once")
	}
	c.keepListing(a, l)
	c.listedAt = time.Now().Add(-listingFresh - time.Second)
	if c.takeListing(a) != nil {
		t.Error("a stale listing was taken")
	}
	c = &Core{}
	c.keepListing(a, l)
	if c.takeListing(a) != nil {
		t.Error("a listing was kept out of a job")
	}
}
