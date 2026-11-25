package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/operations"
	"github.com/rclone/rclone/fs/walk"

	"github.com/stamler/medialayer/internal/index"
)

// Secondaries are other folders, on the same cloud or another one, that
// hold a full copy of the library. Changes flow one way: the primary is
// the truth, and each sync makes every secondary match it.
//
//   - A file removed from the primary is moved to the secondary's
//     .medialayer/trash/<time>/ folder, which is emptied after 30 days.
//   - A file that differs on the secondary, because it changed on the
//     primary or was edited on the secondary directly, is moved to trash
//     before the primary's copy replaces it.
//   - If one sync would remove or replace more than 5% of the library's
//     files, it pauses and asks before changing anything.
//   - The secondary gets the primary's repo.json, snapshot and art packs,
//     so it can open the library on its own and knows which cloud is the
//     primary. A secondary whose repo.json names a newer generation, or a
//     different library, is never written to.
//
// Not yet: the sync lease (one device at a time), the 5% limit counted
// over a day rather than one sync, and promoting a secondary.

const (
	trashDir   = repoDir + "/trash"
	trashStamp = "20060102T150405Z"
	trashKeep  = 30 * 24 * time.Hour
	// massChange is the share of the library's files that one sync may
	// remove or replace on a secondary without asking.
	massChange = 0.05
)

// source is a secondary in settings.json.
type source struct {
	ID     string `json:"id"`
	Remote string `json:"remote"` // rclone remote in the core's config; "" for a local folder
	Folder string `json:"folder"` // on the remote, or an absolute local path
}

// sourceType is the rclone backend, such as "drive", or "local".
func (s source) sourceType() string {
	if s.Remote == "" {
		return "local"
	}
	return config.GetValue(s.Remote, "type")
}

// mirrorState is a secondary's last sync, kept for this session.
type mirrorState struct {
	state                     string // MirrorSyncing, MirrorSynced, MirrorPaused or MirrorError
	copies, replaces, deletes int    // pending when paused, else done in the last sync
	err                       string
	synced                    time.Time
	// approved is how many removals and replacements the user allowed for
	// the next sync of this secondary, after a pause.
	approved int
}

// States of a secondary, as SourceStatus reports them.
const (
	MirrorSyncing = "syncing"
	MirrorSynced  = "synced"
	MirrorPaused  = "paused" // waiting for ApproveChanges
	MirrorError   = "error"
)

// SourceStatus is one of the library's folders, primary first.
type SourceStatus struct {
	ID       string    `json:"id"`
	Type     string    `json:"type"`   // rclone backend: "drive", "local", ...
	Remote   string    `json:"remote"` // "" for a local folder
	Folder   string    `json:"folder"`
	Primary  bool      `json:"primary"`
	State    string    `json:"state,omitempty"`
	Copies   int       `json:"copies"`
	Replaces int       `json:"replaces"`
	Deletes  int       `json:"deletes"`
	Error    string    `json:"error,omitempty"`
	Synced   time.Time `json:"synced,omitzero"`
}

// sourcesLocked lists the primary and the secondaries. The caller holds
// c.mu.
func (c *Core) sourcesLocked() []SourceStatus {
	if c.settings.Folder == "" {
		return nil
	}
	out := []SourceStatus{{
		ID: replica, Type: "drive", Remote: remoteName, Folder: c.settings.Folder, Primary: true,
	}}
	for _, s := range c.settings.Secondaries {
		st := SourceStatus{ID: s.ID, Type: s.sourceType(), Remote: s.Remote, Folder: s.Folder}
		if m := c.mirrors[s.ID]; m != nil {
			st.State, st.Error, st.Synced = m.state, m.err, m.synced
			st.Copies, st.Replaces, st.Deletes = m.copies, m.replaces, m.deletes
		}
		out = append(out, st)
	}
	return out
}

