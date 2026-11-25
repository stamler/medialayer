package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/rc"
)

// googleClient is MediaLayer's own Google OAuth client. rclone's shared
// client is heavily throttled. For the proof of concept it is read from
// google-oauth.json in the data directory, so it is never committed.
type googleClient struct {
	ID     string `json:"client_id"`
	Secret string `json:"client_secret"`
}

func (c *Core) googleClient() (googleClient, error) {
	var g googleClient
	p := filepath.Join(c.dataDir, "google-oauth.json")
	b, err := os.ReadFile(p)
	if err != nil {
		return g, fmt.Errorf("read MediaLayer's Google OAuth client from %s: %w", p, err)
	}
	if err := json.Unmarshal(b, &g); err != nil {
		return g, fmt.Errorf("%s: %w", p, err)
	}
	if g.ID == "" || g.Secret == "" {
		return g, fmt.Errorf("%s needs client_id and client_secret", p)
	}
	return g, nil
}

// ConnectGoogleDrive signs in to Google Drive. It opens the browser at
// Google's consent page and blocks until the user finishes or ctx ends.
// Full Drive access is needed to write the library's .medialayer folder.
func (c *Core) ConnectGoogleDrive(ctx context.Context) error {
	if err := c.connectDrive(ctx, remoteName); err != nil {
		return err
	}
	c.mu.Lock()
	c.fsys = nil // a new account may see a different folder
	c.mu.Unlock()
	return nil
}

// AddGoogleAccount signs in to another Google account, for a secondary,
// and returns the new remote's name.
func (c *Core) AddGoogleAccount(ctx context.Context) (string, error) {
	name := ""
	for i := 2; name == ""; i++ {
		if n := fmt.Sprintf("%s%d", remoteName, i); config.GetValue(n, "type") == "" {
			name = n
		}
	}
	if err := c.connectDrive(ctx, name); err != nil {
		config.DeleteRemote(name)
		return "", err
	}
	return name, nil
}

// driveAccounts lists the connected Google Drive remotes, the primary's
// first.
func driveAccounts() []string {
	out := []string{}
	for _, name := range config.FileSections() {
		if config.GetValue(name, "type") == "drive" && config.GetValue(name, "token") != "" {
			if name == remoteName {
				out = append([]string{name}, out...)
			} else {
				out = append(out, name)
			}
		}
	}
	return out
}

// connectDrive signs in to Google Drive as the rclone remote name.
func (c *Core) connectDrive(ctx context.Context, name string) error {
	g, err := c.googleClient()
	if err != nil {
		return err
	}
	params := rc.Params{"client_id": g.ID, "client_secret": g.Secret, "scope": "drive"}
	opt := config.UpdateRemoteOpt{NonInteractive: true}
	out, err := config.CreateRemote(ctx, name, "drive", params, opt)
	// The backend's config questions (use a local browser? a shared
	// drive?) are answered with their defaults: a browser on this machine,
	// and My Drive.
	for err == nil && out != nil && out.State != "" {
		if out.Error != "" {
			return errors.New(out.Error)
		}
		answer := out.Result
		if out.Option != nil {
			answer = ""
			if out.Option.Default != nil {
				answer = fmt.Sprint(out.Option.Default)
			}
		}
		out, err = config.UpdateRemote(ctx, name, nil, config.UpdateRemoteOpt{
			NonInteractive: true, Continue: true, State: out.State, Result: answer,
		})
	}
	if err != nil {
		return fmt.Errorf("Google sign-in: %w", err)
	}
	if out != nil && out.Error != "" {
		return errors.New(out.Error)
	}
	if config.GetValue(name, "token") == "" {
		return errors.New("Google sign-in did not finish")
	}
	return nil
}

// ListFolders lists the folders inside dir ("" for the top of My Drive)
// on a connected remote ("" for the primary's), sorted by name, as paths
// relative to the top.
func (c *Core) ListFolders(ctx context.Context, remote, dir string) ([]string, error) {
	if remote == "" {
		remote = remoteName
	}
	if config.GetValue(remote, "token") == "" {
		return nil, errors.New("not connected to Google Drive")
	}
	f, err := fs.NewFs(ctx, remote+":")
	if err != nil {
		return nil, err
	}
	entries, err := f.List(ctx, strings.Trim(dir, "/"))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if d, ok := e.(fs.Directory); ok {
			out = append(out, d.Remote())
		}
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i]) < strings.ToLower(out[j]) })
	return out, nil
}

// SetFolder picks the library folder and starts opening it: the index is
// restored from the folder's .medialayer repository if there is one, then
// brought up to date by indexing, and published back. Picking a different
// folder discards the local index of the old one.
func (c *Core) SetFolder(folder string) error {
	folder = strings.Trim(folder, "/")
	if folder == "" {
		return errors.New("pick a folder, not the top of the drive")
	}
	c.mu.Lock()
	if c.jobDone != nil {
		c.mu.Unlock()
		return errors.New("the library is being indexed; wait for it to finish")
	}
	if folder != c.settings.Folder {
		if c.db != nil {
			_ = c.db.Close()
			c.db = nil
		}
		if err := resetLocal(c.dataDir, c.artDir()); err != nil {
			c.mu.Unlock()
			return err
		}
		c.settings.Folder = folder
		c.settings.DBFile = ""
		c.settings.Secondaries = nil // they hold copies of the old library
		c.mirrors = nil
		c.fsys = nil
		if err := c.saveSettings(); err != nil {
			c.mu.Unlock()
			return err
		}
		if err := c.openDB(); err != nil {
			c.mu.Unlock()
			return err
		}
	}
	c.mu.Unlock()
	return c.StartSync()
}

// folder returns the library folder.
func (c *Core) folder() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.settings.Folder
}

// resetLocal deletes every local index file (including ones left by
// restores) and the art packs.
func resetLocal(dataDir, artDir string) error {
	files, err := filepath.Glob(filepath.Join(dataDir, "library*.sqlite*"))
	if err != nil {
		return err
	}
	for _, p := range files {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.RemoveAll(filepath.Join(dataDir, "snapshots")); err != nil {
		return err
	}
	if err := os.RemoveAll(artDir); err != nil {
		return err
	}
	return os.MkdirAll(artDir, 0o700)
}
