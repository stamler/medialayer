package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stamler/medialayer/internal/index"
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
	var m memberRecord
	b, _ = os.ReadFile(filepath.Join(sec, ".medialayer/member.json"))
	if err := json.Unmarshal(b, &m); err != nil || m.State != "synced" || m.Snapshot != "000001.sqlite" || m.LibraryID != repo.LibraryID {
		t.Errorf("member.json = %s (%v)", b, err)
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
	if b, _ := os.ReadFile(filepath.Join(sec, ".medialayer/member.json")); !strings.Contains(string(b), `"state":"paused"`) {
		t.Errorf("member.json = %s, want paused", b)
	}
	// A sync without approval stays paused.
	if err := c.sync(ctx); err != nil {
		t.Fatal(err)
	}
	if s := mirrorOf(t, c, "sec1"); s.State != MirrorPaused {
		t.Fatalf("secondary = %+v, want still paused", s)
	}
	c.mu.Lock()
	c.mirrors["sec1"].approved = "not this plan"
	c.mu.Unlock()
	if err := c.sync(ctx); err != nil {
		t.Fatal(err)
	}
	if s := mirrorOf(t, c, "sec1"); s.State != MirrorPaused {
		t.Fatalf("an approval of another plan went ahead: %+v", s)
	}
	c.mu.Lock()
	c.mirrors["sec1"].approved = c.mirrors["sec1"].pending // what ApproveChanges sets, without starting a job
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
		{remoteName, "Music/.."},
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

func TestMirrorRefusesUnsafeFolders(t *testing.T) {
	ctx := context.Background()
	for name, setup := range map[string]func(lib, sec string) string{
		"not empty, not a library": func(lib, sec string) string {
			write(t, filepath.Join(sec, "taxes/2025.pdf"), []byte("keep me"))
			return sec
		},
		"the primary itself": func(lib, sec string) string { return lib },
		"inside the primary": func(lib, sec string) string { return filepath.Join(lib, "Backup") },
		"holds the primary":  func(lib, sec string) string { return filepath.Dir(lib) },
	} {
		t.Run(name, func(t *testing.T) {
			c, lib, sec := newMirrorCore(t)
			folder := setup(lib, sec)
			c.mu.Lock()
			c.settings.Secondaries = []source{{ID: "sec1", Folder: folder}}
			c.mu.Unlock()
			// The core's own data folder sits beside the library in tests.
			data, _ := filepath.Rel(filepath.Dir(lib), c.dataDir)
			tree := func() map[string]bool {
				m := snapshotTree(t, filepath.Dir(lib))
				for p := range m {
					if strings.HasPrefix(p, filepath.ToSlash(data)+"/") {
						delete(m, p)
					}
				}
				return m
			}
			before := tree()
			if err := c.sync(ctx); err != nil {
				t.Fatal(err)
			}
			if s := mirrorOf(t, c, "sec1"); s.State != MirrorError || s.Error == "" {
				t.Errorf("secondary = %+v, want an error", s)
			}
			// Nothing outside the primary's own .medialayer changed.
			after := tree()
			for p := range after {
				if !before[p] && !strings.Contains(p, ".medialayer/") {
					t.Errorf("%s appeared", p)
				}
			}
			for p := range before {
				if !after[p] && !strings.Contains(p, ".medialayer/") {
					t.Errorf("%s disappeared", p)
				}
			}
			if _, err := os.Stat(filepath.Join(sec, "taxes/2025.pdf")); name == "not empty, not a library" && err != nil {
				t.Error("the folder's own file was moved")
			}
		})
	}
}

// snapshotTree lists every file under dir.
func snapshotTree(t *testing.T, dir string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			out[filepath.ToSlash(rel)] = true
		}
		return nil
	})
	return out
}

func TestMirrorIgnoresSystemFiles(t *testing.T) {
	ctx := context.Background()
	c, _, sec := newMirrorCore(t)
	if err := c.sync(ctx); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(sec, ".DS_Store"), []byte("finder"))
	write(t, filepath.Join(sec, "A/._01.flac"), []byte("appledouble"))
	if err := c.sync(ctx); err != nil {
		t.Fatal(err)
	}
	if s := mirrorOf(t, c, "sec1"); s.State != MirrorSynced || s.Deletes != 0 {
		t.Errorf("secondary = %+v, want no deletes", s)
	}
	if _, err := os.Stat(filepath.Join(sec, ".DS_Store")); err != nil {
		t.Error(".DS_Store was moved")
	}
}