// overlaps reports whether two folders on one remote are the same or one
// holds the other.
func overlaps(a, b string) bool {
	a, b = strings.Trim(a, "/"), strings.Trim(b, "/")
	return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

// AddSecondary adds a folder as a secondary and starts a sync, which
// copies the library to it. remote is a connected rclone remote, or ""
// for a local folder (an absolute path).
func (c *Core) AddSecondary(remote, folder string) error {
	if remote == "" {
		folder = filepath.Clean(folder)
		if !filepath.IsAbs(folder) {
			return errors.New("a local folder needs an absolute path")
		}
	} else {
		folder = strings.Trim(folder, "/")
		if folder == "" {
			return errors.New("pick a folder, not the top of the drive")
		}
		if config.GetValue(remote, "type") == "" {
			return fmt.Errorf("%s is not connected", remote)
		}
	}
	c.mu.Lock()
	if c.settings.Folder == "" {
		c.mu.Unlock()
		return errors.New("pick the library's primary folder first")
	}
	if c.jobDone != nil {
		c.mu.Unlock()
		return errors.New("the library is syncing; wait for it to finish")
	}
	if remote == remoteName && overlaps(folder, c.settings.Folder) {
		c.mu.Unlock()
		return errors.New("a secondary can't be the primary folder, or inside it, or hold it")
	}
	for _, s := range c.settings.Secondaries {
		if s.Remote == remote && overlaps(folder, s.Folder) {
			c.mu.Unlock()
			return fmt.Errorf("%s is already a secondary, or overlaps one", folder)
		}
	}
	c.settings.Secondaries = append(c.settings.Secondaries, source{ID: index.NewID(), Remote: remote, Folder: folder})
	if err := c.saveSettings(); err != nil {
		c.settings.Secondaries = c.settings.Secondaries[:len(c.settings.Secondaries)-1]
		c.mu.Unlock()
		return err
	}
	c.mu.Unlock()
	return c.StartSync()
}

// RemoveSecondary stops syncing to a secondary. Its files stay where they
// are.
func (c *Core) RemoveSecondary(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.jobDone != nil {
		return errors.New("the library is syncing; wait for it to finish")
	}
	for i, s := range c.settings.Secondaries {
		if s.ID == id {
			old := c.settings.Secondaries
			c.settings.Secondaries = append(append([]source(nil), old[:i]...), old[i+1:]...)
			if err := c.saveSettings(); err != nil {
				c.settings.Secondaries = old
				return err
			}
			delete(c.mirrors, id)
			return nil
		}
	}
	return fmt.Errorf("no secondary %s", id)
}

// ApproveChanges lets a paused secondary go ahead with the removals and
// replacements it paused for, and starts a sync.
func (c *Core) ApproveChanges(id string) error {
	c.mu.Lock()
	m := c.mirrors[id]
	if m == nil || m.state != MirrorPaused {
		c.mu.Unlock()
		return errors.New("that folder isn't waiting for approval")
	}
	m.approved = m.replaces + m.deletes
	c.mu.Unlock()
	return c.StartSync()
}

func (c *Core) setMirror(id string, update func(m *mirrorState)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.mirrors == nil {
		c.mirrors = map[string]*mirrorState{}
	}
	m := c.mirrors[id]
	if m == nil {
		m = &mirrorState{}
		c.mirrors[id] = m
	}
	update(m)
}

// openSource opens a secondary's folder.
func openSource(ctx context.Context, s source) (fs.Fs, error) {
	if s.Remote == "" {
		return fs.NewFs(ctx, s.Folder)
	}
	return fs.NewFs(ctx, s.Remote+":"+s.Folder)
}

// listLibrary lists every file in a library folder outside .medialayer,
// by path. A missing folder is empty.
func listLibrary(ctx context.Context, f fs.Fs) (map[string]fs.Object, error) {
	out := map[string]fs.Object{}
	// A secondary's folder may not exist yet; check first, as walk logs
	// a missing folder as an error.
	if _, err := f.List(ctx, ""); errors.Is(err, fs.ErrorDirNotFound) {
		return out, nil
	}
	err := walk.ListR(ctx, f, "", true, -1, walk.ListObjects, func(entries fs.DirEntries) error {
		for _, e := range entries {
			if o, ok := e.(fs.Object); ok && !strings.HasPrefix(o.Remote(), repoDir+"/") {
				out[o.Remote()] = o
			}
		}
		return nil
	})
	if errors.Is(err, fs.ErrorDirNotFound) {
		return map[string]fs.Object{}, nil
	}
	return out, err
}

// mirrorAll brings every secondary up to date with the primary f. A
// secondary that fails or pauses doesn't stop the others; its status
// says what happened.
func (c *Core) mirrorAll(ctx context.Context, f fs.Fs) error {
	c.mu.Lock()
	secs := append([]source(nil), c.settings.Secondaries...)
	c.mu.Unlock()
	if len(secs) == 0 {
		return nil
	}
	c.setJob(func(j *jobStatus) { j.Phase, j.Done, j.Total, j.Current = PhaseMirroring, 0, 0, "" })
	repo, err := c.updateMembers(ctx, f, secs)
	if err != nil {
		return fmt.Errorf("update the library's members: %w", err)
	}
	src, err := listLibrary(ctx, f)
	if err != nil {
		return fmt.Errorf("list %s: %w", f.Root(), err)
	}
	for _, s := range secs {
		if err := ctx.Err(); err != nil {
			return err
		}
		c.setMirror(s.ID, func(m *mirrorState) { m.state, m.err = MirrorSyncing, "" })
		err := c.mirror(ctx, f, src, repo, s)
		if err != nil && ctx.Err() != nil {
			c.setMirror(s.ID, func(m *mirrorState) { m.state = "" })
			return ctx.Err()
		}
		if err != nil {
			fs.Errorf(nil, "secondary %s: %v", s.Folder, err)
			c.setMirror(s.ID, func(m *mirrorState) { m.state, m.err = MirrorError, err.Error() })
		}
	}
	return nil
}

// updateMembers makes the primary's repo.json list exactly the primary and
// these secondaries, and returns it.
func (c *Core) updateMembers(ctx context.Context, f fs.Fs, secs []source) (*repoRecord, error) {
	repo, err := readRepo(ctx, f)
	if err != nil {
		return nil, err
	}
	if repo == nil {
		return nil, errors.New("the primary has no repo.json yet")
	}
	want := []member{repo.Primary}
	for _, s := range secs {
		want = append(want, member{Replica: s.ID, Type: s.sourceType(), Folder: s.Folder})
	}
	same := len(want) == len(repo.Members)
	for i := 0; same && i < len(want); i++ {
		same = want[i] == repo.Members[i]
	}
	if same {
		return repo, nil
	}
	repo.Members = want
	return repo, c.writeRepo(ctx, f, repo)
}

// writeRepo uploads repo.json to f.
func (c *Core) writeRepo(ctx context.Context, f fs.Fs, repo *repoRecord) error {
	b, err := json.MarshalIndent(repo, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(c.dataDir, "repo-"+index.NewID()+".json")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp) }()
	// Put replaces an existing repo.json in place, on Drive too.
	return upload(ctx, f, tmp, repoDir+"/repo.json")
}

