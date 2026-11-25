package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/rclone/rclone/fs"
)

// Promote makes a secondary the library's primary. It is allowed only
// when the secondary is a complete copy of the current index version: its
// member.json says synced at this device's version and the current
// generation. The generation is raised by one, the new repo.json is
// written to the new primary first (which must succeed) and then to the
// old primary, which becomes a secondary. Promoting the old primary back
// works the same way, once a sync has made it a synced secondary.
//
// Files are not moved or copied: a synced secondary already holds them.
func (c *Core) Promote(id string) error {
	c.mu.Lock()
	if c.jobDone != nil {
		c.mu.Unlock()
		return errors.New("the library is syncing; wait for it to finish")
	}
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

	ctx := context.Background()
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
	dst, err := openSource(ctx, target)
	if err != nil {
		return err
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
	c.fsys = nil
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