func TestCheckLocalFolder(t *testing.T) {
	home, _ := os.UserHomeDir()
	for _, bad := range []string{"/", "/Volumes/Music", home, filepath.Join(home, "Library/CloudStorage/GoogleDrive-x/My Drive")} {
		if checkLocalFolder(bad) == nil {
			t.Errorf("%s was allowed", bad)
		}
	}
	for _, ok := range []string{"/Volumes/Music/Backup", filepath.Join(home, "Music/Backup")} {
		if err := checkLocalFolder(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
}

func TestMirrorResumesCutShortFirstCopy(t *testing.T) {
	ctx := context.Background()
	c, _, sec := newMirrorCore(t)
	if err := c.sync(ctx); err != nil {
		t.Fatal(err)
	}
	// As if the first copy stopped before repo.json was written.
	if err := os.Remove(filepath.Join(sec, ".medialayer/repo.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(sec, "notes.txt")); err != nil {
		t.Fatal(err)
	}
	if err := c.sync(ctx); err != nil {
		t.Fatal(err)
	}
	if s := mirrorOf(t, c, "sec1"); s.State != MirrorSynced || s.Copies != 1 {
		t.Errorf("secondary = %+v, want synced with 1 copy", s)
	}
	if _, err := os.Stat(filepath.Join(sec, ".medialayer/repo.json")); err != nil {
		t.Error("repo.json was not written")
	}
}

func TestMirrorMatchesDecomposedNames(t *testing.T) {
	ctx := context.Background()
	c, lib, sec := newMirrorCore(t)
	composed, decomposed := "Extras/Caf\u00e9.txt", "Extras/Cafe\u0301.txt"
	write(t, filepath.Join(lib, composed), []byte("same"))
	if err := c.sync(ctx); err != nil {
		t.Fatal(err)
	}
	// As an HFS+ disk keeps the name.
	if err := os.Rename(filepath.Join(sec, composed), filepath.Join(sec, decomposed)); err != nil {
		t.Fatal(err)
	}
	if err := c.sync(ctx); err != nil {
		t.Fatal(err)
	}
	if s := mirrorOf(t, c, "sec1"); s.State != MirrorSynced || s.Copies+s.Deletes+s.Replaces != 0 {
		t.Errorf("secondary = %+v, want nothing to do", s)
	}
}

func TestIsFull(t *testing.T) {
	for err, want := range map[error]bool{
		fmt.Errorf("googleapi: Error 403: The user's Drive storage quota has been exceeded., storageQuotaExceeded"): true,
		fmt.Errorf("write: %w", syscall.ENOSPC):                                              true,
		fmt.Errorf("googleapi: Error 403: User rate limit exceeded., userRateLimitExceeded"): false,
		nil: false,
	} {
		if got := isFull(err); got != want {
			t.Errorf("isFull(%v) = %v", err, got)
		}
	}
}

func TestMirrorCarriesEachIndexVersion(t *testing.T) {
	ctx := context.Background()
	c, lib, sec := newMirrorCore(t)
	if err := c.sync(ctx); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(lib, "C/New/01.flac"), testFLAC([]string{"TITLE=New"}, nil, 1000))
	if err := c.sync(ctx); err != nil {
		t.Fatal(err)
	}
	var m memberRecord
	b, _ := os.ReadFile(filepath.Join(sec, ".medialayer/member.json"))
	if err := json.Unmarshal(b, &m); err != nil || m.State != "synced" || m.Version != 2 || m.Snapshot != "000002.sqlite" || m.Generation != 1 {
		t.Fatalf("member.json = %s (%v)", b, err)
	}
	snaps, _ := os.ReadDir(filepath.Join(sec, ".medialayer/snapshots"))
	if len(snaps) != 1 || snaps[0].Name() != "000002.sqlite" {
		t.Errorf("secondary snapshots = %v, want only 000002.sqlite", snaps)
	}
	// The secondary's snapshot is that version of the index, with the
	// new track in it.
	sdb, err := index.Open(filepath.Join(sec, ".medialayer/snapshots/000002.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sdb.Close() }()
	if v, _ := sdb.Meta("version"); v != "2" {
		t.Errorf("snapshot version = %q", v)
	}
	if n, _ := sdb.TrackCount(); n != 4 {
		t.Errorf("snapshot has %d tracks, want 4", n)
	}
}
