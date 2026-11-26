package core

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/rclone/rclone/backend/local"
	"github.com/rclone/rclone/fs"

	"github.com/stamler/medialayer/internal/index"
	"github.com/stamler/medialayer/internal/media"
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
	// The probe parked a stream; the follow-up from 0 continues it.
	if n := len(c.stream.heads); n != 1 {
		t.Errorf("%d parked streams after the probe, want 1", n)
	}
	resp, body = get("bytes=0-")
	if resp.StatusCode != 206 || !bytes.Equal(body, want) {
		t.Errorf("bytes=0- after the probe: status %d, %d bytes", resp.StatusCode, len(body))
	}
	if n := len(c.stream.heads); n != 0 {
		t.Errorf("%d parked streams after the follow-up, want 0", n)
	}
	// A repeated probe is answered from memory and keeps the stream parked.
	get("bytes=0-1")
	resp, body = get("bytes=0-1")
	if resp.StatusCode != 206 || !bytes.Equal(body, want[:2]) || len(c.stream.heads) != 1 {
		t.Errorf("repeated probe: status %d, %q, %d parked", resp.StatusCode, body, len(c.stream.heads))
	}
	resp, body = get("bytes=0-")
	if !bytes.Equal(body, want) || len(c.stream.heads) != 0 {
		t.Errorf("follow-up after two probes: %d bytes, %d parked", len(body), len(c.stream.heads))
	}
	// A follow-up that starts inside the probed bytes.
	get("bytes=0-99")
	resp, body = get("bytes=50-200")
	if resp.StatusCode != 206 || !bytes.Equal(body, want[50:201]) {
		t.Errorf("bytes=50-200 after a probe: status %d, %d bytes", resp.StatusCode, len(body))
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
		{"bytes=5-2", 0, 99, false, false},
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

// Two devices that index a new folder at the same time end up with the
// same file ids: the second to publish takes the first one's snapshot,
// and uploads only art its index refers to.
func TestTwoFirstDevicesShareFileIDs(t *testing.T) {
	ctx := context.Background()
	lib := writeLibrary(t, nil)
	a, b := newTestCore(t, lib), newTestCore(t, lib)
	// B indexes a version of 01.flac with a cover, making an art pack ...
	orig, err := os.ReadFile(filepath.Join(lib, "A/Album/01.flac"))
	if err != nil {
		t.Fatal(err)
	}
	withCover := testFLAC([]string{"TITLE=One"}, bytes.Repeat([]byte("art"), 100), 100_000)
	if err := os.WriteFile(filepath.Join(lib, "A/Album/01.flac"), withCover, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := b.index(ctx, b.fsys); err != nil {
		t.Fatal(err)
	}
	// ... but the file changes back and A publishes first.
	if err := os.WriteFile(filepath.Join(lib, "A/Album/01.flac"), orig, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.sync(ctx); err != nil {
		t.Fatal(err)
	}
	// A change after A's publish makes B publish too.
	if err := os.WriteFile(filepath.Join(lib, "B/Other/01.flac"),
		testFLAC([]string{"TITLE=Three (live)"}, nil, 50_000), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := b.sync(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.sync(ctx); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(fileIDs(t, a)) != fmt.Sprint(fileIDs(t, b)) {
		t.Errorf("file ids differ: %v and %v", fileIDs(t, a), fileIDs(t, b))
	}
	if _, err := os.Stat(filepath.Join(lib, ".medialayer/snapshots/000002.sqlite")); err != nil {
		t.Errorf("B did not publish: %v", err)
	}
	// B's art pack belonged to the index it discarded: it stays local.
	if packs, _ := filepath.Glob(filepath.Join(lib, ".medialayer/art/*")); len(packs) != 0 {
		t.Errorf("cloud art packs = %v, want none", packs)
	}
}

func TestResetLocalRemovesEveryIndexFile(t *testing.T) {
	dir := t.TempDir()
	art := filepath.Join(dir, "art")
	for _, name := range []string{"library.sqlite", "library.sqlite-wal", "library-X.sqlite", "settings.json"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := resetLocal(dir, art); err != nil {
		t.Fatal(err)
	}
	left, _ := filepath.Glob(filepath.Join(dir, "*"))
	if len(left) != 2 { // settings.json and the empty art folder
		t.Errorf("left %v", left)
	}
}

// Snapshot names that are not six-digit numbers are left alone.
func TestPruneIgnoresOddSnapshotNames(t *testing.T) {
	ctx := context.Background()
	lib := writeLibrary(t, nil)
	snaps := filepath.Join(lib, ".medialayer/snapshots")
	if err := os.MkdirAll(snaps, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snaps, "1.sqlite"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := newTestCore(t, lib)
	for i := range 3 {
		if i > 0 {
			if err := c.currentDB().SetMeta("dirty", "1"); err != nil {
				t.Fatal(err)
			}
		}
		if err := c.sync(ctx); err != nil {
			t.Fatal(err)
		}
	}
	names, _ := filepath.Glob(filepath.Join(snaps, "*.sqlite"))
	for i := range names {
		names[i] = filepath.Base(names[i])
	}
	if fmt.Sprint(names) != "[000002.sqlite 000003.sqlite 1.sqlite]" {
		t.Errorf("snapshots = %v", names)
	}
}

func TestFolderCoverForTracksWithoutEmbeddedArt(t *testing.T) {
	ctx := context.Background()
	embedded := bytes.Repeat([]byte("embedded"), 100)
	lib := writeLibrary(t, embedded) // A/Album has embedded art, B/Other none
	cover := testPNG(t, 3)
	for name, data := range map[string][]byte{
		"B/Other/00-b-other-cd-flac-2006.jpg":   cover,
		"B/Other/._00-b-other-cd-flac-2006.jpg": []byte("AppleDouble"),
		"B/Other/back.jpg":                      testPNG(t, 4),
		"A/Album/folder.jpg":                    testPNG(t, 5),
		"C/Box/cover.png":                       testPNG(t, 6),
		"C/Box/CD2/01.flac":                     testFLAC([]string{"TITLE=Disc two"}, nil, 1000),
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(lib, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(lib, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	c := newTestCore(t, lib)
	if err := c.sync(ctx); err != nil {
		t.Fatal(err)
	}
	db := c.currentDB()
	var n int
	if err := dbQueryRow(db, `SELECT count(*) FROM file f WHERE NOT EXISTS (SELECT 1 FROM file_art a WHERE a.file_id = f.id)`, &n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d files have no art", n)
	}
	var sha []byte
	if err := dbQueryRow(db, `SELECT a.art_sha256 FROM file_art a JOIN file f ON f.id = a.file_id WHERE f.path = 'B/Other/01.flac'`, &sha); err != nil {
		t.Fatal(err)
	}
	if want := sha256.Sum256(cover); !bytes.Equal(sha, want[:]) {
		t.Error("B/Other got the wrong image")
	}
	var arts int
	if err := dbQueryRow(db, `SELECT count(*) FROM art`, &arts); err != nil {
		t.Fatal(err)
	}
	if arts != 3 {
		t.Errorf("art rows = %d, want 3 (embedded, B's and C's folder covers)", arts)
	}

	// Nothing changed: no new snapshot, no new reads.
	if err := c.sync(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(lib, ".medialayer/snapshots/000002.sqlite")); err == nil {
		t.Error("an unchanged library made a new snapshot")
	}
	if dirty, _ := db.Meta("dirty"); dirty != "0" {
		t.Errorf("dirty = %q after an unchanged sync", dirty)
	}
}

// A cover that can't be decoded is remembered, and the next sync falls
// back to the folder's next best image.
func TestFolderCoverFallsBackAfterBadImage(t *testing.T) {
	ctx := context.Background()
	lib := writeLibrary(t, bytes.Repeat([]byte("embedded"), 100))
	front := testPNG(t, 7)
	for name, data := range map[string][]byte{
		"B/Other/cover.jpg": []byte("RIFF....WEBP not a jpeg"),
		"B/Other/front.jpg": front,
	} {
		if err := os.WriteFile(filepath.Join(lib, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	c := newTestCore(t, lib)
	if err := c.sync(ctx); err != nil {
		t.Fatal(err)
	}
	if st := c.Status(); st.Job.Failed != 1 {
		t.Errorf("failed = %d, want 1 for the bad cover.jpg", st.Job.Failed)
	}
	if err := c.sync(ctx); err != nil {
		t.Fatal(err)
	}
	var sha []byte
	if err := dbQueryRow(c.currentDB(), `SELECT a.art_sha256 FROM file_art a JOIN file f ON f.id = a.file_id WHERE f.path = 'B/Other/01.flac'`, &sha); err != nil {
		t.Fatal(err)
	}
	if want := sha256.Sum256(front); !bytes.Equal(sha, want[:]) {
		t.Error("B/Other did not fall back to front.jpg")
	}
}

// testPNG is a small valid image; seed makes each one different.
func testPNG(t *testing.T, seed int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	img.Set(0, 0, color.RGBA{R: byte(seed), A: 255})
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestPickCover(t *testing.T) {
	dir := t.TempDir()
	f, err := fs.NewFs(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	pick := func(names ...string) string {
		var objs []fs.Object
		for _, n := range names {
			if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			o, err := f.NewObject(context.Background(), n)
			if err != nil {
				t.Fatal(err)
			}
			objs = append(objs, o)
		}
		for _, n := range names {
			_ = os.Remove(filepath.Join(dir, n))
		}
		if o := pickCover(objs); o != nil {
			return o.Remote()
		}
		return ""
	}
	tests := []struct {
		names []string
		want  string
	}{
		{[]string{"back.jpg", "Cover.JPG", "cd.jpg"}, "Cover.JPG"},
		{[]string{"Album - Front.png", "Album - Back.png"}, "Album - Front.png"},
		{[]string{"00-sam_roberts-chemical_city-cd-flac-2006.jpg"}, "00-sam_roberts-chemical_city-cd-flac-2006.jpg"},
		{[]string{"00-artist-album-cd.jpg", "00-artist-album-proof.jpg"}, "00-artist-album-proof.jpg"},
		{[]string{"._Front Cover.jpg", "Front Cover.jpg"}, "Front Cover.jpg"},
		{[]string{"frontcover.jpg", "backcover.jpg"}, "frontcover.jpg"},
		{[]string{"cd1.jpg"}, ""},
		{[]string{"rear.jpg", "booklet-01.jpg"}, ""},
		{[]string{"Front.jpg", "cover.jpg"}, "cover.jpg"},
		{[]string{"Backstreet Boys - Millennium - Front.jpg"}, "Backstreet Boys - Millennium - Front.jpg"},
		{[]string{"Album (CD) - Front.jpg", "cd.jpg"}, "Album (CD) - Front.jpg"},
		{[]string{"Daft Punk - Discovery.jpg", "folder.jpg"}, "folder.jpg"},
		{[]string{"00-artist-album-scans.jpg"}, ""},
		{[]string{"scan.jpg", "only.jpg"}, "only.jpg"},
		{[]string{"a.jpg", "b.jpg"}, ""},
		{[]string{"back.jpg", "inlay.jpg"}, ""},
	}
	for _, tt := range tests {
		if got := pick(tt.names...); got != tt.want {
			t.Errorf("pickCover(%v) = %q, want %q", tt.names, got, tt.want)
		}
	}
}

func TestSquarish(t *testing.T) {
	for _, tc := range []struct {
		w, h int
		want bool
	}{{500, 500, true}, {300, 268, true}, {1413, 1391, true}, {3141, 1676, false}, {600, 1000, false}, {0, 0, false}} {
		if got := squarish(tc.w, tc.h); got != tc.want {
			t.Errorf("squarish(%d, %d) = %v", tc.w, tc.h, got)
		}
	}
}

func TestStreamServesDecodedFLAC(t *testing.T) {
	ctx := context.Background()
	lib := writeLibrary(t, nil)
	hashes := map[string]string{}
	list, err := os.ReadFile(filepath.Join("..", "media", "testdata", "audio-hashes.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(list)), "\n") {
		name, sum, _ := strings.Cut(line, " ")
		hashes[name] = sum
	}
	for _, name := range []string{"flac32.flac", "flac16.oga", "flac24-51.ogg"} {
		b, err := os.ReadFile(filepath.Join("..", "media", "testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(lib, "D", name), b)
		if name == "flac32.flac" { // and a copy with a damaged frame
			b = bytes.Clone(b)
			b[len(b)/2] ^= 0x40
			write(t, filepath.Join(lib, "D", "damaged.flac"), b)
		}
	}
	c := newTestCore(t, lib)
	if err := c.index(ctx, c.fsys); err != nil {
		t.Fatal(err)
	}
	wavs := map[string][]byte{}
	for _, name := range []string{"flac32.flac", "flac16.oga", "flac24-51.ogg", "damaged.flac"} {
		t.Run(name, func(t *testing.T) {
			var id string
			if err := c.currentDB().SQL().QueryRow(`SELECT id FROM file WHERE path = ?`, "D/"+name).Scan(&id); err != nil {
				t.Fatal(err)
			}
			url := c.Status().StreamBase + id + ".wav"
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
			resp, wav := get("")
			if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "audio/wav" || len(wav) < wavHeaderLen ||
				string(wav[:4]) != "RIFF" || string(wav[60:64]) != "data" {
				t.Fatalf("status %d, type %q, %d bytes", resp.StatusCode, resp.Header.Get("Content-Type"), len(wav))
			}
			rate, chans := binary.LittleEndian.Uint32(wav[24:]), wav[22]
			width := int(binary.LittleEndian.Uint16(wav[34:])) / 8
			if int(binary.LittleEndian.Uint32(wav[64:])) != len(wav)-wavHeaderLen {
				t.Fatalf("data chunk of %d bytes in %d", binary.LittleEndian.Uint32(wav[64:]), len(wav))
			}
			wavs[name] = wav
			if name == "damaged.flac" {
				// Silence where the damaged frame was, and the audio after.
				clean := wavs["flac32.flac"]
				if len(wav) != len(clean) || !bytes.Equal(wav[len(wav)-4096:], clean[len(clean)-4096:]) ||
					!bytes.Equal(wav[:wavHeaderLen], clean[:wavHeaderLen]) || bytes.Equal(wav, clean) {
					t.Fatalf("damaged: %d bytes, clean %d; head same %v tail same %v", len(wav), len(clean),
						bytes.Equal(wav[:4096], clean[:4096]), bytes.Equal(wav[len(wav)-4096:], clean[len(clean)-4096:]))
				}
				return
			}
			// The samples are the audio hash's, shifted to fill their bytes.
			data, bits := wav[wavHeaderLen:], byte(width*8) // whole-byte depths: no shift
			head := append([]byte("pcm\x00"), 0, 0, 0, 0, chans, bits)
			binary.LittleEndian.PutUint32(head[4:], rate)
			if sum := sha256.Sum256(append(head, data...)); hex.EncodeToString(sum[:]) != hashes[name] {
				t.Errorf("decoded audio hashes %x, want %s", sum, hashes[name])
			}
			for _, rng := range [][2]int{{0, 1}, {0, 99}, {50, 200}, {68, 68 + 4095}, {1001, 50000}, {len(wav) / 2, len(wav) - 1}, {len(wav) - 7, len(wav) - 1}} {
				resp, body := get(fmt.Sprintf("bytes=%d-%d", rng[0], rng[1]))
				if resp.StatusCode != 206 || !bytes.Equal(body, wav[rng[0]:rng[1]+1]) {
					t.Errorf("bytes=%d-%d: status %d, %d bytes", rng[0], rng[1], resp.StatusCode, len(body))
				}
			}
		})
	}
}

func TestWAVLayoutPadsOddData(t *testing.T) {
	src := &media.FLACSource{Info: media.StreamInfo{SampleRate: 44100, Channels: 1, BitsPerSample: 24}, Total: 3}
	l, err := newWAVLayout(src)
	if err != nil || l.size != wavHeaderLen+10 || binary.LittleEndian.Uint32(l.header[4:]) != uint32(l.size-8) ||
		binary.LittleEndian.Uint32(l.header[64:]) != 9 {
		t.Fatalf("%+v, %v", l, err)
	}
	src.Total = 1 << 40
	if _, err := newWAVLayout(src); err == nil {
		t.Fatal("a WAVE file over 4 GB")
	}
}
