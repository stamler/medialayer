package core

import (
	"context"
	"fmt"

	"github.com/rclone/rclone/fs"

	"github.com/stamler/medialayer/internal/index"
)

// The hash pass gives every track of the primary its audio hash (see
// audiohash.go), in the background, one file at a time. It starts when a
// sync ends well and stops when the next job starts, as jobs restore and
// rewrite the index; the next sync starts it again, carrying on with the
// files still without a hash. Hashes reach the index as they are made,
// and the index is marked changed so the next sync publishes them.
//
// Each file is read in full, so the first pass downloads the library
// once. A file that can't be hashed (damaged, say) is tried again only in
// a later session.

// hashFormats are the formats the pass hashes.
var hashFormats = []string{"flac", "mp3", "m4a", "caf", "oga"}

// AudioHashStatus is the hash pass's progress, as Status reports it.
type AudioHashStatus struct {
	Running bool  `json:"running"`
	Hashed  int64 `json:"hashed"` // tracks with an audio hash
	Total   int64 `json:"total"`  // tracks the pass hashes
	Failed  int64 `json:"failed"` // tracks it couldn't hash this session
}

// hashState is the hash pass's state in Core, under c.mu.
type hashState struct {
	cancel context.CancelFunc // stops the pass; nil once it was told to stop
	// done is closed when the pass's goroutine has ended; nil if none
	// runs. A stopped pass keeps it until it has ended, so no second pass
	// starts beside it and Close waits for it.
	done chan struct{}
	// again starts a new pass when the stopping one ends.
	again  bool
	status AudioHashStatus
	known  bool            // status has been counted
	failed map[string]bool // by hashFailKey, for this session
}

func hashFailKey(path string, size, mtime int64) string {
	return fmt.Sprintf("%s\x00%d\x00%d", path, size, mtime)
}

// startHashPassLocked starts the pass if it isn't running. The caller
// holds c.mu, and no job runs.
func (c *Core) startHashPassLocked() {
	if c.closed || c.settings.Folder == "" || c.db == nil || c.jobDone != nil {
		return
	}
	if c.hash.done != nil {
		if c.hash.cancel == nil {
			c.hash.again = true // after the stopping one ends
		}
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	c.hash.cancel, c.hash.done, c.hash.again = cancel, done, false
	c.hash.status.Running = true
	db := c.db
	go func() {
		defer close(done)
		err := c.hashPass(ctx, db)
		if err != nil && ctx.Err() == nil {
			fs.Errorf(nil, "audio hash pass: %v", err)
		}
		cancel()
		c.mu.Lock()
		defer c.mu.Unlock()
		c.hash.cancel, c.hash.done = nil, nil
		c.hash.status.Running = false
		if c.hash.again {
			c.hash.again = false
			c.startHashPassLocked()
		}
	}()
}

// stopHashPassLocked tells the pass to stop, if one runs. It ends soon
// after; waitHashPass waits for that. The caller holds c.mu.
func (c *Core) stopHashPassLocked() {
	if c.hash.cancel != nil {
		c.hash.cancel()
	}
	c.hash.cancel, c.hash.again = nil, false
	c.hash.status.Running = false
}

// waitHashPass waits until no pass runs.
func (c *Core) waitHashPass() {
	for {
		c.mu.Lock()
		done := c.hash.done
		c.mu.Unlock()
		if done == nil {
			return
		}
		<-done
	}
}

// hashWriteOK runs write while holding hashWrite, unless the pass was
// told to stop. Jobs take hashWrite after stopping the pass, so no pass
// write lands during a job.
func (c *Core) hashWriteOK(ctx context.Context, write func() error) (bool, error) {
	c.hashWrite.Lock()
	defer c.hashWrite.Unlock()
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	return true, write()
}

// hashPass hashes the primary's files that have no audio hash yet,
// writing only to db, the index it started with.
func (c *Core) hashPass(ctx context.Context, db *index.DB) error {
	f, err := c.libraryFs(ctx)
	if err != nil {
		return err
	}
	total, hashed, err := db.AudioHashCounts(hashFormats...)
	if err != nil {
		return err
	}
	todo, err := db.FilesWithoutAudioSHA(hashFormats...)
	if err != nil {
		return err
	}
	// update changes the status, unless another library replaced this one.
	update := func(u func(h *hashState)) {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.db == db {
			u(&c.hash)
		}
	}
	update(func(h *hashState) {
		h.status.Total, h.status.Hashed, h.known = total, hashed, true
		if h.failed == nil {
			h.failed = map[string]bool{}
		}
	})
	defer func() {
		if err := c.saveAudioHashes(nil); err != nil {
			fs.Errorf(nil, "save %s: %v", audioHashFile, err)
		}
	}()
	marked := false
	for i, t := range todo {
		if c.hashFileHook != nil {
			c.hashFileHook()
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		key := hashFailKey(t.Path, t.Size, t.MTime)
		c.mu.Lock()
		skip := c.hash.failed[key]
		c.mu.Unlock()
		if skip {
			continue
		}
		o, err := f.NewObject(ctx, t.Path)
		if err != nil || o.Size() != t.Size || o.ModTime(ctx).UnixMilli() != t.MTime {
			continue // changed since it was indexed: the next sync indexes it again
		}
		sha, err := c.audioSHA(ctx, f, o)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			fs.Errorf(o, "audio hash: %v", err)
			update(func(h *hashState) {
				h.failed[key] = true
				h.status.Failed++
			})
			continue
		}
		stored := false
		if _, err := c.hashWriteOK(ctx, func() error {
			var err error
			if stored, err = storeAudioSHA(ctx, db, o, sha); err != nil || !stored || marked {
				return err
			}
			// The hashes go out with the next index version.
			marked = true
			return db.SetMeta("dirty", "1")
		}); err != nil {
			return err
		}
		if stored {
			update(func(h *hashState) { h.status.Hashed++ })
		}
		if i%50 == 49 {
			if err := c.saveAudioHashes(nil); err != nil {
				fs.Errorf(nil, "save %s: %v", audioHashFile, err)
			}
		}
	}
	return nil
}
