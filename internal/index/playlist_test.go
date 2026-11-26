package index

import (
	"errors"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stamler/medialayer/internal/media"
)

// testDB is an index with files a.flac, b.flac, ... by path, returning
// their ids in that order.
func testDB(t *testing.T, names ...string) (*DB, []string) {
	t.Helper()
	d, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	var ids []string
	for _, n := range names {
		id, err := d.PutFile(File{Path: n, Size: 1, MTime: 1, Info: &media.Info{Format: "flac"}})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return d, ids
}

func items(t *testing.T, d *DB, id string) []string {
	t.Helper()
	got, err := d.PlaylistItems(id)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func clearDirty(t *testing.T, d *DB) {
	t.Helper()
	if err := d.SetMeta("dirty", "0"); err != nil {
		t.Fatal(err)
	}
}

func wantDirty(t *testing.T, d *DB, want bool, after string) {
	t.Helper()
	v, err := d.Meta("dirty")
	if err != nil {
		t.Fatal(err)
	}
	if (v == "1") != want {
		t.Errorf("dirty = %q after %s, want %v", v, after, want)
	}
}

func TestPlaylistEdits(t *testing.T) {
	d, f := testDB(t, "a.flac", "b.flac", "c.flac", "d.flac")
	a, b, c, dd := f[0], f[1], f[2], f[3]

	clearDirty(t, d)
	id, err := d.CreatePlaylist("  Road trip ")
	if err != nil {
		t.Fatal(err)
	}
	wantDirty(t, d, true, "create")
	if lists, _ := d.Playlists(); len(lists) != 1 || lists[0].Name != "Road trip" {
		t.Errorf("playlists = %+v", lists)
	}
	if _, err := d.CreatePlaylist(" "); err == nil {
		t.Error("made a playlist with no name")
	}

	clearDirty(t, d)
	if err := d.AddToPlaylist(id, []string{a, b}, -1); err != nil {
		t.Fatal(err)
	}
	wantDirty(t, d, true, "add")
	if err := d.AddToPlaylist(id, []string{c}, 1); err != nil {
		t.Fatal(err)
	}
	if err := d.AddToPlaylist(id, []string{dd}, 99); err != nil {
		t.Fatal(err)
	}
	if got := items(t, d, id); !slices.Equal(got, []string{a, c, b, dd}) {
		t.Fatalf("items = %v", got)
	}
	if err := d.AddToPlaylist(id, []string{a, "nope"}, -1); err == nil {
		t.Error("added an unknown file")
	}
	if got := items(t, d, id); len(got) != 4 {
		t.Errorf("a failed add changed the playlist: %v", got)
	}

	// Moves follow Swift's move(fromOffsets:toOffset:).
	for _, m := range []struct {
		positions []int
		before    int
		want      []string
	}{
		{[]int{0}, 2, []string{c, a, b, dd}},
		{[]int{3}, 0, []string{dd, c, a, b}},
		{[]int{0, 2}, -1, []string{c, b, dd, a}},
		{[]int{1, 2}, 1, []string{c, b, dd, a}}, // onto itself: unchanged
		{[]int{2, 0}, 4, []string{b, a, c, dd}}, // order kept
	} {
		if err := d.MovePlaylistItems(id, m.positions, m.before); err != nil {
			t.Fatal(err)
		}
		if got := items(t, d, id); !slices.Equal(got, m.want) {
			t.Fatalf("move %v before %d: items = %v, want %v", m.positions, m.before, got, m.want)
		}
	}
	if err := d.MovePlaylistItems(id, []int{4}, 0); err == nil {
		t.Error("moved a position past the end")
	}

	clearDirty(t, d)
	if err := d.MovePlaylistItems(id, []int{1}, 2); err != nil { // a no-op
		t.Fatal(err)
	}
	wantDirty(t, d, false, "a move that changes nothing")

	if err := d.RemoveFromPlaylist(id, []int{0, 3}); err != nil {
		t.Fatal(err)
	}
	if got := items(t, d, id); !slices.Equal(got, []string{a, c}) {
		t.Fatalf("after remove: items = %v", got)
	}
	// Positions stay dense.
	var maxPos int
	if err := d.SQL().QueryRow(`SELECT max(pos) FROM playlist_item WHERE playlist_id = ?`, id).Scan(&maxPos); err != nil || maxPos != 1 {
		t.Errorf("max pos = %d (%v), want 1", maxPos, err)
	}

	clearDirty(t, d)
	if err := d.RenamePlaylist(id, "Commute"); err != nil {
		t.Fatal(err)
	}
	wantDirty(t, d, true, "rename")
	clearDirty(t, d)
	if err := d.RenamePlaylist(id, "Commute"); err != nil {
		t.Fatal(err)
	}
	wantDirty(t, d, false, "a rename to the same name")

	if err := d.DeletePlaylist(id); err != nil {
		t.Fatal(err)
	}
	wantDirty(t, d, true, "delete")
	if _, err := d.PlaylistItems(id); !errors.Is(err, ErrNoPlaylist) {
		t.Errorf("items of a deleted playlist: %v", err)
	}
	var n int
	if err := d.SQL().QueryRow(`SELECT count(*) FROM playlist_item`).Scan(&n); err != nil || n != 0 {
		t.Errorf("%d items left after delete (%v)", n, err)
	}
	if n, _ := d.TrackCount(); n != 4 {
		t.Errorf("deleting a playlist removed tracks: %d left", n)
	}
	for _, err := range []error{
		d.RenamePlaylist(id, "x"), d.DeletePlaylist(id), d.AddToPlaylist(id, []string{a}, -1),
		d.RemoveFromPlaylist(id, nil), d.MovePlaylistItems(id, nil, 0),
	} {
		if !errors.Is(err, ErrNoPlaylist) {
			t.Errorf("edit of a deleted playlist: %v", err)
		}
	}
}

// Items keep their file when it goes missing and is indexed again.
func TestPlaylistSurvivesReindexing(t *testing.T) {
	d, f := testDB(t, "a.flac", "b.flac")
	id, err := d.CreatePlaylist("P")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.AddToPlaylist(id, f, -1); err != nil {
		t.Fatal(err)
	}
	if err := d.SetMissing(f[:1], true); err != nil {
		t.Fatal(err)
	}
	if _, err := d.PutFile(File{Path: "b.flac", Size: 2, MTime: 2, Info: &media.Info{Format: "flac"}}); err != nil {
		t.Fatal(err)
	}
	if got := items(t, d, id); !slices.Equal(got, f) {
		t.Errorf("items = %v, want %v", got, f)
	}
}

func TestCopyPlaylists(t *testing.T) {
	src, sf := testDB(t, "a.flac", "b.flac", "c.flac")
	dst, df := testDB(t, "c.flac", "a.flac") // other ids; no b.flac
	if n, err := src.CopyPlaylists(dst); n != 0 || err != nil {
		t.Fatalf("copied %d (%v) with no playlists", n, err)
	}
	p1, _ := src.CreatePlaylist("One")
	if err := src.AddToPlaylist(p1, []string{sf[1], sf[2], sf[0]}, -1); err != nil {
		t.Fatal(err)
	}
	p2, _ := src.CreatePlaylist("Two")
	if err := src.AddToPlaylist(p2, []string{sf[0]}, -1); err != nil {
		t.Fatal(err)
	}
	// dst already has p2 (it was in the snapshot): it stays as it is.
	if _, err := dst.SQL().Exec(`INSERT INTO playlist (id, name, updated_at) VALUES (?, 'Two', 1)`, p2); err != nil {
		t.Fatal(err)
	}
	clearDirty(t, dst)
	n, err := src.CopyPlaylists(dst)
	if err != nil || n != 1 {
		t.Fatalf("copied %d (%v), want 1", n, err)
	}
	wantDirty(t, dst, true, "copying playlists")
	if got := items(t, dst, p1); !slices.Equal(got, []string{df[0], df[1]}) {
		t.Errorf("copied items = %v, want c then a (%v)", got, df)
	}
	if got := items(t, dst, p2); len(got) != 0 {
		t.Errorf("a playlist dst had was changed: %v", got)
	}
	lists, _ := dst.Playlists()
	if len(lists) != 2 {
		t.Errorf("dst playlists = %+v", lists)
	}
}
