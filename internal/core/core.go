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

	_ "github.com/rclone/rclone/backend/drive" // Google Drive
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/config/configfile"

	"github.com/stamler/medialayer/internal/index"
)

// remoteName is the rclone remote the core creates for Google Drive, in
// its own rclone config file (not the user's).
const remoteName = "gdrive"

// Core is one library on one device.
type Core struct {
	dataDir string

	mu       sync.Mutex
	settings settings
	fsys     fs.Fs     // the library folder, nil until one is picked
	db       *index.DB // nil until a folder is picked
	dbEpoch  int64     // raised each time the database file is replaced
	job      jobStatus
	jobDone  chan struct{}      // closed when the running job ends; nil if none
	cancel   context.CancelFunc // stops the running job
	// tracks is the index's track count, kept here so Status never waits
	// on the database while the indexer writes.
	tracks atomic.Int64
	stream   *streamServer
}

// settings are kept in settings.json in the data directory.
type settings struct {
	Folder   string `json:"folder"`    // library folder on the remote, e.g. "Music"
	DeviceID string `json:"device_id"` // random, made once per install
	// DBFile is the local index's file name. A restore writes a new file
	// rather than replacing this one under the app's open connection.
	DBFile string `json:"db_file,omitempty"`
}

// New opens the core with its files in dataDir (on macOS,
// ~/Library/Application Support/MediaLayer). It starts the streaming
// endpoint and, if a folder was picked before, opens its index.
func New(dataDir string) (*Core, error) {
	if err := os.MkdirAll(filepath.Join(dataDir, "art"), 0o700); err != nil {
		return nil, err
	}
	if err := config.SetConfigPath(filepath.Join(dataDir, "rclone.conf")); err != nil {
		return nil, err
	}
	configfile.Install()

	c := &Core{dataDir: dataDir}
	if err := c.loadSettings(); err != nil {
		return nil, err
	}
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

// Connected reports whether a Google Drive remote is configured.
func (c *Core) Connected() bool {
	return config.GetValue(remoteName, "type") != "" && config.GetValue(remoteName, "token") != ""
}

// libraryFs returns the rclone Fs for the library folder, opening it on
// first use.
func (c *Core) libraryFs(ctx context.Context) (fs.Fs, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fsys != nil {
		return c.fsys, nil
	}
	if c.settings.Folder == "" {
		return nil, errors.New("no library folder picked")
	}
	f, err := fs.NewFs(ctx, remoteName+":"+c.settings.Folder)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", c.settings.Folder, err)
	}
	c.fsys = f
	return f, nil
}

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
	done := c.jobDone
	if c.cancel != nil {
		c.cancel()
	}
	c.mu.Unlock()
	if done != nil {
		<-done
	}
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
