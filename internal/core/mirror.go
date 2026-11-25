package core

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/operations"
	"github.com/rclone/rclone/fs/walk"
	"golang.org/x/text/unicode/norm"

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
	// pending identifies the removals and replacements a pause is for;
	// approved is the one the user allowed, for the next sync only.
	pending, approved string
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

// checkLocalFolder refuses local folders that can't be a secondary: the
// top of a disk, the home folder, and folders a cloud app syncs (such as
// Google Drive for desktop), which might hold the primary.
func checkLocalFolder(folder string) error {
	if real, err := filepath.EvalSymlinks(folder); err == nil {
		folder = real
	}
	parts := strings.Split(strings.Trim(filepath.ToSlash(folder), "/"), "/")
	if folder == "/" || (len(parts) <= 2 && parts[0] == "Volumes") {
		return errors.New("pick a folder on the disk, not the top of it")
	}
	if len(parts) >= 2 && parts[0] == "Volumes" && strings.HasPrefix(parts[1], "GoogleDrive") {
		return errors.New("this folder is synced by a cloud app; add that cloud's folder directly instead")
	}
	if home, err := os.UserHomeDir(); err == nil {
		if folder == home {
			return errors.New("pick a folder inside your home folder, not the home folder itself")
		}
		if cloud := filepath.Join(home, "Library", "CloudStorage"); folder == cloud || strings.HasPrefix(folder, cloud+"/") {
			return errors.New("this folder is synced by a cloud app; add that cloud's folder directly instead")
		}
	}
	return nil
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
		if err := checkLocalFolder(folder); err != nil {
			return err
		}
	} else {
		folder = strings.Trim(folder, "/")
		if folder == "" {
			return errors.New("pick a folder, not the top of the drive")
		}
		for _, part := range strings.Split(folder, "/") {
			if part == "" || part == "." || part == ".." {
				return fmt.Errorf("%q is not a folder path", folder)
			}
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
	switch {
	case c.jobDone != nil:
		c.mu.Unlock()
		return errors.New("the library is syncing; wait for it to finish")
	case m == nil || m.state != MirrorPaused:
		c.mu.Unlock()
		return errors.New("that folder isn't waiting for approval")
	}
	m.approved = m.pending
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

// dropSecondaries forgets the secondaries; their files stay.
func (c *Core) dropSecondaries() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.settings.Secondaries) == 0 {
		return nil
	}
	fs.Logf(nil, "the primary has no repo.json any more; secondaries need adding again")
	c.notice = "The primary folder had lost its .medialayer, so its secondaries were set aside. Add them again to keep copying to them."
	c.settings.Secondaries, c.mirrors = nil, nil
	return c.saveSettings()
}

// openSource opens a secondary's folder.
func openSource(ctx context.Context, s source) (fs.Fs, error) {
	if s.Remote == "" {
		return fs.NewFs(ctx, s.Folder)
	}
	return fs.NewFs(ctx, s.Remote+":"+s.Folder)
}

// listing is a library folder's files outside .medialayer, by path.
type listing struct {
	files map[string]fs.Object
	// extra are further files with a name already in files: Drive allows
	// several files of one name in a folder.
	extra []fs.Object
	dupes map[string]bool // paths with extra files
	// nested is set when the folder holds a .medialayer below its top: a
	// library inside it, or a folder that holds the primary. probes are
	// the probe files seen in those.
	nested bool
	probes map[string][]string // file name -> paths
}

// systemName reports names that the operating system keeps in folders,
// which are never copied or trashed.
func systemName(base string) bool {
	switch base {
	case ".DS_Store", ".Spotlight-V100", ".fseventsd", ".Trashes", ".TemporaryItems",
		".DocumentRevisions-V100", ".localized", "Icon\r", "desktop.ini", "Thumbs.db":
		return true
	}
	return strings.HasPrefix(base, "._")
}

