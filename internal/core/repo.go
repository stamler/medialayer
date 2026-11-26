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
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/fs/operations"

	"github.com/stamler/medialayer/internal/index"
)

// The .medialayer repository on the cloud, for the proof of concept:
//
//	.medialayer/repo.json                 library id and the primary record
//	.medialayer/snapshots/000001.sqlite   the index, newest number wins
//	.medialayer/art/pack-<ulid>.bin       cover art, content-addressed by the index
//	.medialayer/lease-<device id>.json    each device's claim on the sync lease (lease.go)
//
// Change logs and header packs come later.

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
	Replica string `json:"replica"`          // e.g. "gdrive"
	Type    string `json:"type"`             // rclone backend, e.g. "drive"
	Remote  string `json:"remote,omitempty"` // the device's rclone remote (which account), "" for local
	Folder  string `json:"folder"`           // library folder on that cloud
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
	snap, snapN := newestSnapshot(snaps)
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
	// The restored index is that version, and the primary holds it; keep
	// it as the local snapshot that secondaries receive.
	err = sdb.SetMeta("version", strconv.Itoa(snapN))
	if err == nil {
		err = sdb.SetMeta("primary_version", strconv.Itoa(snapN))
	}
	if err == nil {
		err = os.MkdirAll(filepath.Join(c.dataDir, "snapshots"), 0o700)
	}
	if err == nil {
		_ = os.Remove(c.localSnapshot(snapN))
		err = sdb.Snapshot(c.localSnapshot(snapN))
	}
	if err != nil {
		_ = sdb.Close()
		return err
	}

	// Playlists made in the index being replaced (one indexed from
	// scratch while another device published first) carry over by path.
	// They join the restored index now, with the items the snapshot
	// knows, so they never vanish; the full lists wait to be applied
	// after this sync's index pass (see applyCarried).
	c.playlistMu.Lock()
	defer c.playlistMu.Unlock()
	if old := c.currentDB(); old != nil {
		lists, err := old.PlaylistsByPath()
		if err == nil {
			_, err = sdb.AddPlaylistsByPath(lists)
		}
		if err == nil {
			err = c.carryPlaylists(lists)
		}
		if err != nil {
			_ = sdb.Close()
			return fmt.Errorf("carry over playlists: %w", err)
		}
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
	c.countTracks(sdb)
	// The old file is deleted at the next start, when nothing has it open.
	if old != nil {
		_ = old.Close()
	}
	return nil
}

// The local index carries a version, raised each time it changes and
// stored in its meta table as "version" (with the library's generation),
// so any copy of it says which version it is. Each version is kept as a
// local snapshot file, snapshots/NNNNNN.sqlite in the data directory,
// which is what the primary and every secondary receive. "primary_version"
// records the newest version the primary holds.

// localSnapshot is the local snapshot file of a version.
func (c *Core) localSnapshot(version int) string {
	return filepath.Join(c.dataDir, "snapshots", snapshotName(version))
}

func snapshotName(version int) string { return fmt.Sprintf("%06d.sqlite", version) }

// markAllDirty marks the index as changed in every way, for a version
// that was made but not saved: so the next sync makes one again, and the
// app treats its files and playlists as unsaved.
func markAllDirty(db *index.DB) {
	for _, k := range []string{"dirty", "files_dirty", "playlists_dirty"} {
		_ = db.SetMeta(k, "1")
	}
}

// metaInt reads a numeric meta value; missing is 0.
func metaInt(db *index.DB, key string) (int, error) {
	v, err := db.Meta(key)
	if err != nil || v == "" {
		return 0, err
	}
	return strconv.Atoi(v)
}

// currentVersion returns the index's version and its local snapshot
// file, making a new version when the index changed since the last one.
// newest is the highest snapshot number known on the primary, so
// versions keep rising across devices.
func (c *Core) currentVersion(db *index.DB, newest int, generation int64) (int, string, error) {
	dirty, err := db.Meta("dirty")
	if err != nil {
		return 0, "", err
	}
	version, err := metaInt(db, "version")
	if err != nil {
		return 0, "", err
	}
	local := c.localSnapshot(version)
	if _, err := os.Stat(local); version > 0 && dirty != "1" && err == nil {
		return version, local, nil
	}
	version = max(version, newest) + 1
	for k, v := range map[string]string{
		"version": strconv.Itoa(version), "generation": strconv.FormatInt(generation, 10),
		"dirty": "0", "files_dirty": "0", "playlists_dirty": "0",
	} {
		if err := db.SetMeta(k, v); err != nil {
			return 0, "", err
		}
	}
	local = c.localSnapshot(version)
	if err := os.MkdirAll(filepath.Dir(local), 0o700); err != nil {
		return 0, "", err
	}
	_ = os.Remove(local)
	if err := db.Snapshot(local); err != nil {
		markAllDirty(db)
		return 0, "", err
	}
	// Older versions are no longer needed: every copy gets the newest.
	old, _ := filepath.Glob(filepath.Join(c.dataDir, "snapshots", "*.sqlite"))
	for _, p := range old {
		if p != local {
			_ = os.Remove(p)
		}
	}
	return version, local, nil
}

