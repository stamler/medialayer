package core

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stamler/medialayer/internal/index"
)

func TestHashPassHashesEveryTrack(t *testing.T) {
	quietLease(t)
	c, lib, _ := newMirrorCore(t) // three FLACs that only have headers, so can't be decoded
	c.mu.Lock()
	c.settings.Secondaries = nil
	c.mu.Unlock()
	for i := range 4 {
		write(t, filepath.Join(lib, fmt.Sprintf("Rip/%02d.flac", i)), encodedFLAC(t, "Track", int64(i), 4096))
	}
	if st := runSync(t, c); st.Job.Phase != PhaseDone {
		t.Fatalf("job = %+v", st.Job)
	}
	c.waitHashPass()
	h := c.Status().AudioHashes
	if h == nil || h.Running || h.Total != 7 || h.Hashed != 4 || h.Failed != 3 {
		t.Fatalf("audio hashes = %+v, want 4 of 7 hashed and 3 failed", h)
	}
	db := c.currentDB()
	if total, hashed, err := db.AudioHashCounts(hashFormats...); err != nil || total != 7 || hashed != 4 {
		t.Fatalf("index has %d of %d hashed (%v)", hashed, total, err)
	}
	if v, _ := db.Meta("dirty"); v != "1" {
		t.Errorf("index dirty = %q, want a new version due", v)
	}
	// The next sync publishes them, and its pass has nothing left but the
	// files that failed, which it doesn't try again.
	before, _ := metaInt(db, "version")
	if st := runSync(t, c); st.Job.Phase != PhaseDone {
		t.Fatalf("job = %+v", st.Job)
	}
	c.waitHashPass()
	if after, _ := metaInt(c.currentDB(), "version"); after != before+1 {
		t.Errorf("version %d after %d, want a new one with the hashes", after, before)
	}
	if h := c.Status().AudioHashes; h == nil || h.Hashed != 4 || h.Failed != 3 {
		t.Fatalf("second pass = %+v", h)
	}
	if v, _ := c.currentDB().Meta("dirty"); v == "1" {
		t.Error("a pass with nothing new marked the index changed")
	}
}

// slowLibrary adds n real FLACs to lib, and holds the hash pass before
// its first file until release is called, so it is still running when
// the test acts.
func slowLibrary(t *testing.T, c *Core, lib string, n int) (release func()) {
	t.Helper()
	for i := range n {
		write(t, filepath.Join(lib, fmt.Sprintf("Slow/%03d.flac", i)), encodedFLAC(t, "Slow", int64(i), 4096))
	}
	gate := make(chan struct{})
	var once sync.Once
	c.hashFileHook = func() { <-gate }
	release = func() { once.Do(func() { close(gate) }) }
	t.Cleanup(release)
	return release
}

func TestJobStopsHashPassAndRestartsIt(t *testing.T) {
	quietLease(t)
	c, lib, _ := newMirrorCore(t)
	c.mu.Lock()
	c.settings.Secondaries = nil
	c.mu.Unlock()
	release := slowLibrary(t, c, lib, 20)
	if st := runSync(t, c); st.Job.Phase != PhaseDone {
		t.Fatalf("job = %+v", st.Job)
	}
	c.mu.Lock()
	if c.hash.cancel == nil {
		c.mu.Unlock()
		t.Fatal("no hash pass after a sync")
	}
	var stoppedInJob bool
	var hashedInJob int64 = -1
	err := c.startJobLocked(func(ctx context.Context) error {
		c.mu.Lock()
		stoppedInJob = c.hash.cancel == nil
		c.mu.Unlock()
		_, n, err := c.currentDB().AudioHashCounts(hashFormats...)
		hashedInJob = n
		// No pass write lands while the job runs.
		for range 20 {
			if _, m, _ := c.currentDB().AudioHashCounts(hashFormats...); m != n {
				return fmt.Errorf("hashes went from %d to %d during the job", n, m)
			}
		}
		return err
	})
	done := c.jobDone
	c.mu.Unlock()
	release()
	if err != nil {
		t.Fatal(err)
	}
	<-done
	if st := c.Status(); st.Job.Phase != PhaseDone || !stoppedInJob || hashedInJob < 0 {
		t.Fatalf("job = %+v, pass stopped in job: %v", st.Job, stoppedInJob)
	}
	// The pass starts again after the job and finishes the library.
	c.waitHashPass()
	if _, n, _ := c.currentDB().AudioHashCounts(hashFormats...); n != 20 {
		t.Fatalf("%d of 20 hashed after the pass started again", n)
	}
}

