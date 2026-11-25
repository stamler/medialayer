package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newMirrorCore is a test core with a primary library of 64 files (so one
// change stays under the 5% limit) and one empty local secondary.
func newMirrorCore(t *testing.T) (c *Core, lib, sec string) {
	t.Helper()
	lib = writeLibrary(t, nil)
	for i := range 60 {
		write(t, filepath.Join(lib, fmt.Sprintf("Extras/%02d.txt", i)), []byte(fmt.Sprint("extra ", i)))
	}
	sec = filepath.Join(t.TempDir(), "Backup")
	c = newTestCore(t, lib)
	c.mu.Lock()
	c.settings.Secondaries = []source{{ID: "sec1", Folder: sec}}
	c.mu.Unlock()
	return c, lib, sec
}

func write(t *testing.T, p string, b []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func mirrorOf(t *testing.T, c *Core, id string) SourceStatus {
	t.Helper()
	for _, s := range c.Status().Sources {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("no source %s", id)
	return SourceStatus{}
}

// trashed lists the paths under the secondary's trash, without the
// per-sync time folder.
func trashed(t *testing.T, sec string) []string {
	t.Helper()
	var out []string
	root := filepath.Join(sec, ".medialayer", "trash")
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			rel, _ := filepath.Rel(root, p)
			_, rest, _ := strings.Cut(filepath.ToSlash(rel), "/")
			out = append(out, rest)
		}
		return nil
	})
	return out
}

func TestMirrorCopiesLibraryAndRepository(t *testing.T) {
	ctx := context.Background()
	c, lib, sec := newMirrorCore(t)
	if err := c.sync(ctx); err != nil {
		t.Fatal(err)
	}
	if s := mirrorOf(t, c, "sec1"); s.State != MirrorSynced || s.Copies != 64 || s.Error != "" {
		t.Fatalf("secondary = %+v, want synced with 64 copies", s)
	}
	for _, p := range []string{"A/Album/01.flac", "notes.txt", "Extras/59.txt", ".medialayer/repo.json", ".medialayer/snapshots/000001.sqlite"} {
		want, _ := os.ReadFile(filepath.Join(lib, p))
		got, err := os.ReadFile(filepath.Join(sec, p))
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s not copied (%v)", p, err)
		}
	}
	var repo repoRecord
	b, _ := os.ReadFile(filepath.Join(sec, ".medialayer/repo.json"))
	if err := json.Unmarshal(b, &repo); err != nil {
		t.Fatal(err)
	}
	if len(repo.Members) != 2 || repo.Members[1].Replica != "sec1" || repo.Members[1].Type != "local" {
		t.Errorf("members = %+v", repo.Members)
	}
	if repo.Primary.Replica != replica {
		t.Errorf("primary = %+v", repo.Primary)
	}

	// Nothing changed: nothing copied.
	if err := c.sync(ctx); err != nil {
		t.Fatal(err)
	}
	if s := mirrorOf(t, c, "sec1"); s.State != MirrorSynced || s.Copies+s.Replaces+s.Deletes != 0 {
		t.Errorf("unchanged sync = %+v", s)
	}
}

func TestMirrorTrashesRemovedAndEditedFiles(t *testing.T) {
	ctx := context.Background()
	c, lib, sec := newMirrorCore(t)
	if err := c.sync(ctx); err != nil {
		t.Fatal(err)
	}
	// Removed on the primary; edited on the secondary directly.
	if err := os.Remove(filepath.Join(lib, "notes.txt")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(sec, "Extras/01.txt"), []byte("edited on the secondary"))
	if err := c.sync(ctx); err != nil {
		t.Fatal(err)
	}
	if s := mirrorOf(t, c, "sec1"); s.State != MirrorSynced || s.Deletes != 1 || s.Replaces != 1 {
		t.Fatalf("secondary = %+v, want 1 delete and 1 replace", s)
	}
	if _, err := os.Stat(filepath.Join(sec, "notes.txt")); err == nil {
		t.Error("notes.txt is still on the secondary")
	}
	if b, _ := os.ReadFile(filepath.Join(sec, "Extras/01.txt")); string(b) != "extra 1" {
		t.Errorf("Extras/01.txt = %q, want the primary's copy", b)
	}
	got := strings.Join(trashed(t, sec), ",")
	if got != "Extras/01.txt,notes.txt" {
		t.Errorf("trash holds %q", got)
	}
	b, _ := os.ReadFile(filepath.Join(sec, ".medialayer/trash", firstDir(t, filepath.Join(sec, ".medialayer/trash")), "Extras/01.txt"))
	if string(b) != "edited on the secondary" {
		t.Errorf("trash has %q, want the secondary's edit", b)
	}
}

func firstDir(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("no folders in %s (%v)", dir, err)
	}
	return entries[0].Name()
}

