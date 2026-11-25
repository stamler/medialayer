package core

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/rclone/rclone/backend/local"
	"github.com/rclone/rclone/fs"

	"github.com/stamler/medialayer/internal/index"
)

// testFLAC builds a small FLAC file with the given tags and cover.
func testFLAC(tags []string, cover []byte, audio int) []byte {
	var b bytes.Buffer
	b.WriteString("fLaC")
	block := func(typ byte, last bool, data []byte) {
		if last {
			typ |= 0x80
		}
		n := len(data)
		b.Write([]byte{typ, byte(n >> 16), byte(n >> 8), byte(n)})
		b.Write(data)
	}
	si := make([]byte, 34)
	binary.BigEndian.PutUint64(si[10:], uint64(44100)<<44|uint64(1)<<41|uint64(15)<<36|uint64(44100*60))
	block(0, false, si)

	var vc bytes.Buffer
	le := func(n int) { _ = binary.Write(&vc, binary.LittleEndian, uint32(n)) }
	le(0)
	le(len(tags))
	for _, t := range tags {
		le(len(t))
		vc.WriteString(t)
	}
	block(4, cover == nil, vc.Bytes())

	if cover != nil {
		var p bytes.Buffer
		be := func(n int) { _ = binary.Write(&p, binary.BigEndian, uint32(n)) }
		be(3)
		be(len("image/jpeg"))
		p.WriteString("image/jpeg")
		be(0)
		be(500)
		be(500)
		be(24)
		be(0)
		be(len(cover))
		p.Write(cover)
		block(6, true, p.Bytes())
	}
	audioBytes := make([]byte, audio)
	rand.New(rand.NewSource(1)).Read(audioBytes)
	b.Write(audioBytes)
	return b.Bytes()
}

// newTestCore makes a core whose library folder is a local directory.
func newTestCore(t *testing.T, libDir string) *Core {
	t.Helper()
	c, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	f, err := fs.NewFs(context.Background(), libDir)
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.settings.Folder = "test"
	c.fsys = f
	if err := c.openDB(); err != nil {
		t.Fatal(err)
	}
	c.mu.Unlock()
	return c
}

func writeLibrary(t *testing.T, cover []byte) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string][]byte{
		"A/Album/01.flac": testFLAC([]string{"TITLE=One", "ARTIST=A", "ALBUM=Album", "GENRE=Rock"}, cover, 100_000),
		"A/Album/02.flac": testFLAC([]string{"TITLE=Two", "ARTIST=A", "ALBUM=Album", "GENRE=Rock"}, cover, 100_000),
		"B/Other/01.flac": testFLAC([]string{"TITLE=Three", "ARTIST=B", "ALBUM=Other"}, nil, 50_000),
		"notes.txt":       []byte("not media"),
	}
	for p, b := range files {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestSyncIndexesPublishesAndRestores(t *testing.T) {
	ctx := context.Background()
	cover := bytes.Repeat([]byte("cover"), 2000)
	lib := writeLibrary(t, cover)

	c := newTestCore(t, lib)
	if err := c.sync(ctx); err != nil {
		t.Fatal(err)
	}
	st := c.Status()
	if st.Tracks != 3 || st.Job.Failed != 0 {
		t.Fatalf("tracks = %d, failed = %d (%v), want 3 and 0", st.Tracks, st.Job.Failed, st.Job.Failures)
	}
	// One cover shared by two tracks is stored once.
	var arts, links int
	db := c.currentDB()
	if err := dbQueryRow(db, `SELECT count(*) FROM art`, &arts); err != nil {
		t.Fatal(err)
	}
	if err := dbQueryRow(db, `SELECT count(*) FROM file_art`, &links); err != nil {
		t.Fatal(err)
	}
	if arts != 1 || links != 2 {
		t.Errorf("art rows = %d, file_art rows = %d, want 1 and 2", arts, links)
	}
	for _, p := range []string{".medialayer/repo.json", ".medialayer/snapshots/000001.sqlite"} {
		if _, err := os.Stat(filepath.Join(lib, p)); err != nil {
			t.Errorf("missing %s: %v", p, err)
		}
	}
	packs, _ := filepath.Glob(filepath.Join(lib, ".medialayer/art/pack-*.bin"))
	if len(packs) != 1 {
		t.Errorf("got %d art packs on the cloud, want 1", len(packs))
	}

	// A second run with no changes publishes nothing new.
	if err := c.sync(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(lib, ".medialayer/snapshots/000002.sqlite")); err == nil {
		t.Error("an unchanged library made a new snapshot")
	}

	// A second device opens the library from the snapshot instead of
	// indexing it again: the file ids, made at random when a file is
	// first indexed, are the same.
	c2 := newTestCore(t, lib)
	if err := c2.sync(ctx); err != nil {
		t.Fatal(err)
	}
	if st := c2.Status(); st.Tracks != 3 || st.Job.Failed != 0 {
		t.Fatalf("second device: tracks = %d, failed = %d (%v)", st.Tracks, st.Job.Failed, st.Job.Failures)
	}
	ids1, ids2 := fileIDs(t, c), fileIDs(t, c2)
	if fmt.Sprint(ids1) != fmt.Sprint(ids2) {
		t.Errorf("second device has file ids %v, want %v from the snapshot", ids2, ids1)
	}
	got, err := os.ReadFile(filepath.Join(c2.artDir(), filepath.Base(packs[0])))
	if err != nil || !bytes.Equal(got, cover) {
		t.Errorf("restored art pack: %v, %d bytes", err, len(got))
	}
}

func TestSyncNoticesChangesAndRemovals(t *testing.T) {
	ctx := context.Background()
	lib := writeLibrary(t, nil)
	c := newTestCore(t, lib)
	if err := c.sync(ctx); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(lib, "B/Other/01.flac")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lib, "A/Album/02.flac"),
		testFLAC([]string{"TITLE=Two (remaster)", "ARTIST=A"}, nil, 120_000), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := c.sync(ctx); err != nil {
		t.Fatal(err)
	}
	if st := c.Status(); st.Tracks != 2 {
		t.Errorf("tracks = %d, want 2 after a removal", st.Tracks)
	}
	var title string
	if err := dbQueryRow(c.currentDB(), `SELECT t.value FROM tag t JOIN file f ON f.id = t.file_id
		WHERE f.path = 'A/Album/02.flac' AND t.key = 'TITLE'`, &title); err != nil {
		t.Fatal(err)
	}
	if title != "Two (remaster)" {
		t.Errorf("title = %q after a retag", title)
	}
	if _, err := os.Stat(filepath.Join(lib, ".medialayer/snapshots/000002.sqlite")); err != nil {
		t.Errorf("changes made no new snapshot: %v", err)
	}
}