// listLibrary lists a library folder. A missing folder is empty when
// missingOK, as for a secondary not yet written to, and an error
// otherwise.
func listLibrary(ctx context.Context, f fs.Fs, missingOK bool) (*listing, error) {
	l := &listing{files: map[string]fs.Object{}, dupes: map[string]bool{}, probes: map[string][]string{}}
	// Check first, as walk logs a missing folder as an error.
	if _, err := f.List(ctx, ""); errors.Is(err, fs.ErrorDirNotFound) {
		if missingOK {
			return l, nil
		}
		return nil, err
	}
	err := walk.ListR(ctx, f, "", true, -1, walk.ListObjects, func(entries fs.DirEntries) error {
	next:
		for _, e := range entries {
			o, ok := e.(fs.Object)
			if !ok {
				continue
			}
			r := o.Remote()
			if strings.HasPrefix(r, repoDir+"/") {
				continue
			}
			parts := strings.Split(r, "/")
			for i, part := range parts {
				if part == repoDir {
					l.nested = true
					if i+2 < len(parts) && parts[i+1] == "probe" {
						name := parts[len(parts)-1]
						l.probes[name] = append(l.probes[name], r)
					}
					continue next
				}
				if systemName(part) {
					continue next
				}
			}
			if _, ok := l.files[r]; ok {
				l.dupes[r] = true
				l.extra = append(l.extra, o)
				continue
			}
			l.files[r] = o
		}
		return nil
	})
	return l, err
}

// putBytes writes a small file to f.
func putBytes(ctx context.Context, f fs.Fs, remote string, b []byte) error {
	_, err := operations.RcatSize(ctx, f, remote, io.NopCloser(bytes.NewReader(b)), int64(len(b)), time.Now(), nil)
	return err
}

// target is a secondary that passed the checks before syncing.
type target struct {
	s      source
	dst    fs.Fs
	have   *listing
	probe  string       // its probe file, under .medialayer/probe/
	member memberRecord // the folder's member.json, if any
}

// mirrorAll brings every secondary up to date with the primary f. A
// secondary that fails or pauses doesn't stop the others; its status
// says what happened.
//
// A probe file written to each secondary makes sure it is not the
// primary, nor inside it, however it was reached (another sign-in to the
// same account, a shortcut, a local path): it must not be readable at the
// same path in the primary, nor seen anywhere in the primary's listing. A
// secondary that holds the primary is refused for the .medialayer inside
// it. Nothing is written to the primary, which may be full. Probes are
// kept and rewritten in place, one per device and folder, so Drive's own
// trash doesn't fill with them.
func (c *Core) mirrorAll(ctx context.Context, f fs.Fs) error {
	c.mu.Lock()
	secs := append([]source(nil), c.settings.Secondaries...)
	c.mu.Unlock()
	c.mu.Lock()
	listed := c.settings.ListedMembers
	device := c.settings.DeviceID
	c.mu.Unlock()
	if len(secs) == 0 && !listed {
		return nil // leave members other devices may have listed
	}
	repo, err := c.updateMembers(ctx, f, secs)
	if err != nil {
		return fmt.Errorf("update the library's members: %w", err)
	}
	if listed != (len(secs) > 0) {
		c.mu.Lock()
		c.settings.ListedMembers = len(secs) > 0
		err := c.saveSettings()
		c.mu.Unlock()
		if err != nil {
			return err
		}
	}
	if len(secs) == 0 {
		return nil
	}
	c.setJob(func(j *jobStatus) { j.Phase, j.Done, j.Total, j.Current = PhaseMirroring, 0, 0, "" })
	run := index.NewID()

	fail := func(s source, err error) {
		fs.Errorf(nil, "secondary %s: %v", s.Folder, err)
		c.setMirror(s.ID, func(m *mirrorState) { m.state, m.err, m.approved = MirrorError, err.Error(), "" })
	}
	var targets []target
	for _, s := range secs {
		if err := ctx.Err(); err != nil {
			return err
		}
		t, err := c.check(ctx, f, s, repo, device, run)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			fail(s, err)
			continue
		}
		targets = append(targets, t)
	}
	if len(targets) == 0 {
		return nil
	}
	src, err := listLibrary(ctx, f, false)
	if err != nil {
		return fmt.Errorf("list %s: %w", f.Root(), err)
	}
	for _, t := range targets {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Every copy of its probe, as an old one may sit elsewhere in the
		// primary too.
		var inside error
		for _, p := range src.probes[path.Base(t.probe)] {
			b, err := readSmall(ctx, f, p)
			if err != nil && !absent(err) {
				inside = err
				break
			}
			if err == nil && string(b) == run {
				inside = errors.New("this folder is inside the primary; pick one outside it")
				break
			}
		}
		if inside != nil {
			fail(t.s, inside)
			continue
		}
		c.setMirror(t.s.ID, func(m *mirrorState) { m.state, m.err = MirrorSyncing, "" })
		err := c.mirror(ctx, f, src, repo, t)
		if err != nil && ctx.Err() != nil {
			c.setMirror(t.s.ID, func(m *mirrorState) { m.state = "" })
			return ctx.Err()
		}
		if err != nil {
			fail(t.s, err)
		}
	}
	return nil
}

