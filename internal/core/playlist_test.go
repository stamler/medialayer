package core

import (
	"context"
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
// device published the library first and its snapshot replaces the index.
func TestPlaylistCarriesOverToRestoredIndex(t *testing.T) {
	ctx := context.Background()
	lib := writeLibrary(t, nil)
	a, b := newTestCore(t, lib), newTestCore(t, lib)
	if err := b.index(ctx, b.fsys); err != nil {
		t.Fatal(err)
	}
	bIDs := fileIDs(t, b)
	pl, err := b.CreatePlaylist("Early")
	if err != nil {
		t.Fatal(err)
	}
	if err := b.AddToPlaylist(pl, []string{bIDs["B/Other/01.flac"], bIDs["A/Album/01.flac"]}, -1); err != nil {
		t.Fatal(err)
	}
	if err := a.sync(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.sync(ctx); err != nil {
		t.Fatal(err)
	}
	aIDs := fileIDs(t, a)
	if bIDs["A/Album/01.flac"] == aIDs["A/Album/01.flac"] {
		t.Fatal("the test needs B to index with its own file ids")
	}
	got, err := b.currentDB().PlaylistItems(pl)
	if want := []string{aIDs["B/Other/01.flac"], aIDs["A/Album/01.flac"]}; err != nil || !slices.Equal(got, want) {
		t.Errorf("carried-over items = %v (%v), want %v", got, err, want)
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
