package core

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rclone/rclone/fs"
)

// Phases of the sync job, as Status reports them.
const (
	PhaseIdle       = "idle"
	PhaseOpening    = "opening"    // restoring the index from the cloud
	PhaseListing    = "listing"    // listing the library folder
	PhaseIndexing   = "indexing"   // reading headers of new and changed files
	PhasePublishing = "publishing" // writing .medialayer to the cloud
	PhaseMirroring  = "mirroring"  // copying changes to the secondaries
	PhaseDone       = "done"
	PhaseError      = "error"
)

// jobStatus is the progress of the sync job.
type jobStatus struct {
	Phase    string    `json:"phase"`
	Done     int64     `json:"done"`  // files handled in this phase
	Total    int64     `json:"total"` // files to handle in this phase
	Failed   int64     `json:"failed"`
	Current  string    `json:"current,omitempty"` // file being read
	Error    string    `json:"error,omitempty"`   // why the job stopped, in PhaseError
	Failures []string  `json:"failures,omitempty"`
	Started  time.Time `json:"started,omitzero"`
	Finished time.Time `json:"finished,omitzero"`
}

// Status is what the app shows and polls.
type Status struct {
	Connected  bool      `json:"connected"`
	Folder     string    `json:"folder"`
	DBPath     string    `json:"db_path"`
	DBEpoch    int64     `json:"db_epoch"` // changes when the database file is replaced
	ArtDir     string    `json:"art_dir"`
	StreamBase string    `json:"stream_base"` // append "<file id>.<format>"
	Tracks     int64     `json:"tracks"`
	Job        jobStatus `json:"job"`
	// Sources are the library's folders, primary first.
	Sources []SourceStatus `json:"sources"`
	// Accounts are the connected Google accounts (rclone remotes), the
	// primary's first.
	Accounts []string `json:"accounts"`
	Notice   string   `json:"notice,omitempty"`
}

// Status returns the current state.
func (c *Core) Status() Status {
	c.mu.Lock()
	s := Status{
		Connected:  c.Connected(),
		Folder:     c.settings.Folder,
		DBEpoch:    c.dbEpoch,
		ArtDir:     c.artDir(),
		StreamBase: c.stream.base(),
		Job:        c.job,
	}
	s.Job.Failures = append([]string(nil), c.job.Failures...)
	s.Sources = c.sourcesLocked()
	s.Accounts = driveAccounts()
	s.Notice = c.notice
	if s.Job.Phase == "" {
		s.Job.Phase = PhaseIdle
	}
	if c.db != nil {
		s.DBPath = c.db.Path()
		s.Tracks = c.tracks.Load()
	}
	c.mu.Unlock()
	return s
}

// setJob updates the job status under the lock.
func (c *Core) setJob(update func(j *jobStatus)) {
	c.mu.Lock()
	update(&c.job)
	c.mu.Unlock()
}

// StartSync starts opening, indexing and publishing the library folder in
// the background. It does nothing if that is already running.
func (c *Core) StartSync() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.jobDone != nil {
		return nil
	}
	if c.settings.Folder == "" {
		return errors.New("no library folder picked")
	}
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	c.jobDone, c.cancel = done, cancel
	c.job = jobStatus{Phase: PhaseOpening, Started: time.Now()}
	c.notice = ""
	go func() {
		defer close(done)
		err := c.sync(ctx)
		if db := c.currentDB(); db != nil {
			c.countTracks(db)
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		cancel()
		c.jobDone, c.cancel = nil, nil
		c.job.Current = ""
		c.job.Finished = time.Now()
		if err != nil {
			c.job.Phase = PhaseError
			c.job.Error = err.Error()
		} else {
			c.job.Phase = PhaseDone
		}
	}()
	return nil
}

// sync restores, indexes and publishes, in that order.
func (c *Core) sync(ctx context.Context) error {
	f, err := c.libraryFs(ctx)
	if err != nil {
		return err
	}
	if c.currentDB() == nil {
		// An earlier failure left no index open; start again from an empty one.
		c.mu.Lock()
		err := c.openDB()
		c.mu.Unlock()
		if err != nil {
			return err
		}
	}
	// A second pass happens only when another device published the
	// library while this one indexed it from scratch: this device then
	// takes that snapshot, keeping its file ids, and indexes on top.
	for range 2 {
		if err := c.restore(ctx, f); err != nil {
			return fmt.Errorf("open the library's .medialayer: %w", err)
		}
		if err := c.index(ctx, f); err != nil {
			return err
		}
		err := c.publish(ctx, f)
		if errors.Is(err, errPublishedElsewhere) {
			continue
		}
		if isFull(err) {
			// Sync only writes to the secondaries, so they still get the
			// primary's last saved snapshot and every file.
			fs.Errorf(f, "publish: %v", err)
			c.mu.Lock()
			c.notice = "The primary's cloud is full, so the library's latest changes couldn't be saved there. Copies still sync from it."
			c.mu.Unlock()
			err = nil
		}
		if err != nil {
			return fmt.Errorf("write the library's .medialayer: %w", err)
		}
		return c.mirrorAll(ctx, f)
	}
	return errPublishedElsewhere
}