// check opens a secondary and makes sure it is safe to write: empty or a
// copy of this library (or a first copy of it that was cut short), and
// not the primary, nor holding it. It then writes the secondary's probe
// file and, before the first copy, its member record.
func (c *Core) check(ctx context.Context, f fs.Fs, s source, repo *repoRecord, device, run string) (target, error) {
	t := target{s: s, probe: repoDir + "/probe/" + device + "-" + s.ID}
	dst, err := openSource(ctx, s)
	if err != nil {
		return t, err
	}
	t.dst = dst
	have, err := listLibrary(ctx, dst, true)
	if err != nil {
		return t, fmt.Errorf("list %s: %w", s.Folder, err)
	}
	t.have = have
	if have.nested {
		return t, errors.New("this folder holds a MediaLayer library inside it, perhaps the primary; pick one outside it")
	}
	theirs, err := readRepo(ctx, dst)
	if err != nil {
		return t, err
	}
	// member.json is written before the first copy, so a copy that was
	// cut short, with files but no repo.json yet, is still recognised.
	var mine memberRecord
	if b, err := readSmall(ctx, dst, repoDir+"/member.json"); err == nil {
		if err := json.Unmarshal(b, &mine); err != nil {
			return t, fmt.Errorf("member.json: %w", err)
		}
	} else if !absent(err) {
		return t, err
	}
	switch {
	case theirs == nil && mine.LibraryID != "" && mine.LibraryID != repo.LibraryID:
		return t, errors.New("this folder holds part of a different MediaLayer library; pick an empty folder")
	case theirs == nil && mine.LibraryID == "" && len(have.files) > 0:
		return t, errors.New("this folder isn't empty and isn't a copy of this library; pick an empty folder")
	case theirs != nil && theirs.LibraryID != repo.LibraryID:
		return t, errors.New("this folder holds a different MediaLayer library; pick an empty folder or a copy of this library")
	case theirs != nil && theirs.Generation > repo.Generation:
		return t, fmt.Errorf("this folder names a newer primary (generation %d, this device knows %d), so it was left alone",
			theirs.Generation, repo.Generation)
	}
	if err := putBytes(ctx, dst, t.probe, []byte(run)); err != nil {
		return t, fmt.Errorf("write a probe file: %w", err)
	}
	b, err := readSmall(ctx, f, t.probe)
	if err != nil && !absent(err) {
		return t, err // can't tell whether it's the primary
	}
	if err == nil && string(b) == run {
		return t, errors.New("this folder is the primary itself")
	}
	if mine.LibraryID == repo.LibraryID {
		t.member = mine
	}
	return t, nil
}

// isFull reports errors that mean the cloud or disk has no room left.
func isFull(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.ENOSPC) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "storageQuotaExceeded") || (strings.Contains(msg, "quotaExceeded") && strings.Contains(msg, "storage"))
}

// absent reports errors that mean a file or folder isn't there.
func absent(err error) bool {
	return errors.Is(err, fs.ErrorObjectNotFound) || errors.Is(err, fs.ErrorDirNotFound)
}

// memberRecord is a secondary's .medialayer/member.json: which library
// it is a copy of, and whether the last sync to it finished. It says
// "copying" from before the first change of a sync until the sync ends,
// so a reader can tell a finished copy from one in progress or cut
// short; repo.json alone only says what it was last a copy of.
type memberRecord struct {
	LibraryID string    `json:"library_id"`
	Replica   string    `json:"replica"`
	State     string    `json:"state"`              // "copying" or "synced"
	Snapshot  string    `json:"snapshot,omitempty"` // the primary's snapshot it matches, when synced
	SyncedAt  time.Time `json:"synced_at,omitzero"`
}

