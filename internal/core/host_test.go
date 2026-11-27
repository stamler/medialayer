package core

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stamler/medialayer/internal/index"
	"github.com/stamler/medialayer/internal/media"
)

// writeHostLibrary is writeLibrary with one track of real audio, which
// the audio hash can decode (writeLibrary's can't: their hashes fail).
func writeHostLibrary(t *testing.T, cover []byte) string {
	t.Helper()
	lib := writeLibrary(t, cover)
	write(t, filepath.Join(lib, "C/Real/01.flac"), encodedFLAC(t, "Real", 1, 4096))
	// A device made it when the folder was picked; the helper never does.
	if err := os.Mkdir(filepath.Join(lib, repoDir), 0o755); err != nil {
		t.Fatal(err)
	}
	return lib
}

func runHostOnce(t *testing.T, lib string) {
	t.Helper()
	if err := RunHost(context.Background(), HostOptions{Folder: lib, Version: "test"}, t.Logf); err != nil {
		t.Fatal(err)
	}
}

func readHostInventory(t *testing.T, lib string) *hostInventory {
	t.Helper()
	f, err := os.Open(filepath.Join(lib, filepath.FromSlash(hostInventoryFile)))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	inv, err := decodeHostInventory(f)
	if err != nil {
		t.Fatal(err)
	}
	return inv
}

// editHostInventory changes the inventory as a wrong or old helper might
// have written it.
func editHostInventory(t *testing.T, lib string, edit func(*hostInventory)) {
	t.Helper()
	inv := readHostInventory(t, lib)
	edit(inv)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if err := json.NewEncoder(gz).Encode(inv); err != nil {
		t.Fatal(err)
	}
	_ = gz.Close()
	if err := os.WriteFile(filepath.Join(lib, filepath.FromSlash(hostInventoryFile)), buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func retitle(inv *hostInventory, p, title string) {
	for i := range inv.Files {
		if inv.Files[i].Path != p {
			continue
		}
		for j, tag := range inv.Files[i].Info.Tags {
			if tag[0] == "TITLE" {
				inv.Files[i].Info.Tags[j][1] = title
			}
		}
	}
}

func indexedTitle(t *testing.T, c *Core, p string) string {
	t.Helper()
	var title string
	if err := c.currentDB().SQL().QueryRow(`SELECT t.value FROM tag t JOIN file f ON f.id = t.file_id
		WHERE f.path = ? AND t.key = 'TITLE'`, p).Scan(&title); err != nil {
		t.Fatal(err)
	}
	return title
}

// treeFiles lists every file under dir but the helper's own.
func treeFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if !strings.HasPrefix(filepath.ToSlash(rel), hostDir+"/") {
			out[rel] = info.ModTime().String() + "/" + string(rune(info.Size()))
		}
		return nil
	})
	return out
}

func TestHostHelperWritesInventory(t *testing.T) {
	cover := bytes.Repeat([]byte("cover"), 2000)
	lib := writeHostLibrary(t, cover)
	before := treeFiles(t, lib)
	runHostOnce(t, lib)
	if after := treeFiles(t, lib); len(after) != len(before) {
		t.Errorf("the helper wrote outside %s: %v", hostDir, after)
	}
	inv := readHostInventory(t, lib)
	if len(inv.Files) != 4 || inv.ParserVersion != media.ParserVersion || inv.AudioHashVersion != index.AudioHashVersion {
		t.Fatalf("inventory = %+v", inv)
	}
	for _, r := range inv.Files {
		// writeLibrary's tracks have no real audio: their hashes fail.
		hashed := r.AudioSHA256 != "" && r.HashError == ""
		if r.Info == nil || r.Change == "" || r.Error != "" || hashed != (r.Path == "C/Real/01.flac") {
			t.Errorf("row %+v", r)
		}
	}
	if len(inv.Images) != 1 {
		t.Fatalf("images = %v, want the one cover", inv.Images)
	}
	for sha := range inv.Images {
		b, err := os.ReadFile(filepath.Join(lib, filepath.FromSlash(hostArtPath(sha))))
		if err != nil || !bytes.Equal(b, cover) {
			t.Errorf("art %s: %v", sha, err)
		}
	}
	st, err := os.Stat(filepath.Join(lib, filepath.FromSlash(hostInventoryFile)))
	if err != nil || st.Mode().Perm() != 0o644 {
		t.Errorf("inventory mode: %v %v", st.Mode(), err)
	}
	var status HostStatus
	b, _ := os.ReadFile(filepath.Join(lib, filepath.FromSlash(hostStatusFile)))
	if err := json.Unmarshal(b, &status); err != nil || status.Files != 4 || status.Hashed != 1 || status.Sequence < 1 {
		t.Errorf("status.json = %s (%v)", b, err)
	}

	// A file retagged: the next scan reads it again and keeps the others'
	// rows, hashes and all.
	p := filepath.Join(lib, "B/Other/01.flac")
	if err := os.WriteFile(p, testFLAC([]string{"TITLE=Three again", "ARTIST=B"}, nil, 50_000), 0o644); err != nil {
		t.Fatal(err)
	}
	runHostOnce(t, lib)
	inv2 := readHostInventory(t, lib)
	for i, r := range inv2.Files {
		if r.Path == "B/Other/01.flac" {
			if got := r.Info.mediaInfo().Tags[0].Value; got != "Three again" || r.HashError == "" {
				t.Errorf("retagged row: %q, %+v", got, r)
			}
		} else if r.Change != inv.Files[i].Change || r.AudioSHA256 != inv.Files[i].AudioSHA256 || r.HashError != inv.Files[i].HashError {
			t.Errorf("unchanged row %s changed: %+v", r.Path, r)
		}
	}
	if inv2.Helper != inv.Helper || inv2.Sequence <= inv.Sequence {
		t.Errorf("helper %s seq %d, then %s seq %d", inv.Helper, inv.Sequence, inv2.Helper, inv2.Sequence)
	}
}

