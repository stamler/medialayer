package core

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rclone/rclone/fs"
)

// Each version gets an id, naming the one before as its parent; an index
// from before ids gets one at the next sync.
func TestVersionsHaveIDs(t *testing.T) {
	ctx := context.Background()
	c, _, _ := newMirrorCore(t)
	if err := c.sync(ctx); err != nil {
		t.Fatal(err)
	}
	db := c.currentDB()
	first, _ := db.Meta("version_id")
	if first == "" {
		t.Fatal("the first version has no id")
	}
	v1, _ := metaInt(db, "version")
	// As an index made before ids were.
	if err := db.SetMeta("version_id", ""); err != nil {
		t.Fatal(err)
	}
	if err := c.sync(ctx); err != nil {
		t.Fatal(err)
	}
	second, _ := db.Meta("version_id")
	v2, _ := metaInt(db, "version")
	if second == "" || second == first || v2 != v1+1 {
		t.Fatalf("after a sync of an index with no id: version %d id %q (was %d %q)", v2, second, v1, first)
	}
	_ = db.SetMeta("dirty", "1")
	if err := c.sync(ctx); err != nil {
		t.Fatal(err)
	}
	third, _ := db.Meta("version_id")
	parent, _ := db.Meta("version_parent")
	if third == second || parent != second {
		t.Fatalf("version id %q, parent %q; want a new id whose parent is %q", third, parent, second)
	}
}

