package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/operations"

	"github.com/stamler/medialayer/internal/index"
)

// The .medialayer repository on the cloud, for the proof of concept:
//
//	.medialayer/repo.json                 library id and the primary record
//	.medialayer/snapshots/000001.sqlite   the index, newest number wins
//	.medialayer/art/pack-<ulid>.bin       cover art, content-addressed by the index
//
// Change logs, header packs and the sync lease come later.

// repoFormat is the version of the repository layout in repo.json.
const repoFormat = 1

// snapshotsKept is how many snapshots stay on the cloud after a publish.
const snapshotsKept = 2

// repoRecord is repo.json: which library this is and which cloud is its
// primary. The highest generation wins when copies disagree.
type repoRecord struct {
	Format     int       `json:"format"`
	LibraryID  string    `json:"library_id"`
	Generation int64     `json:"generation"`
	Primary    member    `json:"primary"`
	ChangedAt  time.Time `json:"changed_at"`
	ChangedBy  string    `json:"changed_by"` // device id
	Members    []member  `json:"members"`
}

type member struct {
	Replica string `json:"replica"` // e.g. "gdrive"
	Type    string `json:"type"`    // rclone backend, e.g. "drive"
	Folder  string `json:"folder"`  // library folder on that cloud
}

// readRepo reads repo.json, or returns nil if the folder has none.
func readRepo(ctx context.Context, f fs.Fs) (*repoRecord, error) {
	o, err := f.NewObject(ctx, repoDir+"/repo.json")
	if errors.Is(err, fs.ErrorObjectNotFound) || errors.Is(err, fs.ErrorDirNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rc, err := o.Open(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	var r repoRecord
	if err := json.NewDecoder(io.LimitReader(rc, 1<<20)).Decode(&r); err != nil {
		return nil, fmt.Errorf("repo.json: %w", err)
	}
	if r.Format != repoFormat {
		return nil, fmt.Errorf("repo.json has format %d; this version reads %d", r.Format, repoFormat)
	}
	return &r, nil
}

// listRepoDir lists one folder of the repository, by name. A missing
// folder is empty.
func listRepoDir(ctx context.Context, f fs.Fs, dir string) (map[string]fs.Object, error) {
	entries, err := f.List(ctx, repoDir+"/"+dir)
	if errors.Is(err, fs.ErrorDirNotFound) {
		return map[string]fs.Object{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]fs.Object{}
	for _, e := range entries {
		if o, ok := e.(fs.Object); ok {
			out[path.Base(o.Remote())] = o
		}
	}
	return out, nil
}

// errPublishedElsewhere means another device published the library
// while this one was indexing it from scratch.
var errPublishedElsewhere = errors.New("another device published this library first")

// snapshotNumber parses "000042.sqlite" (six or more digits); ok is false
// for other names.
func snapshotNumber(name string) (int, bool) {
	s, found := strings.CutSuffix(name, ".sqlite")
	if !found || len(s) < 6 || strings.Trim(s, "0123456789") != "" {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	return n, err == nil && n > 0
}

// newestSnapshot returns the highest-numbered snapshot, or nil.
func newestSnapshot(snaps map[string]fs.Object) (fs.Object, int) {
	var best fs.Object
	bestN := 0
	for name, o := range snaps {
		if n, ok := snapshotNumber(name); ok && n > bestN {
			best, bestN = o, n
		}
	}
	return best, bestN
}

// download copies o to the local file dst, through a temporary file.
func download(ctx context.Context, o fs.Object, dst string) error {
	rc, err := o.Open(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	tmp := dst + ".download"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, rc)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// restore replaces an empty local index with the newest snapshot from the
// cloud, and fetches the art packs it refers to. A local index that
// already belongs to a library is kept: indexing brings it up to date.
func (c *Core) restore(ctx context.Context, f fs.Fs) error {
	db := c.currentDB()
	if id, err := db.Meta("library_id"); err != nil || id != "" {
		return err
	}
	repo, err := readRepo(ctx, f)
	if err != nil || repo == nil {
		return err
	}
	snaps, err := listRepoDir(ctx, f, "snapshots")
	if err != nil {
		return err
	}
	snap, _ := newestSnapshot(snaps)
	if snap == nil {
		return nil // a repository with no snapshot yet: index from scratch
	}
	packs, err := listRepoDir(ctx, f, "art")
	if err != nil {
		return err
	}
	c.setJob(func(j *jobStatus) { j.Total = int64(1 + len(packs)) })
	// The snapshot becomes a new database file: the app may have the
	// current one open, and replacing a SQLite file under an open
	// connection can corrupt what it reads.
	name := "library-" + index.NewID() + ".sqlite"
	path := filepath.Join(c.dataDir, name)
	if err := download(ctx, snap, path); err != nil {
		return err
	}
	keep := false
	defer func() {
		if !keep {
			for _, p := range []string{path, path + "-wal", path + "-shm"} {
				_ = os.Remove(p)
			}
		}
	}()
	c.setJob(func(j *jobStatus) { j.Done++ })

	// Check the snapshot opens and is this library before using it.
	sdb, err := index.Open(path)
	if err != nil {
		return fmt.Errorf("snapshot %s: %w", snap.Remote(), err)
	}
	snapID, err := sdb.Meta("library_id")
	if err == nil && snapID != repo.LibraryID {
		err = fmt.Errorf("snapshot %s belongs to library %q, not %q", snap.Remote(), snapID, repo.LibraryID)
	}
	if err != nil {
		_ = sdb.Close()
		return err
	}

	for name, o := range packs {
		if !strings.HasPrefix(name, "pack-") {
			continue
		}
		local := filepath.Join(c.artDir(), name)
		if st, err := os.Stat(local); err == nil && st.Size() == o.Size() {
			continue // packs never change once published
		}
		if err := download(ctx, o, local); err != nil {
			_ = sdb.Close()
			return err
		}
		c.setJob(func(j *jobStatus) { j.Done++ })
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	old := c.db
	oldFile := c.settings.DBFile
	c.db = sdb
	c.dbEpoch++
	c.settings.DBFile = name
	if err := c.saveSettings(); err != nil {
		c.db, c.settings.DBFile = old, oldFile
		_ = sdb.Close()
		return err
	}
	keep = true
	// The old file is deleted at the next start, when nothing has it open.
	if old != nil {
		_ = old.Close()
	}
	return nil
}

// publish writes the index to the cloud when it changed: art packs the
// cloud lacks, a new snapshot, and repo.json if the folder has none yet.
func (c *Core) publish(ctx context.Context, f fs.Fs) error {
	db := c.currentDB()
	repo, err := readRepo(ctx, f)
	if err != nil {
		return err
	}
	dirty, err := db.Meta("dirty")
	if err != nil {
		return err
	}
	if repo != nil && dirty != "1" {
		return nil
	}
	c.setJob(func(j *jobStatus) { j.Phase, j.Done, j.Total, j.Current = PhasePublishing, 0, 0, "" })

	libraryID, err := db.Meta("library_id")
	if err != nil {
		return err
	}
	if repo != nil && libraryID != "" && libraryID != repo.LibraryID {
		return fmt.Errorf("this folder holds library %s, but the local index is library %s", repo.LibraryID, libraryID)
	}
	if repo != nil && libraryID == "" {
		snaps, err := listRepoDir(ctx, f, "snapshots")
		if err != nil {
			return err
		}
		if s, _ := newestSnapshot(snaps); s != nil {
			// Indexed from scratch while another device published: take
			// its snapshot instead, so both devices share file ids.
			return errPublishedElsewhere
		}
		libraryID = repo.LibraryID // a repository with no snapshot yet
	}
	if repo == nil {
		if libraryID == "" {
			libraryID = index.NewID()
		}
		repo = &repoRecord{
			Format:     repoFormat,
			LibraryID:  libraryID,
			Generation: 1,
			Primary:    member{Replica: replica, Type: "drive", Folder: c.folder()},
			ChangedAt:  time.Now().UTC(),
			ChangedBy:  c.settings.DeviceID,
		}
		repo.Members = []member{repo.Primary}
	} else {
		repo = nil // already on the cloud; don't rewrite it
	}
	if err := db.SetMeta("library_id", libraryID); err != nil {
		return err
	}

	// Art packs first, so a snapshot never refers to a pack the cloud lacks.
	cloudPacks, err := listRepoDir(ctx, f, "art")
	if err != nil {
		return err
	}
	// Only packs the index refers to: a discarded index can leave others.
	packs, err := db.ArtPacks()
	if err != nil {
		return err
	}
	for _, name := range packs {
		if _, ok := cloudPacks[name]; ok {
			continue
		}
		if err := upload(ctx, f, filepath.Join(c.artDir(), name), repoDir+"/art/"+name); err != nil {
			return err
		}
	}

	snaps, err := listRepoDir(ctx, f, "snapshots")
	if err != nil {
		return err
	}
	_, newest := newestSnapshot(snaps)
	name := fmt.Sprintf("%06d.sqlite", newest+1)
	// Clear the flag in the snapshot itself; it is set again locally if
	// the upload fails.
	if err := db.SetMeta("dirty", "0"); err != nil {
		return err
	}
	local := filepath.Join(c.dataDir, name)
	_ = os.Remove(local)
	if err := db.Snapshot(local); err != nil {
		_ = db.SetMeta("dirty", "1")
		return err
	}
	defer func() { _ = os.Remove(local) }()
	if err := upload(ctx, f, local, repoDir+"/snapshots/"+name); err != nil {
		_ = db.SetMeta("dirty", "1")
		return err
	}

	if repo != nil {
		b, err := json.MarshalIndent(repo, "", "  ")
		if err != nil {
			return err
		}
		tmp := filepath.Join(c.dataDir, "repo.json")
		if err := os.WriteFile(tmp, b, 0o600); err != nil {
			return err
		}
		defer func() { _ = os.Remove(tmp) }()
		if err := upload(ctx, f, tmp, repoDir+"/repo.json"); err != nil {
			return err
		}
	}

	// Keep the newest few snapshots.
	type numbered struct {
		n int
		o fs.Object
	}
	var old []numbered
	for name, o := range snaps {
		if n, ok := snapshotNumber(name); ok {
			old = append(old, numbered{n, o})
		}
	}
	sort.Slice(old, func(i, j int) bool { return old[i].n > old[j].n })
	for i, s := range old {
		if i+1 >= snapshotsKept { // the new snapshot counts as one
			if err := s.o.Remove(ctx); err != nil {
				fs.Errorf(s.o, "remove old snapshot: %v", err)
			}
		}
	}
	return nil
}

// upload copies the local file src to remote on f.
func upload(ctx context.Context, f fs.Fs, src, remote string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	_, err = operations.RcatSize(ctx, f, remote, in, st.Size(), st.ModTime(), nil)
	if err != nil {
		return fmt.Errorf("upload %s: %w", remote, err)
	}
	return nil
}
