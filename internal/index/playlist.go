package index

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Playlists live in the index (tables playlist and playlist_item), so
// they travel with the library: they are in every snapshot that goes to
// the primary and the secondaries. An item names a file by id, and a file
// row is never deleted, only marked missing, so an item whose file
// disappears stays in its place and comes back with the file. Readers
// skip items whose file is missing.
//
// Item positions run 0, 1, 2, ... in each playlist. Every write here
// keeps them dense and, in the same transaction, sets meta "dirty" (so
// that the next sync publishes a new index version) and
// "playlists_dirty" (so that the core knows playlist changes are not
// saved yet). Making a version clears both.

// ErrNoPlaylist means no playlist has the given id.
var ErrNoPlaylist = errors.New("no such playlist")

// Playlist is one playlist.
type Playlist struct {
	ID        string
	Name      string
	UpdatedAt int64 // Unix ms
}

// Playlists returns every playlist, by name.
func (d *DB) Playlists() ([]Playlist, error) {
	rows, err := d.db.Query(`SELECT id, name, updated_at FROM playlist ORDER BY name, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Playlist
	for rows.Next() {
		var p Playlist
		if err := rows.Scan(&p.ID, &p.Name, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PlaylistItems returns a playlist's file ids in order, including files
// that are missing now.
func (d *DB) PlaylistItems(id string) ([]string, error) {
	tx, err := d.db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := playlistExists(tx, id); err != nil {
		return nil, err
	}
	return playlistItems(tx, id)
}

func cleanName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("a playlist needs a name")
	}
	return name, nil
}

func markDirty(tx *sql.Tx) error {
	_, err := tx.Exec(`INSERT INTO meta (key, value) VALUES ('dirty', '1'), ('playlists_dirty', '1')
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`)
	return err
}

func playlistExists(tx *sql.Tx, id string) error {
	var n int
	if err := tx.QueryRow(`SELECT count(*) FROM playlist WHERE id = ?`, id).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return ErrNoPlaylist
	}
	return nil
}

func playlistItems(tx *sql.Tx, id string) ([]string, error) {
	rows, err := tx.Query(`SELECT file_id FROM playlist_item WHERE playlist_id = ? ORDER BY pos`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var f string
		if err := rows.Scan(&f); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// CreatePlaylist makes an empty playlist and returns its id.
func (d *DB) CreatePlaylist(name string) (string, error) {
	name, err := cleanName(name)
	if err != nil {
		return "", err
	}
	tx, err := d.db.Begin()
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	id := NewID()
	if _, err := tx.Exec(`INSERT INTO playlist (id, name, updated_at) VALUES (?, ?, ?)`,
		id, name, time.Now().UnixMilli()); err != nil {
		return "", err
	}
	if err := markDirty(tx); err != nil {
		return "", err
	}
	return id, tx.Commit()
}

// RenamePlaylist renames a playlist.
func (d *DB) RenamePlaylist(id, name string) error {
	name, err := cleanName(name)
	if err != nil {
		return err
	}
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var old string
	err = tx.QueryRow(`SELECT name FROM playlist WHERE id = ?`, id).Scan(&old)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNoPlaylist
	}
	if err != nil || old == name {
		return err
	}
	if _, err := tx.Exec(`UPDATE playlist SET name = ?, updated_at = ? WHERE id = ?`,
		name, time.Now().UnixMilli(), id); err != nil {
		return err
	}
	if err := markDirty(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// DeletePlaylist deletes a playlist and its items. Its files stay.
func (d *DB) DeletePlaylist(id string) error {
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := playlistExists(tx, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM playlist_item WHERE playlist_id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM playlist WHERE id = ?`, id); err != nil {
		return err
	}
	if err := markDirty(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// editItems rewrites a playlist's items as edit returns them, in one
// transaction. Only the part that changed is written: the items before
// it and after it keep their rows, the ones after it shifted when the
// length changed, so adding to or removing from a long playlist stays
// quick. Nothing is written when the items don't change.
func (d *DB) editItems(id string, edit func(tx *sql.Tx, items []string) ([]string, error)) error {
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := playlistExists(tx, id); err != nil {
		return err
	}
	items, err := playlistItems(tx, id)
	if err != nil {
		return err
	}
	next, err := edit(tx, slices.Clone(items))
	if err != nil {
		return err
	}
	if slices.Equal(items, next) {
		return nil
	}
	// items[p:len(items)-s] becomes next[p:len(next)-s].
	p := 0
	for p < len(items) && p < len(next) && items[p] == next[p] {
		p++
	}
	s := 0
	for s < len(items)-p && s < len(next)-p && items[len(items)-1-s] == next[len(next)-1-s] {
		s++
	}
	oldEnd, shift := len(items)-s, len(next)-len(items)
	if _, err := tx.Exec(`DELETE FROM playlist_item WHERE playlist_id = ? AND pos >= ? AND pos < ?`,
		id, p, oldEnd); err != nil {
		return err
	}
	if shift != 0 && s > 0 {
		// Through negative positions, so no two rows share one on the way.
		if _, err := tx.Exec(`UPDATE playlist_item SET pos = -(pos + ?) - 1 WHERE playlist_id = ? AND pos >= ?`,
			shift, id, oldEnd); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE playlist_item SET pos = -pos - 1 WHERE playlist_id = ? AND pos < 0`, id); err != nil {
			return err
		}
	}
	ins, err := tx.Prepare(`INSERT INTO playlist_item (playlist_id, pos, file_id) VALUES (?, ?, ?)`)
	if err != nil {
		return err
	}
	defer func() { _ = ins.Close() }()
	for i, f := range next[p : len(next)-s] {
		if _, err := ins.Exec(id, p+i, f); err != nil {
			return fmt.Errorf("write playlist item: %w", err)
		}
	}
	if _, err := tx.Exec(`UPDATE playlist SET updated_at = ? WHERE id = ?`, time.Now().UnixMilli(), id); err != nil {
		return err
	}
	if err := markDirty(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// AddToPlaylist inserts files before the item at position before, or at
// the end when before is negative or past the end. Ids the index doesn't
// know are an error, and nothing is added.
func (d *DB) AddToPlaylist(id string, fileIDs []string, before int) error {
	return d.editItems(id, func(tx *sql.Tx, items []string) ([]string, error) {
		for _, f := range fileIDs {
			var n int
			if err := tx.QueryRow(`SELECT count(*) FROM file WHERE id = ?`, f).Scan(&n); err != nil {
				return nil, err
			}
			if n == 0 {
				return nil, fmt.Errorf("no file with id %q in the library", f)
			}
		}
		if before < 0 || before > len(items) {
			before = len(items)
		}
		return slices.Insert(items, before, fileIDs...), nil
	})
}

// checkPositions returns the given positions as a set, or an error if one
// is out of range.
func checkPositions(positions []int, n int) (map[int]bool, error) {
	set := map[int]bool{}
	for _, p := range positions {
		if p < 0 || p >= n {
			return nil, fmt.Errorf("the playlist has no item at position %d", p)
		}
		set[p] = true
	}
	return set, nil
}

// RemoveFromPlaylist removes the items at the given positions.
func (d *DB) RemoveFromPlaylist(id string, positions []int) error {
	return d.editItems(id, func(_ *sql.Tx, items []string) ([]string, error) {
		drop, err := checkPositions(positions, len(items))
		if err != nil {
			return nil, err
		}
		var out []string
		for i, f := range items {
			if !drop[i] {
				out = append(out, f)
			}
		}
		return out, nil
	})
}

// MovePlaylistItems moves the items at the given positions, keeping their
// order, to just before the item that is at position before now, or to
// the end when before is negative or past the end. This is how a list
// view reports a drag: as Swift's move(fromOffsets:toOffset:).
func (d *DB) MovePlaylistItems(id string, positions []int, before int) error {
	return d.editItems(id, func(_ *sql.Tx, items []string) ([]string, error) {
		move, err := checkPositions(positions, len(items))
		if err != nil {
			return nil, err
		}
		if before < 0 || before > len(items) {
			before = len(items)
		}
		var moved, rest []string
		at := 0 // where the moved items go in rest
		for i, f := range items {
			if move[i] {
				moved = append(moved, f)
				continue
			}
			if i < before {
				at++
			}
			rest = append(rest, f)
		}
		return slices.Insert(rest, at, moved...), nil
	})
}

// RemoveMissingFromPlaylist removes the items whose file is missing from
// the library now, which the app hides.
func (d *DB) RemoveMissingFromPlaylist(id string) error {
	return d.editItems(id, func(tx *sql.Tx, items []string) ([]string, error) {
		rows, err := tx.Query(`SELECT i.file_id FROM playlist_item i JOIN file f ON f.id = i.file_id
			WHERE i.playlist_id = ? AND f.missing = 1`, id)
		if err != nil {
			return nil, err
		}
		gone := map[string]bool{}
		for rows.Next() {
			var f string
			if err := rows.Scan(&f); err != nil {
				rows.Close()
				return nil, err
			}
			gone[f] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return slices.DeleteFunc(items, func(f string) bool { return gone[f] }), nil
	})
}

// PathPlaylist is a playlist with its items named by path, to carry it to
// another index of the same library, whose file ids differ.
type PathPlaylist struct {
	Playlist
	Paths []string
}

// PlaylistsByPath returns every playlist with its items' paths in order.
func (d *DB) PlaylistsByPath() ([]PathPlaylist, error) {
	lists, err := d.Playlists()
	if err != nil || len(lists) == 0 {
		return nil, err
	}
	rows, err := d.db.Query(`SELECT i.playlist_id, f.path FROM playlist_item i
		JOIN file f ON f.id = i.file_id ORDER BY i.playlist_id, i.pos`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	paths := map[string][]string{}
	for rows.Next() {
		var pl, p string
		if err := rows.Scan(&pl, &p); err != nil {
			return nil, err
		}
		paths[pl] = append(paths[pl], p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]PathPlaylist, len(lists))
	for i, p := range lists {
		out[i] = PathPlaylist{Playlist: p, Paths: paths[p.ID]}
	}
	return out, nil
}

// AddPlaylistsByPath adds playlists carried from another index: each item
// finds its file here by path, and items whose path this index doesn't
// know are left out. A playlist this index already has (by id) is left as
// it is. It returns how many playlists it added, and marks the index
// dirty if any.
func (d *DB) AddPlaylistsByPath(lists []PathPlaylist) (int, error) {
	if len(lists) == 0 {
		return 0, nil
	}
	tx, err := d.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	added := 0
	for _, p := range lists {
		res, err := tx.Exec(`INSERT INTO playlist (id, name, updated_at) VALUES (?, ?, ?)
			ON CONFLICT (id) DO NOTHING`, p.ID, p.Name, p.UpdatedAt)
		if err != nil {
			return 0, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue
		}
		added++
		ids, err := filesByPath(tx, p.Paths)
		if err != nil {
			return 0, err
		}
		for pos, fileID := range ids {
			if _, err := tx.Exec(`INSERT INTO playlist_item (playlist_id, pos, file_id) VALUES (?, ?, ?)`,
				p.ID, pos, fileID); err != nil {
				return 0, err
			}
		}
	}
	if added == 0 {
		return 0, nil
	}
	if err := markDirty(tx); err != nil {
		return 0, err
	}
	return added, tx.Commit()
}

// filesByPath returns the ids of the files at the given paths, in order,
// leaving out paths the index doesn't know.
func filesByPath(tx *sql.Tx, paths []string) ([]string, error) {
	var ids []string
	for _, path := range paths {
		var fileID string
		err := tx.QueryRow(`SELECT id FROM file WHERE path = ?`, path).Scan(&fileID)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		ids = append(ids, fileID)
	}
	return ids, nil
}

// RefillPlaylistByPath sets the items of a playlist added by
// AddPlaylistsByPath from its full list of paths again, once more of them
// may resolve (after indexing). A playlist edited since it was added (its
// updated_at differs) or deleted is left alone. It reports whether the
// playlist was still as added.
func (d *DB) RefillPlaylistByPath(p PathPlaylist) (bool, error) {
	unedited := false
	err := d.editItems(p.ID, func(tx *sql.Tx, items []string) ([]string, error) {
		var updated int64
		if err := tx.QueryRow(`SELECT updated_at FROM playlist WHERE id = ?`, p.ID).Scan(&updated); err != nil {
			return nil, err
		}
		if updated != p.UpdatedAt {
			return items, nil // edited since: the user's version stands
		}
		unedited = true
		return filesByPath(tx, p.Paths)
	})
	if errors.Is(err, ErrNoPlaylist) {
		return false, nil
	}
	return unedited, err
}
