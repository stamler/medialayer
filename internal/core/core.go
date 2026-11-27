// Package core is MediaLayer's Go core for the player apps: it connects a
// cloud, indexes a music folder into SQLite, keeps the library's
// .medialayer repository on the cloud, and streams tracks to the player
// over a localhost HTTP endpoint.
//
// The apps read the SQLite index directly; the core only fetches from the
// cloud and serves byte ranges. Everything here runs in the app's process.
package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/rclone/rclone/backend/drive" // Google Drive
	"github.com/rclone/rclone/fs"

	"github.com/stamler/medialayer/internal/index"
)

// remoteName is the rclone remote the core creates for Google Drive, in
// its own rclone config file (not the user's).
const remoteName = "gdrive"

// Core is one library on one device.
type Core struct {
	dataDir string

	mu sync.Mutex
	// playlistMu serialises playlist changes with restore; take it
	// before mu.
	playlistMu sync.Mutex
	settings   settings
	fsys       fs.Fs     // the library folder, nil until one is picked
	db         *index.DB // nil until a folder is picked
	dbEpoch    int64     // raised each time the database file is replaced
	job        jobStatus
	jobDone    chan struct{}      // closed when the running job ends; nil if none
	phaseStart time.Time          // when the job's current phase began
	resync     bool               // start a sync when the job ends (errSwitchFinished)
	cancel     context.CancelFunc // stops the running job
	// tracks is the index's track count, kept here so Status never waits
	// on the database while the indexer writes.
	tracks atomic.Int64
	// protonKeyWait is set while a Proton sign-in waits for a security
	// key's touch.
	protonKeyWait atomic.Int32       // a KeyTouch
	protonCancel  context.CancelFunc // ends the Proton sign-in under way; under mu
	protonPending *protonSignIn      // a sign-in waiting for an answer; under mu
	// badCovers are folder images that failed to read or decode, so later
	// syncs in this session don't download them again.
	badCovers map[string]bool
	mirrors   map[string]*mirrorState // by secondary id
	// audioHashes are the audio hashes made on this device, by audioKey,
	// loaded from audio-hashes.json on first use.
	audioHashes      map[string]string
	audioHashesDirty bool
	hash             hashState // the background hash pass
	// hashWrite is held by the hash pass while it writes the index; a job
	// takes it once after stopping the pass, so no write lands after.
	hashWrite sync.Mutex
	// audioFileMu serialises writes of audio-hashes.json.
	audioFileMu sync.Mutex
	// hashFileHook, if set (by tests), runs before the pass reads each
	// file.
	hashFileHook func()
	notice       string // something the user should know, until the next sync
	// keyNotice says the saved sign-ins were set aside at start-up. It
	// shows for the session, as the first sync starts at once.
	keyNotice string
	// primaryFull is set when the last publish found the primary's cloud
	// full, so a sync can't save changes there until space is freed.
	primaryFull bool
	stream      *streamServer
	// leaseSeen is when each other device's claim on a sync lease was
	// first seen as it is now, by folder and file.
	leaseSeen map[string]leaseSighting
	// retry starts the next sync after another device kept this one
	// waiting; closed stops it for good.
	retry  *time.Timer
	closed bool
}

// settings are kept in settings.json in the data directory.
type settings struct {
	Folder string `json:"folder"` // the primary's library folder, e.g. "Music"
	// PrimaryRemote is the primary's rclone remote; "" means remoteName.
	PrimaryRemote string `json:"primary_remote,omitempty"`
	// PrimaryID is the primary's member id in repo.json; "" means replica.
	PrimaryID string `json:"primary_id,omitempty"`
	DeviceID  string `json:"device_id"` // random, made once per install
	// DBFile is the local index's file name. A restore writes a new file
	// rather than replacing this one under the app's open connection.
	DBFile string `json:"db_file,omitempty"`
	// Secondaries hold copies of the library, synced one way from the
	// primary (the Folder above).
	Secondaries []source `json:"secondaries,omitempty"`
	// ListedMembers is set while this device has listed secondaries in the
	// primary's repo.json, so it clears them there when the last goes.
	ListedMembers bool `json:"listed_members,omitempty"`
	// ReviewFirst is a demoted primary whose next sync pauses before any
	// removal or replacement, as anything it would change there was
	// changed on it after the last check.
	ReviewFirst string `json:"review_first,omitempty"`
	// Picked is set from picking the folder until the first sync reads
	// its repository.
	Picked bool `json:"picked,omitempty"`
	// DebugOff turns off debug logging, which is on until then.
	DebugOff bool `json:"debug_off,omitempty"`
}

// New opens the core with its files in dataDir and its sign-ins
// unencrypted. The apps use Open.
func New(dataDir string) (*Core, error) { return Open(dataDir, "") }