// mirror makes one secondary match the primary f, whose files are src.
func (c *Core) mirror(ctx context.Context, f fs.Fs, src map[string]fs.Object, repo *repoRecord, s source) error {
	dst, err := openSource(ctx, s)
	if err != nil {
		return err
	}
	theirs, err := readRepo(ctx, dst)
	if err != nil {
		return err
	}
	if theirs != nil && theirs.LibraryID != repo.LibraryID {
		return errors.New("this folder holds a different MediaLayer library; pick an empty folder or a copy of this library")
	}
	if theirs != nil && theirs.Generation > repo.Generation {
		return fmt.Errorf("this folder names a newer primary (generation %d, this device knows %d), so it was left alone",
			theirs.Generation, repo.Generation)
	}
	have, err := listLibrary(ctx, dst)
	if err != nil {
		return fmt.Errorf("list %s: %w", s.Folder, err)
	}

	var copies, replaces, deletes []string
	for p, o := range src {
		d, ok := have[p]
		switch {
		case !ok:
			copies = append(copies, p)
		case !operations.Equal(ctx, o, d):
			replaces = append(replaces, p)
		}
	}
	for p := range have {
		if _, ok := src[p]; !ok {
			deletes = append(deletes, p)
		}
	}
	changes := len(replaces) + len(deletes)
	c.mu.Lock()
	approved := 0
	if m := c.mirrors[s.ID]; m != nil {
		approved = m.approved
	}
	c.mu.Unlock()
	if changes > 0 && float64(changes) > massChange*float64(max(len(src), 1)) && changes > approved {
		c.setMirror(s.ID, func(m *mirrorState) {
			m.state, m.copies, m.replaces, m.deletes, m.approved = MirrorPaused, len(copies), len(replaces), len(deletes), 0
		})
		return nil
	}

	c.setJob(func(j *jobStatus) {
		j.Done, j.Total, j.Current = 0, int64(len(copies)+len(replaces)+len(deletes)), ""
	})
	stamp := time.Now().UTC().Format(trashStamp)
	toTrash := func(o fs.Object) error {
		_, err := operations.Move(ctx, dst, nil, trashDir+"/"+stamp+"/"+o.Remote(), o)
		return err
	}
	step := func(p string) {
		c.setJob(func(j *jobStatus) { j.Done++; j.Current = path.Base(p) })
	}
	for _, p := range deletes {
		if err := toTrash(have[p]); err != nil {
			return fmt.Errorf("move %s to trash: %w", p, err)
		}
		step(p)
	}
	for _, p := range replaces {
		if err := toTrash(have[p]); err != nil {
			return fmt.Errorf("move %s to trash: %w", p, err)
		}
		if _, err := operations.Copy(ctx, dst, nil, p, src[p]); err != nil {
			return fmt.Errorf("copy %s: %w", p, err)
		}
		step(p)
	}
	for _, p := range copies {
		if _, err := operations.Copy(ctx, dst, nil, p, src[p]); err != nil {
			return fmt.Errorf("copy %s: %w", p, err)
		}
		step(p)
	}
	if err := c.mirrorRepo(ctx, f, dst, repo); err != nil {
		return fmt.Errorf("copy .medialayer: %w", err)
	}
	if err := emptyTrash(ctx, dst, time.Now()); err != nil {
		fs.Errorf(dst, "empty trash: %v", err)
	}
	c.setMirror(s.ID, func(m *mirrorState) {
		m.state, m.err, m.synced, m.approved = MirrorSynced, "", time.Now().UTC(), 0
		m.copies, m.replaces, m.deletes = len(copies), len(replaces), len(deletes)
	})
	return nil
}

