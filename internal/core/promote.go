package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/rclone/rclone/fs"
)

// Promote makes a secondary the library's primary, as a job that runs in
// the background like a sync. The job first syncs the library, so the
// secondary holds every file the primary has now and nothing else, and
// only then checks it: its member.json must say synced at this device's
// index version under the current generation. The generation is raised by
// one, the new repo.json is written to the new primary first (which must
// succeed) and then to the old primary, the sources are swapped, and a
// second sync makes the old primary a secondary in step with the new one.
// That first sync of the old primary pauses before removing or replacing
// anything, since such a change would come from an edit on it made during
// the switch.
//
// Files are not moved or copied: a synced secondary already holds them.
func (c *Core) Promote(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.jobDone != nil {
		return errors.New("the library is syncing; wait for it to finish")
	}
	if c.closed {
		return errors.New("the core is closed")
	}
	found := false
	for _, s := range c.settings.Secondaries {
		found = found || s.ID == id
	}
	if !found {
		return fmt.Errorf("no secondary %s", id)
	}
	// The job holds the old primary's sync lease throughout, and the
	// target's from before the switch until the sync after it ends.
	return c.startJobLocked(func(ctx context.Context) error {
		f, err := c.libraryFs(ctx)
		if err != nil {
			return err
		}
		if err := c.checkPrimary(ctx, f); err != nil {
			return err
		}
		return c.withLease(ctx, f, func(ctx context.Context) error { return c.promoteHeld(ctx, id) })
	})
}

// promoteHeld is Promote's job, holding the primary's sync lease.
func (c *Core) promoteHeld(ctx context.Context, id string) error {
	if err := c.syncHeld(ctx); err != nil {
		return err
	}
	c.mu.Lock()
	m := c.mirrors[id]
	c.mu.Unlock()
	if m == nil || m.state != MirrorSynced {
		why := "its sync didn't finish"
		if m != nil && m.err != "" {
			why = m.err
		} else if m != nil && m.state == MirrorPaused {
			why = "its sync is paused for review"
		}
		return fmt.Errorf("couldn't make it the primary: %s", why)
	}
	var target source
	c.mu.Lock()
	for _, s := range c.settings.Secondaries {
		if s.ID == id {
			target = s
		}
	}
	c.mu.Unlock()
	if target.ID == "" {
		return fmt.Errorf("no secondary %s", id)
	}
	dst, err := openSource(ctx, target)
	if err != nil {
		return err
	}
	return c.withLease(ctx, dst, func(ctx context.Context) error {
		c.setJob(func(j *jobStatus) { j.Phase, j.Done, j.Total, j.Current = PhasePromoting, 0, 0, "" })
		if err := c.swapPrimary(ctx, id, dst); err != nil {
			return err
		}
		return c.syncHeld(ctx)
	})
}

