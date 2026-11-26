package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/stamler/medialayer/internal/index"
)

// Playlists are kept in the index (see internal/index/playlist.go), so the
// core makes every change to them: it owns the database and its
// versioning. Each change marks the index dirty, and the next sync
// publishes it as a new version to the primary and every secondary. The
// app reads playlists from the database itself, as it reads tracks.
//
// Changes are serialised by c.playlistMu, which restore also holds while
// it reads the playlists of the index it replaces and swaps in the
// restored one, so no change is made to an index that is being replaced.
// The playlists it read join the restored index at once, with the items
// its snapshot knows; their full item lists wait in
// carried-playlists.json until the sync's index pass ends, and then
// refill each playlist not edited since (applyCarried).
//
// A file that is moved or renamed keeps its id (see matchMoves), so it
// stays in its playlists. SetFolder, which deletes the local index, holds
// c.playlistMu and refuses while playlist changes are unsaved, saying
// how to save or discard them (folderChangeBlocked).

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

// RemoveMissingFromPlaylist removes the items whose file is missing from
// the library now (the app hides them).
func (c *Core) RemoveMissingFromPlaylist(id string) error {
	db, err := c.playlistDB()
	if err != nil {
		return err
	}
	defer c.playlistMu.Unlock()
	return db.RemoveMissingFromPlaylist(id)
}

// Errors that stop a folder change, which deletes the local index, from
// discarding playlist changes the library's folders don't hold yet. Each
// names a way out.
var (
	errUnsavedPlaylists = errors.New("playlist changes haven't been saved to the library yet; " +
		"update the library first (Library > Update Library), or delete the playlists to discard them")
	errPlaylistsPrimaryFull = errors.New("playlist changes can't be saved to the library: the primary's cloud is full; " +
		"free up space there and update the library, or delete the playlists to discard the changes")
	errCarriedWaiting = errors.New("playlists from this library are waiting to be restored; " +
		"update the library first (Library > Update Library)")
)

// folderChangeBlocked returns the reason a folder change would lose
// playlist changes, or nil. db is the open index, or nil; primaryFull
// says the last sync found the primary's cloud full. The caller holds
// c.playlistMu.
func (c *Core) folderChangeBlocked(db *index.DB, primaryFull bool) error {
	if db == nil {
		// Carried playlists are added to the index at restore, so they
		// wait outside it only if no index could be opened since.
		carried, err := c.readCarried()
		if err != nil {
			return fmt.Errorf("check the playlists: %w", err)
		}
		if len(carried) > 0 {
			return errCarriedWaiting
		}
		return nil
	}
	unsaved, err := unsavedPlaylists(db)
	if err != nil {
		return fmt.Errorf("check the playlists: %w", err)
	}
	switch {
	case !unsaved:
		return nil
	case primaryFull:
		return errPlaylistsPrimaryFull
	default:
		return errUnsavedPlaylists
	}
}

// unsavedPlaylists reports whether db has playlists, and changes to them
// that the primary doesn't hold yet.
func unsavedPlaylists(db *index.DB) (bool, error) {
	lists, err := db.Playlists()
	if err != nil || len(lists) == 0 {
		return false, err
	}
	changed, err := db.Meta("playlists_dirty")
	if err != nil {
		return false, err
	}
	version, err := metaInt(db, "version")
	if err != nil {
		return false, err
	}
	onPrimary, err := metaInt(db, "primary_version")
	if err != nil {
		return false, err
	}
	return changed == "1" || onPrimary < version, nil
}

// carriedName is the file in the data directory that holds playlists
// carried over from a replaced index, by path, until the next index pass
// has listed every file they name.
const carriedName = "carried-playlists.json"

func (c *Core) carriedPath() string { return filepath.Join(c.dataDir, carriedName) }

// carryPlaylists keeps playlists, by path, to refill after the next index
// pass, along with any already waiting. A playlist read again replaces
// the copy waiting. The caller holds c.playlistMu.
func (c *Core) carryPlaylists(lists []index.PathPlaylist) error {
	if len(lists) == 0 {
		return nil
	}
	waiting, err := c.readCarried()
	if err != nil {
		return err
	}
	fresh := map[string]bool{}
	for _, p := range lists {
		fresh[p.ID] = true
	}
	kept := lists
	for _, p := range waiting {
		if !fresh[p.ID] {
			kept = append(kept, p)
		}
	}
	b, err := json.Marshal(kept)
	if err != nil {
		return err
	}
	return writeFileAtomic(c.carriedPath(), b)
}

func (c *Core) readCarried() ([]index.PathPlaylist, error) {
	b, err := os.ReadFile(c.carriedPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var lists []index.PathPlaylist
	if err := json.Unmarshal(b, &lists); err != nil {
		return nil, fmt.Errorf("%s: %w", carriedName, err)
	}
	return lists, nil
}

// applyCarried refills the carried playlists from their full item lists,
// now that the index pass has listed every file. Restore added them with
// the items the snapshot could resolve; a playlist edited or deleted
// since is left as it is. Items whose path is still unknown are left
// out. A refill makes a new version, which the publish that follows
// saves.
func (c *Core) applyCarried() error {
	c.playlistMu.Lock()
	defer c.playlistMu.Unlock()
	lists, err := c.readCarried()
	if err != nil || len(lists) == 0 {
		return err
	}
	db := c.currentDB()
	if db == nil {
		return errNoLibrary
	}
	for _, p := range lists {
		if _, err := db.RefillPlaylistByPath(p); err != nil {
			return err
		}
	}
	return os.Remove(c.carriedPath())
}