// Make Primary refuses a folder whose member.json names this version's
// number but another index's id.
func TestPromotableChecksVersionID(t *testing.T) {
	ctx := context.Background()
	c, _, sec := newMirrorCore(t)
	if err := c.sync(ctx); err != nil {
		t.Fatal(err)
	}
	f, err := c.libraryFs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := readRepo(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	dst, err := openSource(ctx, source{Folder: sec})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.promotable(ctx, dst, repo); err != nil {
		t.Fatalf("a synced folder isn't promotable: %v", err)
	}
	p := filepath.Join(sec, ".medialayer/member.json")
	var m memberRecord
	b, _ := os.ReadFile(p)
	if err := json.Unmarshal(b, &m); err != nil || m.VersionID == "" {
		t.Fatalf("member.json = %s (%v), want a version id", b, err)
	}
	m.VersionID = "another device's"
	b, _ = json.Marshal(m)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.promotable(ctx, dst, repo); err == nil || !strings.Contains(err.Error(), "different index") {
		t.Fatalf("promotable = %v, want refused for another index", err)
	}
}

func playlistNames(t *testing.T, c *Core) []string {
	t.Helper()
	lists, err := c.currentDB().Playlists()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range lists {
		names = append(names, p.Name)
	}
	slices.Sort(names)
	return names
}

// A device whose index is behind the primary's takes the primary's
// version rather than publish over it: a clean index as it is, one with
// playlists not yet saved with those carried on top.
func TestPublishTakesANewerPrimary(t *testing.T) {
	ctx := context.Background()
	lib := writeLibrary(t, nil)
	c1, c2 := newTestCore(t, lib), newTestCore(t, lib)
	sync := func(c *Core) {
		t.Helper()
		if err := c.sync(ctx); err != nil {
			t.Fatal(err)
		}
	}
	sync(c1)
	sync(c2) // restores c1's version
	if _, err := c2.CreatePlaylist("two"); err != nil {
		t.Fatal(err)
	}
	sync(c2)
	sync(c1) // clean, and behind
	if got := playlistNames(t, c1); !slices.Equal(got, []string{"two"}) {
		t.Fatalf("clean device behind: playlists %v, want [two]", got)
	}
	// Both make a playlist; c1 publishes first.
	if _, err := c1.CreatePlaylist("one"); err != nil {
		t.Fatal(err)
	}
	if _, err := c2.CreatePlaylist("three"); err != nil {
		t.Fatal(err)
	}
	sync(c1)
	sync(c2)
	if got := playlistNames(t, c2); !slices.Equal(got, []string{"one", "three", "two"}) {
		t.Fatalf("device with unsaved playlists: %v, want one, three and two", got)
	}
	sync(c1)
	if got := playlistNames(t, c1); !slices.Equal(got, []string{"one", "three", "two"}) {
		t.Fatalf("first device after: %v", got)
	}
	// A removal on one device stays removed on the other.
	lists, _ := c1.currentDB().Playlists()
	for _, p := range lists {
		if p.Name == "two" {
			if err := c1.DeletePlaylist(p.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	sync(c1)
	sync(c2)
	if got := playlistNames(t, c2); !slices.Equal(got, []string{"one", "three"}) {
		t.Fatalf("after a removal elsewhere: %v, want one and three", got)
	}
	v1, _ := metaInt(c1.currentDB(), "version")
	v2, _ := metaInt(c2.currentDB(), "version")
	id1, _ := c1.currentDB().Meta("version_id")
	id2, _ := c2.currentDB().Meta("version_id")
	if v1 != v2 || id1 != id2 || id1 == "" {
		t.Errorf("devices at version %d %q and %d %q, want the same", v1, id1, v2, id2)
	}
}

// Two devices that each made the same version number: the one that
// reaches the primary second takes the primary's, and later syncs are
// quiet.
func TestPublishTakesAnotherIndexOfTheSameNumber(t *testing.T) {
	ctx := context.Background()
	lib := writeLibrary(t, nil)
	c1, c2 := newTestCore(t, lib), newTestCore(t, lib)
	for _, c := range []*Core{c1, c2} {
		if err := c.sync(ctx); err != nil {
			t.Fatal(err)
		}
	}
	// Both at version 1. c2 makes version 2 locally but the primary
	// never takes it (its playlist stays unsaved), then c1 publishes its
	// own version 2.
	if _, err := c2.CreatePlaylist("mine"); err != nil {
		t.Fatal(err)
	}
	v, _ := metaInt(c2.currentDB(), "version")
	if _, _, err := c2.currentVersion(c2.currentDB(), v, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := c1.CreatePlaylist("theirs"); err != nil {
		t.Fatal(err)
	}
	if err := c1.sync(ctx); err != nil {
		t.Fatal(err)
	}
	v1, _ := metaInt(c1.currentDB(), "version")
	v2, _ := metaInt(c2.currentDB(), "version")
	if v1 != v2 {
		t.Fatalf("versions %d and %d, want the same number", v1, v2)
	}
	if err := c2.sync(ctx); err != nil {
		t.Fatal(err)
	}
	if got := playlistNames(t, c2); !slices.Equal(got, []string{"mine", "theirs"}) {
		t.Fatalf("playlists %v, want mine and theirs", got)
	}
	if err := c1.sync(ctx); err != nil {
		t.Fatal(err)
	}
	if got := playlistNames(t, c1); !slices.Equal(got, []string{"mine", "theirs"}) {
		t.Fatalf("first device: playlists %v", got)
	}
	// Quiet now: no new version on either.
	before, _ := c1.currentDB().Meta("version_id")
	for _, c := range []*Core{c2, c1, c2} {
		if err := c.sync(ctx); err != nil {
			t.Fatal(err)
		}
		if id, _ := c.currentDB().Meta("version_id"); id != before {
			t.Fatalf("a quiet sync changed the version id to %q (was %q)", id, before)
		}
	}
}

// A primary that holds another library is refused, not taken.
func TestPublishRefusesAnotherLibrary(t *testing.T) {
	ctx := context.Background()
	lib := writeLibrary(t, nil)
	c := newTestCore(t, lib)
	if err := c.sync(ctx); err != nil {
		t.Fatal(err)
	}
	other := writeLibrary(t, nil)
	if err := newTestCore(t, other).sync(ctx); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(lib, ".medialayer")); err != nil {
		t.Fatal(err)
	}
	if err := os.CopyFS(filepath.Join(lib, ".medialayer"), os.DirFS(filepath.Join(other, ".medialayer"))); err != nil {
		t.Fatal(err)
	}
	id, _ := c.currentDB().Meta("library_id")
	err := c.sync(ctx)
	if err == nil || !strings.Contains(err.Error(), "holds library") {
		t.Fatalf("sync = %v, want refused for another library", err)
	}
	if now, _ := c.currentDB().Meta("library_id"); now != id {
		t.Errorf("the local index became library %s", now)
	}
}

// sizeless is a cloud object that gives no size.
type sizeless struct{ fs.Object }

func (sizeless) Size() int64 { return -1 }

// A snapshot is taken as different only on what both sides can tell.
func TestSnapshotDiffers(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.sqlite"), []byte("index one"))
	write(t, filepath.Join(dir, "b.sqlite"), []byte("index two"))
	f, err := openSource(ctx, source{Folder: dir})
	if err != nil {
		t.Fatal(err)
	}
	o, err := f.NewObject(ctx, "a.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	same, other := filepath.Join(dir, "a.sqlite"), filepath.Join(dir, "b.sqlite")
	for _, tc := range []struct {
		o     fs.Object
		local string
		want  bool
	}{{o, same, false}, {o, other, true}, {sizeless{o}, same, false}, {sizeless{o}, other, true}} {
		if got := snapshotDiffers(ctx, tc.o, tc.local); got != tc.want {
			t.Errorf("snapshotDiffers(%T, %s) = %v, want %v", tc.o, filepath.Base(tc.local), got, tc.want)
		}
	}
}