// mirrorRepo copies the primary's art packs, newest snapshot and
// repo.json to dst, repo.json last, so a secondary with a repo.json can
// always open the library.
func (c *Core) mirrorRepo(ctx context.Context, f, dst fs.Fs, repo *repoRecord) error {
	for _, dir := range []string{"art", "snapshots"} {
		ours, err := listRepoDir(ctx, f, dir)
		if err != nil {
			return err
		}
		theirs, err := listRepoDir(ctx, dst, dir)
		if err != nil {
			return err
		}
		if dir == "snapshots" {
			snap, _ := newestSnapshot(ours)
			if snap == nil {
				return errors.New("the primary has no snapshot yet")
			}
			ours = map[string]fs.Object{path.Base(snap.Remote()): snap}
		}
		for name, o := range ours {
			if t, ok := theirs[name]; ok && t.Size() == o.Size() {
				continue // packs and snapshots never change once written
			}
			if _, err := operations.Copy(ctx, dst, theirs[name], o.Remote(), o); err != nil {
				return err
			}
		}
		if dir == "snapshots" {
			for name, t := range theirs {
				if _, ok := ours[name]; !ok {
					if err := t.Remove(ctx); err != nil {
						fs.Errorf(t, "remove old snapshot: %v", err)
					}
				}
			}
		}
	}
	return c.writeRepo(ctx, dst, repo)
}

// emptyTrash deletes trash folders older than trashKeep.
func emptyTrash(ctx context.Context, dst fs.Fs, now time.Time) error {
	entries, err := dst.List(ctx, trashDir)
	if errors.Is(err, fs.ErrorDirNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		d, ok := e.(fs.Directory)
		if !ok {
			continue
		}
		t, err := time.Parse(trashStamp, path.Base(d.Remote()))
		if err != nil || now.Sub(t) < trashKeep {
			continue
		}
		if err := operations.Purge(ctx, dst, d.Remote()); err != nil {
			return err
		}
	}
	return nil
}