// Open opens the core with its files in dataDir (on macOS,
// ~/Library/Application Support/MediaLayer), its sign-ins encrypted with
// configKey (see setupConfig). It starts the streaming endpoint and, if a
// folder was picked before, opens its index.
func Open(dataDir, configKey string) (*Core, error) {
	if err := os.MkdirAll(filepath.Join(dataDir, "art"), 0o700); err != nil {
		return nil, err
	}
	notice, err := setupConfig(filepath.Join(dataDir, "rclone.conf"), configKey)
	if err != nil {
		return nil, err
	}

	c := &Core{dataDir: dataDir, keyNotice: notice}
	if err := c.loadSettings(); err != nil {
		return nil, err
	}
	setupLogging(dataDir, !c.settings.DebugOff)
	watchThrottles()
	if c.settings.DeviceID == "" {
		c.settings.DeviceID = index.NewID()
		if err := c.saveSettings(); err != nil {
			return nil, err
		}
	}
	srv, err := startStreamServer(c)
	if err != nil {
		return nil, err
	}
	c.stream = srv
	removeStaleDBs(dataDir, filepath.Base(c.dbPath()))
	if c.settings.Folder != "" {
		if err := c.openDB(); err != nil {
			return nil, err
		}
	}
	return c, nil
}

func (c *Core) dbPath() string {
	if c.settings.DBFile == "" {
		return filepath.Join(c.dataDir, "library.sqlite")
	}
	return filepath.Join(c.dataDir, c.settings.DBFile)
}
func (c *Core) artDir() string       { return filepath.Join(c.dataDir, "art") }
func (c *Core) settingsPath() string { return filepath.Join(c.dataDir, "settings.json") }

func (c *Core) loadSettings() error {
	b, err := os.ReadFile(c.settingsPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, &c.settings)
}

func (c *Core) saveSettings() error {
	b, err := json.MarshalIndent(c.settings, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(c.settingsPath(), b)
}

// openDB opens the local index. The caller holds c.mu or is New.
func (c *Core) openDB() error {
	db, err := index.Open(c.dbPath())
	if err != nil {
		return err
	}
	c.db = db
	c.dbEpoch++
	c.countTracks(db)
	return nil
}

// countTracks refreshes the cached track count from db. It does not take
// c.mu.
func (c *Core) countTracks(db *index.DB) {
	if n, err := db.TrackCount(); err == nil {
		c.tracks.Store(n)
	}
}

// Connected reports whether the primary's cloud is connected.
func (c *Core) Connected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connectedLocked()
}

// connectedLocked is Connected for a caller holding c.mu.
func (c *Core) connectedLocked() bool {
	r := c.primaryRemote()
	if r == localPrimary {
		return true
	}
	return remoteConnected(r)
}

// libraryFs returns the rclone Fs for the library folder, opening it on
// first use.
// The open runs without c.mu, as it can take a while (signing in to
// Proton, say) and Status and streaming take c.mu.
func (c *Core) libraryFs(ctx context.Context) (fs.Fs, error) {
	c.mu.Lock()
	if c.fsys != nil {
		f := c.fsys
		c.mu.Unlock()
		return f, nil
	}
	s := source{Remote: c.primaryRemote(), Folder: c.settings.Folder}
	c.mu.Unlock()
	if s.Folder == "" {
		return nil, errors.New("no library folder picked")
	}
	f, err := openSource(ctx, s)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", s.Folder, err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.primaryRemote() != s.Remote || c.settings.Folder != s.Folder:
		return nil, errors.New("the library folder changed while it was opening")
	case c.fsys != nil:
		return c.fsys, nil // opened meanwhile by another caller
	}
	c.fsys = f
	return f, nil
}

// Prepare gets ready to stream a file, such as the selected track or the
// next one in the queue, by looking up its cloud object in the
// background.
func (c *Core) Prepare(fileID string) { c.stream.prepare(fileID) }

// removeStaleDBs deletes index files left by earlier restores. It runs at
// start-up, when no app connection can still have them open.
func removeStaleDBs(dataDir, current string) {
	matches, _ := filepath.Glob(filepath.Join(dataDir, "library*.sqlite*"))
	for _, m := range matches {
		base := filepath.Base(m)
		if base == current || base == current+"-wal" || base == current+"-shm" {
			continue
		}
		_ = os.Remove(m)
	}
}

// Close stops a running job, waits for it, stops the streaming endpoint
// and closes the index.
func (c *Core) Close() error {
	c.mu.Lock()
	c.closed = true
	if c.retry != nil {
		c.retry.Stop()
	}
	done := c.jobDone
	if c.cancel != nil {
		c.cancel()
	}
	c.stopHashPassLocked()
	c.mu.Unlock()
	if done != nil {
		<-done
	}
	c.waitHashPass()
	c.stream.close()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.db != nil {
		return c.db.Close()
	}
	return nil
}

func writeFileAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