// readSmall reads a small file.
func readSmall(ctx context.Context, f fs.Fs, remote string) ([]byte, error) {
	o, err := f.NewObject(ctx, remote)
	if err != nil {
		return nil, err
	}
	rc, err := o.Open(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	return io.ReadAll(io.LimitReader(rc, 1<<16))
}

// updateMembers makes the primary's repo.json list exactly the primary and
// these secondaries, and returns it. (Each device lists its own
// secondaries for now; sharing them between devices comes with the
// lease.)
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
		want = append(want, member{Replica: s.ID, Type: s.sourceType(), Remote: s.Remote, Folder: s.Folder})
	}
	same := len(want) == len(repo.Members)
	for i := 0; same && i < len(want); i++ {
		same = want[i] == repo.Members[i]
	}
	if same {
		return repo, nil
	}
	repo.Members = want
	if err := c.writeRepo(ctx, f, repo); err != nil {
		if !isFull(err) {
			return nil, err
		}
		// The secondaries still get the new list in their copy.
		fs.Errorf(f, "can't update members in the full primary's repo.json: %v", err)
	}
	return repo, nil
}

// writeRepo uploads repo.json to f.
func (c *Core) writeRepo(ctx context.Context, f fs.Fs, repo *repoRecord) error {
	b, err := json.MarshalIndent(repo, "", "  ")
	if err != nil {
		return err
	}
	// Put replaces an existing repo.json in place, on Drive too.
	return putBytes(ctx, f, repoDir+"/repo.json", b)
}