func TestMirrorPausesOnMassChange(t *testing.T) {
	ctx := context.Background()
	c, lib, sec := newMirrorCore(t)
	if err := c.sync(ctx); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(lib, "Extras")); err != nil {
		t.Fatal(err)
	}
	if err := c.sync(ctx); err != nil {
		t.Fatal(err)
	}
	s := mirrorOf(t, c, "sec1")
	if s.State != MirrorPaused || s.Deletes != 60 {
		t.Fatalf("secondary = %+v, want paused with 60 deletes", s)
	}
	if _, err := os.Stat(filepath.Join(sec, "Extras/00.txt")); err != nil {
		t.Error("a paused sync removed files")
	}
	// A sync without approval stays paused.
	if err := c.sync(ctx); err != nil {
		t.Fatal(err)
	}
	if s := mirrorOf(t, c, "sec1"); s.State != MirrorPaused {
		t.Fatalf("secondary = %+v, want still paused", s)
	}
	c.mu.Lock()
	c.mirrors["sec1"].approved = 60 // what ApproveChanges sets, without starting a job
	c.mu.Unlock()
	if err := c.sync(ctx); err != nil {
		t.Fatal(err)
	}
	if s := mirrorOf(t, c, "sec1"); s.State != MirrorSynced || s.Deletes != 60 {
		t.Fatalf("secondary = %+v, want synced with 60 deletes", s)
	}
	if n := len(trashed(t, sec)); n != 60 {
		t.Errorf("trash holds %d files, want 60", n)
	}
}

func TestMirrorRefusesOtherLibraryAndNewerGeneration(t *testing.T) {
	ctx := context.Background()
	for name, repo := range map[string]repoRecord{
		"other library":    {Format: repoFormat, LibraryID: "someone-else", Generation: 1},
		"newer generation": {Format: repoFormat, Generation: 2}, // library id filled in below
	} {
		t.Run(name, func(t *testing.T) {
			c, lib, sec := newMirrorCore(t)
			c.mu.Lock()
			c.settings.Secondaries = nil
			c.mu.Unlock()
			if err := c.sync(ctx); err != nil {
				t.Fatal(err)
			}
			primary, err := readRepo(ctx, c.fsys)
			if err != nil {
				t.Fatal(err)
			}
			if repo.LibraryID == "" {
				repo.LibraryID = primary.LibraryID
			}
			b, _ := json.Marshal(repo)
			write(t, filepath.Join(sec, ".medialayer/repo.json"), b)
			write(t, filepath.Join(sec, "mine.txt"), []byte("keep me"))
			c.mu.Lock()
			c.settings.Secondaries = []source{{ID: "sec1", Folder: sec}}
			c.mu.Unlock()
			if err := c.sync(ctx); err != nil {
				t.Fatal(err)
			}
			if s := mirrorOf(t, c, "sec1"); s.State != MirrorError || s.Error == "" {
				t.Errorf("secondary = %+v, want an error", s)
			}
			if _, err := os.Stat(filepath.Join(sec, "mine.txt")); err != nil {
				t.Error("the secondary was changed")
			}
			if _, err := os.Stat(filepath.Join(sec, "notes.txt")); err == nil {
				t.Error("files were copied to the secondary")
			}
			_ = lib
		})
	}
}

func TestEmptyTrashAfter30Days(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	old := now.Add(-31 * 24 * time.Hour).Format(trashStamp)
	recent := now.Add(-29 * 24 * time.Hour).Format(trashStamp)
	for _, d := range []string{old, recent, "not-a-time"} {
		write(t, filepath.Join(dir, ".medialayer/trash", d, "a/b.flac"), []byte("x"))
	}
	f, err := openSource(ctx, source{Folder: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := emptyTrash(ctx, f, now); err != nil {
		t.Fatal(err)
	}
	for d, want := range map[string]bool{old: false, recent: true, "not-a-time": true} {
		_, err := os.Stat(filepath.Join(dir, ".medialayer/trash", d))
		if (err == nil) != want {
			t.Errorf("%s kept = %v, want %v", d, err == nil, want)
		}
	}
}

func TestAddSecondaryRefusesOverlaps(t *testing.T) {
	c, _, sec := newMirrorCore(t)
	c.mu.Lock()
	c.settings.Folder = "Music"
	c.mu.Unlock()
	for _, tc := range []struct{ remote, folder string }{
		{"", "relative/path"},
		{"", sec},             // already a secondary
		{"", sec + "/Inside"}, // inside one
		{"nosuch", "Backup"},  // not connected
		{remoteName, ""},      // top of the drive
	} {
		if err := c.AddSecondary(tc.remote, tc.folder); err == nil {
			t.Errorf("AddSecondary(%q, %q) was allowed", tc.remote, tc.folder)
		}
	}
	for _, tc := range [][2]string{{"Music", "Music"}, {"Music/Backup", "Music"}, {"Music", "Music/Backup"}} {
		if !overlaps(tc[0], tc[1]) {
			t.Errorf("overlaps(%q, %q) = false", tc[0], tc[1])
		}
	}
	if overlaps("Music", "Music Backup") {
		t.Error(`"Music" and "Music Backup" overlap`)
	}
}
