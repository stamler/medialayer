package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
)

// Phases of the sync job, as Status reports them.
const (
	PhaseIdle       = "idle"
	PhaseOpening    = "opening"    // restoring the index from the cloud
	PhaseListing    = "listing"    // listing the library folder
	PhaseIndexing   = "indexing"   // reading headers of new and changed files
	PhasePublishing = "publishing" // writing .medialayer to the cloud
	PhaseMirroring  = "mirroring"  // copying changes to the secondaries
	PhasePromoting  = "promoting"  // switching the primary
	PhaseWaiting    = "waiting"    // another device holds the sync lock; Holder names it
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
	Holder   string    `json:"holder,omitempty"`  // the device syncing, in PhaseWaiting
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
	// ProtonAccounts are the connected Proton accounts: at most one.
	ProtonAccounts []ProtonAccount `json:"proton_accounts"`
	Notice         string          `json:"notice,omitempty"`
	// AudioHashes is the background pass that hashes every track's audio,
	// once it has counted them.
	AudioHashes *AudioHashStatus `json:"audio_hashes,omitempty"`
	// Throttled is the clouds that asked us to slow down in the last
	// two minutes.
	Throttled []Throttle `json:"throttled,omitempty"`
	// Debug is whether debug logging is on; LogPath is the log file.
	Debug   bool   `json:"debug"`
	LogPath string `json:"log_path,omitempty"`
}

// Status returns the current state.
func (c *Core) Status() Status {
	c.mu.Lock()
	s := Status{
		Connected:  c.connectedLocked(),
		Folder:     c.settings.Folder,
		DBEpoch:    c.dbEpoch,
		ArtDir:     c.artDir(),
		StreamBase: c.stream.base(),
		Job:        c.job,
		Debug:      !c.settings.DebugOff,
		LogPath:    logging.path,
	}
	s.Job.Failures = append([]string(nil), c.job.Failures...)
	s.Sources = c.sourcesLocked()
	s.Accounts = driveAccounts(c.primaryRemote())
	s.ProtonAccounts = protonAccounts()
	s.Notice = c.notice
	if s.Notice == "" {
		s.Notice = c.keyNotice
	}
	if c.hash.known {
		h := c.hash.status
		s.AudioHashes = &h
	}
	if s.Job.Phase == "" {
		s.Job.Phase = PhaseIdle
	}
	if c.db != nil {
		s.DBPath = c.db.Path()
		s.Tracks = c.tracks.Load()
	}
	c.mu.Unlock()
	s.Throttled = recentThrottles(time.Now())
	return s
}

// setJob updates the job status under the lock.
func (c *Core) setJob(update func(j *jobStatus)) {
	c.mu.Lock()
	was := c.job.Phase
	update(&c.job)
	if now := c.job.Phase; now != was {
		// Each step's time goes to the log, so a slow sync or switch of
		// the primary shows where its time went.
		if was != "" && was != PhaseIdle && was != PhaseDone && was != PhaseError && !c.phaseStart.IsZero() {
			fs.Logf(nil, "sync: %s took %s", was, time.Since(c.phaseStart).Round(100*time.Millisecond))
		}
		c.phaseStart = time.Now()
	}
	c.mu.Unlock()
}

// StartSync starts opening, indexing and publishing the library folder in
// the background. It does nothing if that is already running.
func (c *Core) StartSync() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.jobDone != nil || c.closed {
		return nil
	}
	return c.startJobLocked(c.sync)
}

