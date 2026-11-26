package core

import (
	"errors"

	"github.com/stamler/medialayer/internal/index"
)

// Playlists are kept in the index (see internal/index/playlist.go), so the
// core makes every change to them: it owns the database and its
// versioning. Each change marks the index dirty, and the next sync
// publishes it as a new version to the primary and every secondary. The
// app reads playlists from the database itself, as it reads tracks.
//
// Changes are serialised by c.playlistMu, which restore also holds while
// it carries playlists over to a restored index and swaps it in, so no
// change is made to an index that is being replaced.

var errNoLibrary = errors.New("no library is open")

// playlistDB returns the open index, with c.playlistMu held; the caller
// unlocks it.
func (c *Core) playlistDB() (*index.DB, error) {
	c.playlistMu.Lock()
	db := c.currentDB()
	if db == nil {
		c.playlistMu.Unlock()
		return nil, errNoLibrary
	}
	return db, nil
}

// CreatePlaylist makes an empty playlist and returns its id.
func (c *Core) CreatePlaylist(name string) (string, error) {
	db, err := c.playlistDB()
	if err != nil {
		return "", err
	}
	defer c.playlistMu.Unlock()
	return db.CreatePlaylist(name)
}

// RenamePlaylist renames a playlist.
func (c *Core) RenamePlaylist(id, name string) error {
	db, err := c.playlistDB()
	if err != nil {
		return err
	}
	defer c.playlistMu.Unlock()
	return db.RenamePlaylist(id, name)
}

// DeletePlaylist deletes a playlist; its files stay in the library.
func (c *Core) DeletePlaylist(id string) error {
	db, err := c.playlistDB()
	if err != nil {
		return err
	}
	defer c.playlistMu.Unlock()
	return db.DeletePlaylist(id)
}

// AddToPlaylist inserts files, by id, before the item at position before
// (negative for the end).
func (c *Core) AddToPlaylist(id string, fileIDs []string, before int) error {
	db, err := c.playlistDB()
	if err != nil {
		return err
	}
	defer c.playlistMu.Unlock()
	return db.AddToPlaylist(id, fileIDs, before)
}

// RemoveFromPlaylist removes the items at the given positions.
func (c *Core) RemoveFromPlaylist(id string, positions []int) error {
	db, err := c.playlistDB()
	if err != nil {
		return err
	}
	defer c.playlistMu.Unlock()
	return db.RemoveFromPlaylist(id, positions)
}

// MovePlaylistItems moves the items at the given positions to just before
// the item now at position before (negative for the end).
func (c *Core) MovePlaylistItems(id string, positions []int, before int) error {
	db, err := c.playlistDB()
	if err != nil {
		return err
	}
	defer c.playlistMu.Unlock()
	return db.MovePlaylistItems(id, positions, before)
}