// planSignature identifies a set of removals and replacements.
func planSignature(lines []string) string {
	sort.Strings(lines)
	h := sha256.New()
	for _, l := range lines {
		h.Write([]byte(l + "\n"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// mirror makes one checked secondary match the primary f, whose files
// are src.
func (c *Core) mirror(ctx context.Context, f fs.Fs, src *listing, repo *repoRecord, t target) error {
	dst, have, s := t.dst, t.have, t.s
	// Paths are matched in Unicode NFC, as HFS+ disks keep names
	// decomposed and Drive composed, and on local disks also by case, as
	// a case-blind disk keeps a folder's old case when the primary renames
	// it by case only.
	key := func(p string) string { return norm.NFC.String(p) }
	if s.Remote == "" {
		key = func(p string) string { return strings.ToLower(norm.NFC.String(p)) }
	}
	// Paths the primary lists twice (Drive), or twice under one key, are
	// left alone on both sides until fixed there.
	skip := map[string]bool{}
	srcKeys := map[string]string{}
	for p := range src.files {
		k := key(p)
		if q, ok := srcKeys[k]; ok {
			skip[k] = true
			fs.Errorf(nil, "secondary %s: skipped %s and %s, which it can't keep apart", s.Folder, p, q)
		}
		srcKeys[k] = p
	}
	for p := range src.dupes {
		skip[key(p)] = true
		fs.Errorf(nil, "secondary %s: skipped %s, which the primary lists twice", s.Folder, p)
	}
	haveKeys := map[string]fs.Object{}
	for p, d := range have.files {
		haveKeys[key(p)] = d
	}
	lookup := func(p string) (fs.Object, bool) { d, ok := haveKeys[key(p)]; return d, ok }

	var copies, replaces []string
	var deletes []fs.Object
	var lines []string
	for p, o := range src.files {
		if skip[key(p)] {
			continue
		}
		d, ok := lookup(p)
		switch {
		case !ok:
			copies = append(copies, p)
		case !operations.Equal(ctx, o, d):
			replaces = append(replaces, p)
			lines = append(lines, "R "+p)
		}
	}
	for p, d := range have.files {
		if _, ok := srcKeys[key(p)]; !ok {
			deletes = append(deletes, d)
			lines = append(lines, "D "+p)
		}
	}
	for _, d := range have.extra {
		if !skip[key(d.Remote())] {
			deletes = append(deletes, d)
			lines = append(lines, "D "+d.Remote()+" (duplicate)")
		}
	}
	changes := len(replaces) + len(deletes)
	sig := planSignature(lines)
	approved := ""
	c.setMirror(s.ID, func(m *mirrorState) { approved, m.approved = m.approved, "" }) // good for one sync
	if changes > 0 && float64(changes) > massChange*float64(max(len(src.files), 1)) && sig != approved {
		c.setMirror(s.ID, func(m *mirrorState) {
			m.state, m.pending = MirrorPaused, sig
			m.copies, m.replaces, m.deletes = len(copies), len(replaces), len(deletes)
		})
		return nil
	}

	writeMember := func(m memberRecord) error {
		b, _ := json.Marshal(m)
		if err := putBytes(ctx, dst, repoDir+"/member.json", b); err != nil {
			return fmt.Errorf("write member.json: %w", err)
		}
		return nil
	}
	member := memberRecord{LibraryID: repo.LibraryID, Replica: s.ID, State: "copying"}
	if len(copies)+changes > 0 || t.member.State != "synced" {
		if err := writeMember(member); err != nil {
			return err
		}
		t.member = member
	}
	c.setJob(func(j *jobStatus) { j.Done, j.Total, j.Current = 0, int64(len(copies)+changes), "" })
	// One trash folder per sync, unique even within a second.
	stamp := time.Now().UTC().Format(trashStamp) + "-" + strings.ToLower(index.NewID()[20:])
	toTrash := func(o fs.Object) error {
		_, err := operations.Move(ctx, dst, nil, trashDir+"/"+stamp+"/"+o.Remote(), o)
		if err != nil {
			return fmt.Errorf("move %s to trash: %w", o.Remote(), err)
		}
		return nil
	}
	step := func(p string) {
		c.setJob(func(j *jobStatus) { j.Done++; j.Current = path.Base(p) })
	}
	for _, d := range deletes {
		if err := toTrash(d); err != nil {
			return err
		}
		step(d.Remote())
	}
	for _, p := range replaces {
		old, _ := lookup(p)
		if err := toTrash(old); err != nil {
			return err
		}
		if _, err := operations.Copy(ctx, dst, nil, p, src.files[p]); err != nil {
			return fmt.Errorf("copy %s: %w", p, err)
		}
		step(p)
	}
	for _, p := range copies {
		if _, err := operations.Copy(ctx, dst, nil, p, src.files[p]); err != nil {
			return fmt.Errorf("copy %s: %w", p, err)
		}
		step(p)
	}
	snap, err := c.mirrorRepo(ctx, f, dst, repo)
	if err != nil {
		return fmt.Errorf("copy .medialayer: %w", err)
	}
	if t.member.State != "synced" || t.member.Snapshot != snap {
		member.State, member.Snapshot, member.SyncedAt = "synced", snap, time.Now().UTC()
		if err := writeMember(member); err != nil {
			return err
		}
	}
	if err := emptyTrash(ctx, dst, time.Now()); err != nil {
		fs.Errorf(dst, "empty trash: %v", err)
	}
	c.setMirror(s.ID, func(m *mirrorState) {
		m.state, m.err, m.synced, m.pending = MirrorSynced, "", time.Now().UTC(), ""
		m.copies, m.replaces, m.deletes = len(copies), len(replaces), len(deletes)
	})
	return nil
}

// mirrorRepo copies the primary's art packs, newest snapshot and
// repo.json to dst, repo.json last, so a secondary with a repo.json can
// always open the library. It returns the snapshot's name.
func (c *Core) mirrorRepo(ctx context.Context, f, dst fs.Fs, repo *repoRecord) (string, error) {
	snapName := ""
	for _, dir := range []string{"art", "snapshots"} {
		ours, err := listRepoDir(ctx, f, dir)
		if err != nil {
			return "", err
		}
		theirs, err := listRepoDir(ctx, dst, dir)
		if err != nil {
			return "", err
		}
		if dir == "snapshots" {
			snap, _ := newestSnapshot(ours)
			if snap == nil {
				return "", errors.New("the primary has no snapshot yet")
			}
			snapName = path.Base(snap.Remote())
			ours = map[string]fs.Object{snapName: snap}
		}
		for name, o := range ours {
			if t, ok := theirs[name]; ok && t.Size() == o.Size() {
				continue // packs and snapshots never change once written
			}
			if _, err := operations.Copy(ctx, dst, theirs[name], o.Remote(), o); err != nil {
				return "", err
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
	return snapName, c.writeRepo(ctx, dst, repo)
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
		stamp, _, _ := strings.Cut(path.Base(d.Remote()), "-")
		t, err := time.Parse(trashStamp, stamp)
		if err != nil || now.Sub(t) < trashKeep {
			continue
		}
		if err := operations.Purge(ctx, dst, d.Remote()); err != nil {
			return err
		}
	}
	return nil
}