// swapPrimary makes secondary id the primary. It runs inside the job, so
// nothing else changes the settings meanwhile.
func (c *Core) swapPrimary(ctx context.Context, id string, dst fs.Fs) error {
	c.mu.Lock()
	var target source
	var others []source
	for _, s := range c.settings.Secondaries {
		if s.ID == id {
			target = s
		} else {
			others = append(others, s)
		}
	}
	oldPrimary := source{Remote: c.primaryRemote(), Folder: c.settings.Folder}
	if oldPrimary.Remote == localPrimary {
		oldPrimary.Remote = ""
	}
	c.mu.Unlock()
	if target.ID == "" {
		return fmt.Errorf("no secondary %s", id)
	}
	f, err := c.libraryFs(ctx)
	if err != nil {
		return err
	}
	repo, err := readRepo(ctx, f)
	if err != nil {
		return fmt.Errorf("read the primary's repo.json: %w", err)
	}
	if repo == nil {
		return errors.New("the primary has no repo.json")
	}
	if err := c.promotable(ctx, dst, repo); err != nil {
		return err
	}

	// The new record: the target is primary, the old primary a member.
	oldPrimary.ID = repo.Primary.Replica
	if oldPrimary.ID == "" {
		oldPrimary.ID = replica
	}
	next := *repo
	next.Generation = repo.Generation + 1
	next.Primary = member{Replica: target.ID, Type: target.sourceType(), Remote: target.Remote, Folder: target.Folder}
	next.ChangedAt = time.Now().UTC()
	next.ChangedBy = c.deviceID()
	next.Members = []member{next.Primary}
	for _, s := range append(others, oldPrimary) {
		next.Members = append(next.Members, member{Replica: s.ID, Type: s.sourceType(), Remote: s.Remote, Folder: s.Folder})
	}
	if err := c.writeRepo(ctx, dst, &next); err != nil {
		return fmt.Errorf("write repo.json to the new primary: %w", err)
	}
	// Best effort: a full old primary keeps its old repo.json until a
	// sync can write it, and the higher generation on the new one wins.
	if err := c.writeRepo(ctx, f, &next); err != nil {
		fs.Errorf(f, "write the new repo.json to the old primary: %v", err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.settings.PrimaryRemote, c.settings.Folder, c.settings.PrimaryID = target.Remote, target.Folder, target.ID
	if target.Remote == "" {
		c.settings.PrimaryRemote = localPrimary
	}
	c.settings.Secondaries = append(others, oldPrimary)
	c.settings.ListedMembers = true
	c.settings.ReviewFirst = oldPrimary.ID
	c.fsys = dst // the client that holds the target's lease, not another
	delete(c.mirrors, target.ID)
	c.stream.forgetAll()
	if err := c.saveSettings(); err != nil {
		return err
	}
	if db := c.db; db != nil {
		_ = db.SetMeta("generation", fmt.Sprint(next.Generation))
		// The new primary holds this version already.
		if v, err := metaInt(db, "version"); err == nil {
			_ = db.SetMeta("primary_version", fmt.Sprint(v))
		}
	}
	return nil
}

// promotable checks that dst is a complete copy of the index's current
// version under the current generation.
func (c *Core) promotable(ctx context.Context, dst fs.Fs, repo *repoRecord) error {
	b, err := readSmall(ctx, dst, repoDir+"/member.json")
	if err != nil {
		return fmt.Errorf("this folder has no readable member.json, so it can't be checked: %w", err)
	}
	var m memberRecord
	if err := json.Unmarshal(b, &m); err != nil {
		return fmt.Errorf("member.json: %w", err)
	}
	version, err := metaInt(c.currentDB(), "version")
	if err != nil {
		return err
	}
	switch {
	case m.LibraryID != repo.LibraryID:
		return errors.New("this folder is a copy of a different library")
	case m.State != "synced":
		return fmt.Errorf("this folder's last sync didn't finish (it is %s); sync it first", m.State)
	case m.Version != version:
		return fmt.Errorf("this folder holds index version %d, but the current one is %d; sync it first", m.Version, version)
	case m.Generation != repo.Generation:
		return fmt.Errorf("this folder was synced under generation %d, but the current one is %d; sync it first", m.Generation, repo.Generation)
	}
	theirs, err := readRepo(ctx, dst)
	if err != nil {
		return err
	}
	if theirs == nil || theirs.Generation > repo.Generation {
		return errors.New("this folder's repo.json is missing or names a newer primary; sync it first")
	}
	return nil
}

func (c *Core) deviceID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.settings.DeviceID
}

// primaryID is the primary's member id. The caller holds c.mu.
func (c *Core) primaryID() string {
	if c.settings.PrimaryID == "" {
		return replica
	}
	return c.settings.PrimaryID
}

// primaryType is the primary's rclone backend, such as "drive".
func (c *Core) primaryType() string {
	c.mu.Lock()
	r := c.primaryRemote()
	c.mu.Unlock()
	if r == localPrimary {
		return "local"
	}
	return source{Remote: r}.sourceType()
}

// localPrimary is PrimaryRemote for a primary in a local folder.
const localPrimary = "-local-"

// primaryRemote is the rclone remote of the primary. The caller holds
// c.mu.
func (c *Core) primaryRemote() string {
	if c.settings.PrimaryRemote == "" {
		return remoteName
	}
	return c.settings.PrimaryRemote
}