func TestStreamServesRanges(t *testing.T) {
	ctx := context.Background()
	lib := writeLibrary(t, nil)
	c := newTestCore(t, lib)
	if err := c.index(ctx, c.fsys); err != nil {
		t.Fatal(err)
	}
	var id string
	if err := dbQueryRow(c.currentDB(), `SELECT id FROM file WHERE path = 'A/Album/01.flac'`, &id); err != nil {
		t.Fatal(err)
	}
	want, _ := os.ReadFile(filepath.Join(lib, "A/Album/01.flac"))
	url := c.Status().StreamBase + id + ".flac"

	get := func(rng string) (*http.Response, []byte) {
		req, _ := http.NewRequest("GET", url, nil)
		if rng != "" {
			req.Header.Set("Range", rng)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, b
	}

	resp, body := get("")
	if resp.StatusCode != 200 || !bytes.Equal(body, want) || resp.Header.Get("Content-Type") != "audio/flac" {
		t.Errorf("whole file: status %d, %d bytes, type %q", resp.StatusCode, len(body), resp.Header.Get("Content-Type"))
	}
	resp, body = get("bytes=0-1")
	if resp.StatusCode != 206 || !bytes.Equal(body, want[:2]) {
		t.Errorf("bytes=0-1: status %d, %q", resp.StatusCode, body)
	}
	resp, body = get("bytes=1000-")
	if resp.StatusCode != 206 || !bytes.Equal(body, want[1000:]) ||
		resp.Header.Get("Content-Range") != fmt.Sprintf("bytes 1000-%d/%d", len(want)-1, len(want)) {
		t.Errorf("bytes=1000-: status %d, %d bytes, %q", resp.StatusCode, len(body), resp.Header.Get("Content-Range"))
	}
	resp, _ = get(fmt.Sprintf("bytes=%d-", len(want)))
	if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Errorf("range past the end: status %d", resp.StatusCode)
	}

	// Wrong token, wrong host.
	bad, _ := http.Get(c.stream.base()[:len(c.stream.base())-len(c.stream.token)-1] + "nope/" + id + ".flac")
	if bad.StatusCode != 404 {
		t.Errorf("wrong token: status %d", bad.StatusCode)
	}
	bad.Body.Close()
	req, _ := http.NewRequest("GET", url, nil)
	req.Host = "evil.example:80"
	bad, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if bad.StatusCode != 403 {
		t.Errorf("wrong host: status %d", bad.StatusCode)
	}
	bad.Body.Close()
}

func TestParseRange(t *testing.T) {
	tests := []struct {
		h          string
		start, end int64
		partial    bool
		err        bool
	}{
		{"", 0, 99, false, false},
		{"bytes=0-1", 0, 1, true, false},
		{"bytes=10-", 10, 99, true, false},
		{"bytes=90-500", 90, 99, true, false},
		{"bytes=-10", 90, 99, true, false},
		{"bytes=-500", 0, 99, true, false},
		{"bytes=0-1,5-6", 0, 99, false, false},
		{"bytes=100-", 0, 0, false, true},
		{"bytes=5-2", 0, 0, false, true},
		{"items=1-2", 0, 99, false, false},
	}
	for _, tt := range tests {
		s, e, p, err := parseRange(tt.h, 100)
		if (err != nil) != tt.err || (err == nil && (s != tt.start || e != tt.end || p != tt.partial)) {
			t.Errorf("parseRange(%q) = %d, %d, %v, %v", tt.h, s, e, p, err)
		}
	}
}

func dbQueryRow(db *index.DB, q string, dest ...any) error {
	return db.SQL().QueryRow(q).Scan(dest...)
}

func fileIDs(t *testing.T, c *Core) map[string]string {
	t.Helper()
	known, err := c.currentDB().KnownFiles()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for p, k := range known {
		out[p] = k.ID
	}
	return out
}