func TestCloseWaitsForHashPass(t *testing.T) {
	quietLease(t)
	c, lib, _ := newMirrorCore(t)
	c.mu.Lock()
	c.settings.Secondaries = nil
	c.mu.Unlock()
	release := slowLibrary(t, c, lib, 20)
	if st := runSync(t, c); st.Job.Phase != PhaseDone {
		t.Fatalf("job = %+v", st.Job)
	}
	// A job stops the pass, then Close comes while it may still be ending.
	c.mu.Lock()
	done := c.hash.done
	c.stopHashPassLocked()
	c.mu.Unlock()
	go func() { time.Sleep(50 * time.Millisecond); release() }()
	if done == nil {
		t.Fatal("no hash pass after a sync")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	default:
		t.Fatal("Close returned while the hash pass ran")
	}
}

func TestOldAudioHashesAreCleared(t *testing.T) {
	p := filepath.Join(t.TempDir(), "i.sqlite")
	db, err := index.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().Exec(`INSERT INTO file (id, path, size, mtime, format, audio_sha256) VALUES ('a', 'a.flac', 1, 1, 'flac', x'00')`); err != nil {
		t.Fatal(err)
	}
	if err := db.SetMeta("audio_hash_version", "1"); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if db, err = index.Open(p); err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if total, hashed, err := db.AudioHashCounts("flac"); err != nil || total != 1 || hashed != 0 {
		t.Fatalf("%d of %d hashed (%v), want the old hash cleared", hashed, total, err)
	}
	if v, _ := db.Meta("audio_hash_version"); v != index.AudioHashVersion {
		t.Fatalf("audio_hash_version = %q", v)
	}
}

func TestIndexAndHashALAC(t *testing.T) {
	quietLease(t)
	c, lib, _ := newMirrorCore(t)
	c.mu.Lock()
	c.settings.Secondaries = nil
	c.mu.Unlock()
	for name, to := range map[string]string{
		"alac16-tagged.m4a": "M4A/01.m4a", "flac16.flac": "M4A/same.flac", "aac-tagged.m4a": "M4A/02.m4a",
		"alac16-mono.mp4": "M4A/03.mp4", "alac16.caf": "M4A/04.caf",
		"flac16.oga": "M4A/05.oga", "opus.ogg": "M4A/06.ogg",
	} {
		b, err := os.ReadFile(filepath.Join("..", "media", "testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(lib, to), b)
	}
	// PCM in CAF isn't a track MediaLayer reads: skipped, not a failure.
	pcm, err := os.ReadFile(filepath.Join("..", "media", "testdata", "alac16.caf"))
	if err != nil {
		t.Fatal(err)
	}
	copy(pcm[bytes.Index(pcm, []byte("desc"))+20:], "lpcm")
	write(t, filepath.Join(lib, "M4A", "pcm.caf"), pcm)
	if st := runSync(t, c); st.Job.Phase != PhaseDone || st.Job.Failed != 0 {
		t.Fatalf("job = %+v", st.Job)
	}
	c.waitHashPass()
	db := c.currentDB().SQL()
	var format, title string
	var bits int
	if err := db.QueryRow(`SELECT f.format, f.bit_depth, t.value FROM file f JOIN tag t ON t.file_id = f.id AND t.key = 'TITLE'
		WHERE f.path = 'M4A/01.m4a'`).Scan(&format, &bits, &title); err != nil || format != "m4a" || bits != 16 || title != "A Title" {
		t.Fatalf("01.m4a indexed as %q, %d bits, %q (%v)", format, bits, title, err)
	}
	var art int
	_ = db.QueryRow(`SELECT count(*) FROM file_art a JOIN file f ON f.id = a.file_id WHERE f.path = 'M4A/01.m4a'`).Scan(&art)
	if art != 1 {
		t.Errorf("01.m4a has %d covers", art)
	}
	hashes := map[string]string{}
	rows, err := db.Query(`SELECT path, hex(audio_sha256) FROM file WHERE path LIKE 'M4A/%'`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var p, h string
		_ = rows.Scan(&p, &h)
		hashes[p] = h
	}
	_ = rows.Close()
	if err := db.QueryRow(`SELECT f.format, f.bit_depth, t.value FROM file f JOIN tag t ON t.file_id = f.id AND t.key = 'TITLE'
		WHERE f.path = 'M4A/04.caf'`).Scan(&format, &bits, &title); err != nil || format != "caf" || bits != 16 || title != "A Title" {
		t.Fatalf("04.caf indexed as %q, %d bits, %q (%v)", format, bits, title, err)
	}
	if len(hashes) != 6 || hashes["M4A/05.oga"] != hashes["M4A/same.flac"] || hashes["M4A/01.m4a"] == "" || hashes["M4A/01.m4a"] != hashes["M4A/same.flac"] ||
		hashes["M4A/04.caf"] != hashes["M4A/same.flac"] ||
		hashes["M4A/02.m4a"] == "" || hashes["M4A/03.mp4"] == "" {
		t.Fatalf("hashes = %v, want ALAC and FLAC of the same audio to match", hashes)
	}
}