// startJobLocked runs work as the job. The caller holds c.mu and has
// checked that no job is running.
func (c *Core) startJobLocked(work func(context.Context) error) error {
	if c.settings.Folder == "" {
		return errors.New("no library folder picked")
	}
	if c.retry != nil {
		c.retry.Stop() // this job is the retry
	}
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	c.jobDone, c.cancel = done, cancel
	c.stopHashPassLocked() // jobs rewrite the index; the pass starts again after
	c.job = jobStatus{Phase: PhaseOpening, Started: time.Now()}
	c.phaseStart = c.job.Started
	c.notice = ""
	if config.GetValue(c.primaryRemote(), "type") == protonType {
		// A fresh client each job, with the session as last renewed: a
		// client kept all day may have lost its session to another client
		// of the account renewing it (docs/sync.md, Proton Drive).
		c.fsys = nil
		c.stream.forgetAll()
	}
	c.jobFs, c.listed, c.listedFs = map[string]fs.Fs{}, nil, nil
	c.takePrimary = false
	if c.fsys != nil {
		c.jobFs[fsKey(source{Remote: c.primaryRemote(), Folder: c.settings.Folder})] = c.fsys
	}
	go func() {
		defer close(done)
		// A write the stopped hash pass had begun ends first.
		c.hashWrite.Lock()
		c.hashWrite.Unlock() //nolint:staticcheck // a barrier
		err := work(ctx)
		if db := c.currentDB(); db != nil {
			c.countTracks(db)
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		cancel()
		c.jobDone, c.cancel, c.jobFs, c.listed, c.listedFs = nil, nil, nil, nil, nil
		c.job.Current = ""
		c.job.Finished = time.Now()
		fs.Logf(nil, "sync: %s took %s; the job took %s (%v)", c.job.Phase,
			c.job.Finished.Sub(c.phaseStart).Round(100*time.Millisecond),
			c.job.Finished.Sub(c.job.Started).Round(100*time.Millisecond), errOrOK(err))
		c.phaseStart = time.Time{}
		var busy *leaseBusyError
		switch {
		case errors.As(err, &busy):
			c.job.Phase, c.job.Holder = PhaseWaiting, busy.name
			c.retryLocked()
		case errors.Is(err, errLeaseLost):
			c.job.Phase = PhaseError
			c.job.Error = err.Error()
			c.retryLocked()
		case errors.Is(err, errSwitchFinished) && c.resync && !c.closed:
			c.resync = false
			c.job.Phase = PhaseDone
			if err := c.startJobLocked(c.sync); err != nil {
				c.job.Phase, c.job.Error = PhaseError, err.Error()
			}
			c.notice = "A switch of the primary to " + c.settings.Folder + " was cut short, so it was finished."
		case err != nil:
			c.job.Phase = PhaseError
			c.job.Error = err.Error()
		default:
			c.job.Phase = PhaseDone
			c.startHashPassLocked()
		}
	}()
	return nil
}

// retryLocked starts a sync after leaseRetry, for a device another one
// kept waiting. The caller holds c.mu.
func (c *Core) retryLocked() {
	if c.closed {
		return
	}
	if c.retry != nil {
		c.retry.Stop()
	}
	c.retry = time.AfterFunc(leaseRetry+leaseJitter(), func() { _ = c.StartSync() })
}

// sync restores, indexes and publishes, in that order, holding the
// library's sync lease.
func (c *Core) sync(ctx context.Context) error {
	f, err := c.libraryFs(ctx)
	if err != nil {
		return err
	}
	// Checked before taking the lease too, so nothing is written to a
	// folder that turns out not to be this device's primary.
	if err := c.checkPrimary(ctx, f); err != nil {
		return err
	}
	return c.withLease(ctx, f, c.syncHeld)
}

// syncHeld is sync, for a caller holding the lease on the primary.
func (c *Core) syncHeld(ctx context.Context) error {
	f, err := c.libraryFs(ctx)
	if err != nil {
		return err
	}
	if err := c.checkPrimary(ctx, f); err != nil {
		return err
	}
	// A second pass happens only when the primary holds a version this
	// device didn't make or take: another device published the library
	// while this one indexed it from scratch, or published a newer
	// version since. This device then takes that snapshot, keeping its
	// file ids, and indexes on top.
	for range 2 {
		if err := c.restore(ctx, f); err != nil {
			return fmt.Errorf("open the library's .medialayer: %w", err)
		}
		if err := c.index(ctx, f); err != nil {
			return err
		}
		if err := c.applyCarried(); err != nil {
			return fmt.Errorf("carry over playlists: %w", err)
		}
		err := c.publish(ctx, f)
		if errors.Is(err, errPublishedElsewhere) {
			continue
		}
		full := isFull(err)
		if full {
			// Sync only writes to the secondaries, so they still get the
			// primary's last saved snapshot and every file.
			fs.Errorf(f, "publish: %v", err)
			c.mu.Lock()
			c.notice = "The primary's cloud is full, so the library's latest changes couldn't be saved there. Copies still sync from it."
			c.mu.Unlock()
			err = nil
		}
		if err == nil {
			c.mu.Lock()
			c.primaryFull = full
			c.mu.Unlock()
		}
		if err != nil {
			return fmt.Errorf("write the library's .medialayer: %w", err)
		}
		return c.mirrorAll(ctx, f)
	}
	return errPublishedElsewhere
}

// checkPrimary makes sure f is still this device's primary. A device
// whose primary was promoted away on another device stops here rather
// than write to what is now a secondary.
func (c *Core) checkPrimary(ctx context.Context, f fs.Fs) error {
	if c.currentDB() == nil {
		// An earlier failure left no index open; start again from an empty one.
		c.mu.Lock()
		err := c.openDB()
		c.mu.Unlock()
		if err != nil {
			return err
		}
	}
	if repo, err := readRepo(ctx, f); err != nil {
		return fmt.Errorf("read the library's repo.json: %w", err)
	} else if repo != nil && repo.Primary.Replica != "" {
		c.mu.Lock()
		picked := c.settings.Picked
		c.mu.Unlock()
		if picked {
			// A folder just picked that a promotion made the primary takes
			// on the id it was given. Its member.json, written while it was
			// a secondary, names that id; a demoted primary's names its own.
			var m memberRecord
			b, err := readSmall(ctx, f, repoDir+"/member.json")
			if err != nil && !absent(err) {
				return fmt.Errorf("read the library's member.json: %w", err)
			}
			if err == nil && json.Unmarshal(b, &m) == nil && m.LibraryID == repo.LibraryID && m.Replica != repo.Primary.Replica {
				where := repo.Primary.Folder
				if repo.Primary.Remote != "" {
					where = repo.Primary.Remote + ":" + where
				}
				return fmt.Errorf("this folder is a copy (a secondary) of a library whose primary is %s; pick that folder", where)
			}
			c.mu.Lock()
			if m.LibraryID == repo.LibraryID && m.Replica != "" {
				c.settings.PrimaryID = repo.Primary.Replica
			}
			c.settings.Picked = false
			err = c.saveSettings()
			c.mu.Unlock()
			if err != nil {
				return err
			}
		}
		c.mu.Lock()
		mine := c.primaryID()
		c.mu.Unlock()
		if repo.Primary.Replica != mine && c.interruptedSwitch(repo, repo.LibraryID, c.knownGeneration()) {
			return c.finishSwitch(ctx, f, repo, mine, repo)
		}
		if repo.Primary.Replica != mine {
			return fmt.Errorf("the library's primary moved to %s on another device; this device doesn't follow a move yet", repo.Primary.Folder)
		}
	}
	return nil
}

func errOrOK(err error) string {
	if err != nil {
		return err.Error()
	}
	return "ok"
}