func TestIndexTakesTagsAndArtFromHelper(t *testing.T) {
	ctx := context.Background()
	cover := bytes.Repeat([]byte("cover"), 2000)
	lib := writeHostLibrary(t, cover)
	runHostOnce(t, lib)
	// A title only the inventory has shows its rows were used.
	editHostInventory(t, lib, func(inv *hostInventory) { retitle(inv, "A/Album/01.flac", "From the helper") })
	c := newTestCore(t, lib)
	if err := c.sync(ctx); err != nil {
		t.Fatal(err)
	}
	if got := indexedTitle(t, c, "A/Album/01.flac"); got != "From the helper" {
		t.Errorf("title = %q, want the helper's", got)
	}
	var arts, links int
	_ = dbQueryRow(c.currentDB(), `SELECT count(*) FROM art`, &arts)
	_ = dbQueryRow(c.currentDB(), `SELECT count(*) FROM file_art`, &links)
	if arts != 1 || links != 2 {
		t.Errorf("art rows = %d, file_art rows = %d, want 1 and 2", arts, links)
	}
	if st := c.Status(); st.Tracks != 4 || st.Job.Failed != 0 {
		t.Errorf("tracks = %d, failed = %v", st.Tracks, st.Job.Failures)
	}
}

func TestIndexIgnoresHelperRowsThatDontFit(t *testing.T) {
	ctx := context.Background()
	cover := bytes.Repeat([]byte("cover"), 2000)
	for name, edit := range map[string]func(t *testing.T, lib string, inv *hostInventory){
		"another parser": func(t *testing.T, lib string, inv *hostInventory) { inv.ParserVersion = "0" },
		"another time": func(t *testing.T, lib string, inv *hostInventory) {
			// The file changed since the helper's row: the helper is behind.
			later := time.Now().Add(time.Hour)
			if err := os.Chtimes(filepath.Join(lib, "A/Album/01.flac"), later, later); err != nil {
				t.Fatal(err)
			}
		},
		"art that isn't what its name says": func(t *testing.T, lib string, inv *hostInventory) {
			for sha := range inv.Images {
				if err := os.WriteFile(filepath.Join(lib, filepath.FromSlash(hostArtPath(sha))), []byte("not the cover"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
		},
		"another format": func(t *testing.T, lib string, inv *hostInventory) { inv.Format = 99 },
	} {
		t.Run(name, func(t *testing.T) {
			lib := writeHostLibrary(t, cover)
			runHostOnce(t, lib)
			editHostInventory(t, lib, func(inv *hostInventory) {
				retitle(inv, "A/Album/01.flac", "From the helper")
				edit(t, lib, inv)
			})
			c := newTestCore(t, lib)
			if err := c.sync(ctx); err != nil {
				t.Fatal(err)
			}
			if got := indexedTitle(t, c, "A/Album/01.flac"); got != "One" {
				t.Errorf("title = %q, want the file's own", got)
			}
			if st := c.Status(); st.Tracks != 4 || st.Job.Failed != 0 {
				t.Errorf("tracks = %d, failed = %v", st.Tracks, st.Job.Failures)
			}
		})
	}
}

func TestHashPassTakesHelperHashesIntoIndexOnly(t *testing.T) {
	ctx := context.Background()
	lib := writeHostLibrary(t, nil)
	write(t, filepath.Join(lib, "C/Real/02.flac"), encodedFLAC(t, "Real two", 2, 4096))
	runHostOnce(t, lib)
	fake := bytes.Repeat([]byte{0xab}, 32)
	var right string
	editHostInventory(t, lib, func(inv *hostInventory) {
		for i := range inv.Files {
			if inv.Files[i].Path == "C/Real/02.flac" {
				right = inv.Files[i].AudioSHA256
				inv.Files[i].AudioSHA256 = hex.EncodeToString(fake)
			}
		}
	})
	c := newTestCore(t, lib)
	c.hashFileHook = nil
	if err := c.index(ctx, c.fsys); err != nil {
		t.Fatal(err)
	}
	db := c.currentDB()
	if err := c.hashPass(ctx, db); err != nil {
		t.Fatal(err)
	}
	var got []byte
	if err := dbQueryRow(db, `SELECT audio_sha256 FROM file WHERE path = 'C/Real/02.flac'`, &got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, fake) {
		t.Errorf("audio_sha256 = %x, want the helper's %x", got, fake)
	}
	// The other files' hashes are the helper's too, and right: the same
	// as this device makes.
	var other []byte
	_ = dbQueryRow(db, `SELECT audio_sha256 FROM file WHERE path = 'C/Real/01.flac'`, &other)
	o, _ := c.fsys.NewObject(ctx, "C/Real/01.flac")
	own, err := hashAudio(ctx, o)
	if err != nil || !bytes.Equal(other, own) {
		t.Errorf("helper's hash %x, this device's %x (%v)", other, own, err)
	}
	// Nothing of the helper's went into audio-hashes.json, which decides
	// tag edits.
	b, _ := os.ReadFile(filepath.Join(c.dataDir, audioHashFile))
	if strings.Contains(string(b), hex.EncodeToString(fake)) || strings.Contains(string(b), right) {
		t.Errorf("audio-hashes.json has the helper's hashes: %s", b)
	}
}

// The helper writes nowhere but .medialayer/host: not before a device
// made .medialayer, and not through a link out of it.
func TestHostHelperStaysInItsFolder(t *testing.T) {
	ctx := context.Background()
	lib := writeLibrary(t, nil)
	err := RunHost(ctx, HostOptions{Folder: lib}, t.Logf)
	if !errors.Is(err, ErrNoRepo) {
		t.Errorf("without .medialayer: %v", err)
	}
	if _, err := os.Stat(filepath.Join(lib, repoDir)); err == nil {
		t.Error("the helper made .medialayer")
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(lib, repoDir)); err != nil {
		t.Fatal(err)
	}
	if err := RunHost(ctx, HostOptions{Folder: lib}, t.Logf); err == nil {
		t.Error("a linked .medialayer was used")
	}
	_ = os.Remove(filepath.Join(lib, repoDir))
	if err := os.MkdirAll(filepath.Join(lib, repoDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(lib, repoDir, "host")); err != nil {
		t.Fatal(err)
	}
	if err := RunHost(ctx, HostOptions{Folder: lib}, t.Logf); err == nil {
		t.Error("a linked host was used")
	}
	_ = os.Remove(filepath.Join(lib, repoDir, "host"))
	// art linked out of host: refused too.
	if err := os.MkdirAll(filepath.Join(lib, repoDir, "host"), 0o700); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(outside, "zz", "precious.txt")
	write(t, keep, []byte("mine"))
	if err := os.Symlink(outside, filepath.Join(lib, repoDir, "host", "art")); err != nil {
		t.Fatal(err)
	}
	if err := RunHost(ctx, HostOptions{Folder: lib}, t.Logf); err == nil {
		t.Error("a linked art was used")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("a file outside was removed: %v", err)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 1 {
		t.Errorf("wrote outside: %v", entries)
	}
	// Folders it makes are readable by a share's other users.
	_ = os.Remove(filepath.Join(lib, repoDir, "host", "art"))
	if err := RunHost(ctx, HostOptions{Folder: lib}, t.Logf); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"host", "host/art"} {
		st, err := os.Stat(filepath.Join(lib, repoDir, d))
		if err != nil || st.Mode().Perm() != 0o755 {
			t.Errorf("%s: %v %v", d, st.Mode(), err)
		}
	}
}