// publish brings the primary up to date with the local index: a new
// version when the index changed, the art packs the primary lacks, that
// version's snapshot, and repo.json if the folder has none yet. A version
// the primary couldn't take (it was full, say) is offered again on every
// sync until it does.
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
	version, err := metaInt(db, "version")
	if err != nil {
		return err
	}
	onPrimary, err := metaInt(db, "primary_version")
	if err != nil {
		return err
	}
	_, statErr := os.Stat(c.localSnapshot(version))
	if repo != nil && dirty != "1" && version > 0 && onPrimary >= version && statErr == nil {
		return nil
	}
	c.setJob(func(j *jobStatus) { j.Phase, j.Done, j.Total, j.Current = PhasePublishing, 0, 0, "" })
	if err := checkLease(ctx); err != nil {
		return err
	}

	libraryID, err := db.Meta("library_id")
	if err != nil {
		return err
	}
	if repo != nil && libraryID != "" && libraryID != repo.LibraryID {
		return fmt.Errorf("this folder holds library %s, but the local index is library %s", repo.LibraryID, libraryID)
	}
	snaps, err := listRepoDir(ctx, f, "snapshots")
	if err != nil {
		return err
	}
	if repo != nil && libraryID == "" {
		if s, _ := newestSnapshot(snaps); s != nil {
			// Indexed from scratch while another device published: take
			// its snapshot instead, so both devices share file ids.
			return errPublishedElsewhere
		}
		libraryID = repo.LibraryID // a repository with no snapshot yet
	}
	newRepo := repo == nil
	if newRepo {
		if libraryID == "" {
			libraryID = index.NewID()
		} else if err := c.dropSecondaries(); err != nil {
			// The folder lost its repository, or is another account's
			// folder at the same path: either way it may not hold what the
			// secondaries copied, so they must not follow it unasked.
			return err
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
	}
	prevID, err := db.Meta("library_id")
	if err != nil {
		return err
	}
	if err := db.SetMeta("library_id", libraryID); err != nil {
		return err
	}
	published := false
	if newRepo {
		// Until repo.json is on the primary, the library doesn't exist
		// there; a later sync must start it again, not take this id for
		// a primary that lost its .medialayer.
		defer func() {
			if !published {
				_ = db.SetMeta("library_id", prevID)
				markAllDirty(db) // nothing of this version is saved anywhere
			}
		}()
	}
	_, newest := newestSnapshot(snaps)
	version, local, err := c.currentVersion(db, newest, repo.Generation)
	if err != nil {
		return err
	}
	// A version waiting for the primary may have been taken meanwhile by
	// another device's different index: then this one becomes a newer
	// version, so a version number always names one index.
	if o, ok := snaps[snapshotName(version)]; version <= newest && !(ok && sameFile(ctx, o, local)) {
		if err := db.SetMeta("dirty", "1"); err != nil {
			return err
		}
		if version, local, err = c.currentVersion(db, newest, repo.Generation); err != nil {
			return err
		}
	}
	name := snapshotName(version)

	// Art packs first, so a snapshot never refers to a pack the cloud lacks.
	if err := c.uploadPacks(ctx, f, db); err != nil {
		return err
	}
	if o, ok := snaps[name]; !ok || !sameFile(ctx, o, local) {
		if err := upload(ctx, f, local, repoDir+"/snapshots/"+name); err != nil {
			return err
		}
	}
	if newRepo {
		b, err := json.MarshalIndent(repo, "", "  ")
		if err != nil {
			return err
		}
		if err := putBytes(ctx, f, repoDir+"/repo.json", b); err != nil {
			return err
		}
	}
	published = true
	if err := db.SetMeta("primary_version", strconv.Itoa(version)); err != nil {
		return err
	}

	// Keep the newest few snapshots.
	type numbered struct {
		n int
		o fs.Object
	}
	var old []numbered
	for sname, o := range snaps {
		if n, ok := snapshotNumber(sname); ok && sname != name {
			old = append(old, numbered{n, o})
		}
	}
	sort.Slice(old, func(i, j int) bool { return old[i].n > old[j].n })
	for i, s := range old {
		if i+1 >= snapshotsKept { // the new snapshot counts as one
			if err := checkLease(ctx); err != nil {
				return err
			}
			if err := s.o.Remove(ctx); err != nil {
				fs.Errorf(s.o, "remove old snapshot: %v", err)
			}
		}
	}
	return nil
}

// uploadPacks uploads the art packs the index refers to that f lacks.
func (c *Core) uploadPacks(ctx context.Context, f fs.Fs, db *index.DB) error {
	have, err := listRepoDir(ctx, f, "art")
	if err != nil {
		return err
	}
	// Only packs the index refers to: a discarded index can leave others.
	packs, err := db.ArtPacks()
	if err != nil {
		return err
	}
	for _, name := range packs {
		local := filepath.Join(c.artDir(), name)
		o, ok := have[name]
		if ok && (fileSize(local) < 0 || o.Size() == fileSize(local)) {
			continue // packs never change once the run that wrote them ends
		}
		if err := upload(ctx, f, local, repoDir+"/art/"+name); err != nil {
			return err
		}
	}
	return nil
}

// sameFile reports whether remote object o has the same contents as the
// local file, by size and a hash the remote keeps: MD5, or else SHA-1
// (Proton Drive). Without either it says no, so the file is written
// again.
func sameFile(ctx context.Context, o fs.Object, local string) bool {
	if o.Size() != fileSize(local) {
		return false
	}
	ht := hash.MD5
	if !o.Fs().Hashes().Contains(ht) {
		ht = hash.SHA1
	}
	theirs, err := o.Hash(ctx, ht)
	if err != nil || theirs == "" {
		return false
	}
	in, err := os.Open(local)
	if err != nil {
		return false
	}
	defer func() { _ = in.Close() }()
	mine, err := hash.StreamTypes(in, hash.NewHashSet(ht))
	if err != nil {
		return false
	}
	return strings.EqualFold(mine[ht], theirs)
}

// fileSize is a local file's size, or -1.
func fileSize(p string) int64 {
	st, err := os.Stat(p)
	if err != nil {
		return -1
	}
	return st.Size()
}

// upload copies the local file src to remote on f.
func upload(ctx context.Context, f fs.Fs, src, remote string) error {
	if err := checkLease(ctx); err != nil {
		return err
	}
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
