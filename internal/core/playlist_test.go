package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stamler/medialayer/internal/index"
)

// A playlist change makes the next sync publish a new version, to the
// primary and the secondary; the playlist survives indexing, including a
// removed file; and a second device restores it.
func TestPlaylistsTravelWithTheLibrary(t *testing.T) {
	ctx := context.Background()
	c, lib, sec := newMirrorCore(t)
	if err := c.sync(ctx); err != nil {
		t.Fatal(err)
	}
	ids := fileIDs(t, c)
	one, two, three := ids["A/Album/01.flac"], ids["A/Album/02.flac"], ids["B/Other/01.flac"]

	pl, err := c.CreatePlaylist("Mix")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.AddToPlaylist(pl, []string{three, one, two}, -1); err != nil {
		t.Fatal(err)
	}
	if err := c.sync(ctx); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{lib, sec} {
		snap := filepath.Join(dir, ".medialayer/snapshots/000002.sqlite")
		sdb, err := index.Open(snap)
		if err != nil {
			t.Fatalf("a playlist change published no new version: %v", err)
		}
		got, err := sdb.PlaylistItems(pl)
		_ = sdb.Close()
		if err != nil || !slices.Equal(got, []string{three, one, two}) {
			t.Errorf("%s: playlist items = %v (%v)", snap, got, err)
		}
	}

	// An unchanged sync publishes nothing new.
	if err := c.sync(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(lib, ".medialayer/snapshots/000003.sqlite")); err == nil {
		t.Error("an unchanged library made a new snapshot")
	}

	// A retagged file keeps its id; a removed one stays in the playlist,
	// missing, and comes back with its file.
	removed := filepath.Join(lib, "B/Other/01.flac")
	saved, err := os.ReadFile(removed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(removed); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(lib, "A/Album/02.flac"), testFLAC([]string{"TITLE=Two (edit)"}, nil, 1000))
	if err := c.sync(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := c.currentDB().PlaylistItems(pl)
	if err != nil || !slices.Equal(got, []string{three, one, two}) {
		t.Errorf("after re-indexing: items = %v (%v)", got, err)
	}
	var missing bool
	if err := dbQueryRow(c.currentDB(), `SELECT missing FROM file WHERE id = '`+three+`'`, &missing); err != nil || !missing {
		t.Errorf("removed file: missing = %v (%v)", missing, err)
	}
	// The removed item can still be moved and removed by position.
	if err := c.MovePlaylistItems(pl, []int{0}, -1); err != nil {
		t.Fatal(err)
	}
	write(t, removed, saved)
	if err := c.sync(ctx); err != nil {
		t.Fatal(err)
	}

	// A second device gets the playlist from the snapshot.
	c2 := newTestCore(t, lib)
	if err := c2.sync(ctx); err != nil {
		t.Fatal(err)
	}
	got, err = c2.currentDB().PlaylistItems(pl)
	if err != nil || !slices.Equal(got, []string{one, two, three}) {
		t.Errorf("second device: items = %v (%v)", got, err)
	}
}

// A playlist made while indexing from scratch carries over when another
// device published the library first and its snapshot replaces the index,
// including an item whose file the snapshot doesn't list yet.
func TestPlaylistCarriesOverToRestoredIndex(t *testing.T) {
	ctx := context.Background()
	lib := writeLibrary(t, nil)
	late := filepath.Join(lib, "C/Late/01.flac")
	lateData := testFLAC([]string{"TITLE=Late"}, nil, 1000)
	write(t, late, lateData)
	a, b := newTestCore(t, lib), newTestCore(t, lib)
	if err := b.index(ctx, b.fsys); err != nil {
		t.Fatal(err)
	}
	bIDs := fileIDs(t, b)
	pl, err := b.CreatePlaylist("Early")
	if err != nil {
		t.Fatal(err)
	}
	if err := b.AddToPlaylist(pl, []string{bIDs["B/Other/01.flac"], bIDs["C/Late/01.flac"], bIDs["A/Album/01.flac"]}, -1); err != nil {
		t.Fatal(err)
	}
	// A publishes while the late file is away, so its snapshot lacks it.
	if err := os.Remove(late); err != nil {
		t.Fatal(err)
	}
	if err := a.sync(ctx); err != nil {
		t.Fatal(err)
	}
	write(t, late, lateData)
	if err := b.sync(ctx); err != nil {
		t.Fatal(err)
	}
	ids := fileIDs(t, b)
	if bIDs["A/Album/01.flac"] == ids["A/Album/01.flac"] {
		t.Fatal("the test needs B to index with its own file ids")
	}
	got, err := b.currentDB().PlaylistItems(pl)
	want := []string{ids["B/Other/01.flac"], ids["C/Late/01.flac"], ids["A/Album/01.flac"]}
	if err != nil || !slices.Equal(got, want) {
		t.Errorf("carried-over items = %v (%v), want %v", got, err, want)
	}
	if _, err := os.Stat(b.carriedPath()); err == nil {
		t.Error("the carried playlists are still waiting after the sync")
	}
	// And B published it as a new version.
	sdb, err := index.Open(filepath.Join(lib, ".medialayer/snapshots/000002.sqlite"))
	if err != nil {
		t.Fatalf("no new version with the carried-over playlist: %v", err)
	}
	defer func() { _ = sdb.Close() }()
	if lists, _ := sdb.Playlists(); len(lists) != 1 || lists[0].ID != pl {
		t.Errorf("published playlists = %+v", lists)
	}
}

func TestPlaylistNeedsALibrary(t *testing.T) {
	c, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.CreatePlaylist("x"); err == nil {
		t.Error("made a playlist with no library open")
	}
}

// Picking another folder would delete the local index, so it waits while
// playlist changes are unsaved.
func TestSetFolderKeepsUnsavedPlaylists(t *testing.T) {
	ctx := context.Background()
	c := newTestCore(t, writeLibrary(t, nil))
	if err := c.sync(ctx); err != nil {
		t.Fatal(err)
	}
	pl, err := c.CreatePlaylist("Mine")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SetFolder("Other"); !errors.Is(err, errUnsavedPlaylists) {
		t.Fatalf("SetFolder with an unsaved playlist: %v", err)
	}
	if lists, _ := c.currentDB().Playlists(); len(lists) != 1 {
		t.Fatalf("playlists after a refused SetFolder: %+v", lists)
	}
	if err := c.sync(ctx); err != nil {
		t.Fatal(err)
	}
	if unsaved, err := unsavedPlaylists(c.currentDB()); err != nil || unsaved {
		t.Fatalf("after a sync: unsaved = %v (%v)", unsaved, err)
	}
	// A file change alone doesn't hold the folder change up.
	if err := c.currentDB().MarkFilesChanged(); err != nil {
		t.Fatal(err)
	}
	if unsaved, _ := unsavedPlaylists(c.currentDB()); unsaved {
		t.Error("a file change counted as an unsaved playlist change")
	}
	// Neither does a playlist change once there are no playlists left.
	if err := c.DeletePlaylist(pl); err != nil {
		t.Fatal(err)
	}
	if unsaved, _ := unsavedPlaylists(c.currentDB()); unsaved {
		t.Error("no playlists, but SetFolder would refuse")
	}
}

// A moved or renamed file keeps its id, so it stays in its playlists.
func TestPlaylistKeepsMovedFiles(t *testing.T) {
	ctx := context.Background()
	lib := writeLibrary(t, nil)
	c := newTestCore(t, lib)
	if err := c.sync(ctx); err != nil {
		t.Fatal(err)
	}
	before := fileIDs(t, c)
	pl, err := c.CreatePlaylist("P")
	if err != nil {
		t.Fatal(err)
	}
	items := []string{before["A/Album/01.flac"], before["B/Other/01.flac"]}
	if err := c.AddToPlaylist(pl, items, -1); err != nil {
		t.Fatal(err)
	}
	for from, to := range map[string]string{
		"A/Album/01.flac": "A/Album/01 One.flac", // renamed
		"B/Other/01.flac": "Moved/01.flac",       // moved
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(lib, to)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(filepath.Join(lib, from), filepath.Join(lib, to)); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.sync(ctx); err != nil {
		t.Fatal(err)
	}
	after := fileIDs(t, c)
	if after["A/Album/01 One.flac"] != items[0] || after["Moved/01.flac"] != items[1] {
		t.Errorf("ids after the moves: %v, want %v kept", after, items)
	}
	if _, ok := after["A/Album/01.flac"]; ok {
		t.Error("the old path is still indexed")
	}
	if n := c.Status().Tracks; n != 3 {
		t.Errorf("tracks = %d, want 3", n)
	}
	got, err := c.currentDB().PlaylistItems(pl)
	if err != nil || !slices.Equal(got, items) {
		t.Errorf("playlist items = %v (%v), want %v", got, err, items)
	}
	// Two copies of one file can't both be the moved one: neither takes
	// the old id.
	data, err := os.ReadFile(filepath.Join(lib, "A/Album/02.flac"))
	if err != nil {
		t.Fatal(err)
	}
	old := after["A/Album/02.flac"]
	if err := os.Remove(filepath.Join(lib, "A/Album/02.flac")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(lib, "X/02.flac"), data)
	write(t, filepath.Join(lib, "Y/02.flac"), data)
	if err := c.sync(ctx); err != nil {
		t.Fatal(err)
	}
	ids := fileIDs(t, c)
	if ids["X/02.flac"] == old || ids["Y/02.flac"] == old {
		t.Errorf("an ambiguous copy took the old id: %v", ids)
	}
}

// Hidden items whose file is missing can be removed.
func TestRemoveMissingFromPlaylist(t *testing.T) {
	ctx := context.Background()
	lib := writeLibrary(t, nil)
	c := newTestCore(t, lib)
	if err := c.sync(ctx); err != nil {
		t.Fatal(err)
	}
	ids := fileIDs(t, c)
	pl, _ := c.CreatePlaylist("P")
	items := []string{ids["A/Album/01.flac"], ids["B/Other/01.flac"], ids["A/Album/02.flac"]}
	if err := c.AddToPlaylist(pl, items, -1); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(lib, "B/Other/01.flac")); err != nil {
		t.Fatal(err)
	}
	if err := c.sync(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveMissingFromPlaylist(pl); err != nil {
		t.Fatal(err)
	}
	got, err := c.currentDB().PlaylistItems(pl)
	if want := []string{items[0], items[2]}; err != nil || !slices.Equal(got, want) {
		t.Errorf("items = %v (%v), want %v", got, err, want)
	}
}
