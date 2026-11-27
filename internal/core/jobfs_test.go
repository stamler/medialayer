package core

import (
	"context"
	"sync"
	"testing"

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
