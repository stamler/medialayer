// Package index is the library's SQLite index: the schema from the
// architecture proposal (meta, file, tag, art, file_art, location and the
// playlist tables) and the writes the indexer makes to it.
//
// The local database is a cache. The cloud holds snapshots of it in
// .medialayer/snapshots, made with Snapshot. The Mac app reads the same
// database file directly, so it is kept in WAL mode: the app can read
// while the indexer writes.
package index

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"time"

	"github.com/oklog/ulid/v2"
	_ "modernc.org/sqlite" // the database/sql driver "sqlite"

	"github.com/stamler/medialayer/internal/media"
)

// SchemaVersion is stored in meta as "schema_version".
const SchemaVersion = "1"

const schema = `
CREATE TABLE IF NOT EXISTS meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS file (
  id            TEXT PRIMARY KEY,
  path          TEXT NOT NULL UNIQUE,
  size          INTEGER NOT NULL,
  mtime         INTEGER NOT NULL,
  sha256        BLOB,
  audio_sha256  BLOB,
  format        TEXT NOT NULL,
  duration_ms   INTEGER,
  sample_rate   INTEGER,
  bit_depth     INTEGER,
  channels      INTEGER,
  header_pack   TEXT,
  header_offset INTEGER,
  header_length INTEGER,
  missing       INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS tag (
  file_id TEXT NOT NULL REFERENCES file(id),
  key     TEXT NOT NULL,
  value   TEXT NOT NULL,
  seq     INTEGER NOT NULL,
  PRIMARY KEY (file_id, key, seq)
);

CREATE TABLE IF NOT EXISTS art (
  sha256      BLOB PRIMARY KEY,
  mime        TEXT NOT NULL,
  width       INTEGER,
  height      INTEGER,
  pack        TEXT NOT NULL,
  pack_offset INTEGER NOT NULL,
  pack_length INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS file_art (
  file_id    TEXT NOT NULL REFERENCES file(id),
  art_sha256 BLOB NOT NULL REFERENCES art(sha256),
  role       INTEGER NOT NULL,
  PRIMARY KEY (file_id, role, art_sha256)
);

CREATE TABLE IF NOT EXISTS location (
  file_id     TEXT NOT NULL REFERENCES file(id),
  replica     TEXT NOT NULL,
  cloud_id    TEXT NOT NULL,
  verified_at INTEGER,
  PRIMARY KEY (file_id, replica)
);

CREATE INDEX IF NOT EXISTS tag_lookup ON tag (key, value);
CREATE INDEX IF NOT EXISTS file_sha ON file (sha256);
CREATE INDEX IF NOT EXISTS file_audio ON file (audio_sha256);

CREATE TABLE IF NOT EXISTS playlist (
  id         TEXT PRIMARY KEY,
  name       TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS playlist_item (
  playlist_id TEXT NOT NULL REFERENCES playlist(id),
  pos         INTEGER NOT NULL,
  file_id     TEXT NOT NULL REFERENCES file(id),
  PRIMARY KEY (playlist_id, pos)
);
`

// DB is an open index.
type DB struct {
	db   *sql.DB
	path string
}

// Open opens or creates the index at path.
func Open(path string) (*DB, error) {
	// One connection: writes are serialised anyway, and it keeps
	// transactions and pragmas on the same connection.
	uri := url.URL{Scheme: "file", Path: path,
		RawQuery: "_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"}
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}
	d := &DB{db: db, path: path}
	if v, err := d.Meta("schema_version"); err != nil {
		_ = db.Close()
		return nil, err
	} else if v == "" {
		if err := d.SetMeta("schema_version", SchemaVersion); err != nil {
			_ = db.Close()
			return nil, err
		}
	} else if v != SchemaVersion {
		_ = db.Close()
		return nil, fmt.Errorf("index schema version %s, want %s", v, SchemaVersion)
	}
	return d, nil
}

// Close closes the database.
func (d *DB) Close() error { return d.db.Close() }

// SQL is the underlying database, for queries this package has no
// method for.
func (d *DB) SQL() *sql.DB { return d.db }

// Path is the database file.
func (d *DB) Path() string { return d.path }

// Meta returns a meta value, or "" if it is not set.
func (d *DB) Meta(key string) (string, error) {
	var v string
	err := d.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// SetMeta sets a meta value.
func (d *DB) SetMeta(key, value string) error {
	_, err := d.db.Exec(`INSERT INTO meta (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// Known is what the index holds about a file, for deciding whether it
// needs indexing again.
type Known struct {
	ID      string
	Size    int64
	MTime   int64 // Unix ms
	SHA256  []byte
	Missing bool
}

// KnownFiles returns every indexed file by path.
func (d *DB) KnownFiles() (map[string]Known, error) {
	rows, err := d.db.Query(`SELECT id, path, size, mtime, sha256, missing FROM file`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Known{}
	for rows.Next() {
		var k Known
		var path string
		if err := rows.Scan(&k.ID, &path, &k.Size, &k.MTime, &k.SHA256, &k.Missing); err != nil {
			return nil, err
		}
		out[path] = k
	}
	return out, rows.Err()
}

// HasArt reports whether an image with this hash is already stored.
func (d *DB) HasArt(sha []byte) (bool, error) {
	var n int
	err := d.db.QueryRow(`SELECT count(*) FROM art WHERE sha256 = ?`, sha).Scan(&n)
	return n > 0, err
}

// File is one file to write to the index.
type File struct {
	Path    string
	Size    int64
	MTime   int64  // Unix ms
	SHA256  []byte // nil if the cloud gave none
	Replica string // e.g. "gdrive"
	CloudID string // the provider's file id
	Info    *media.Info
	Art     []FileArt
}

// FileArt links a file to a stored image.
type FileArt struct {
	SHA256 []byte
	Role   int // picture type
}

// Art is an image stored in an art pack.
type Art struct {
	SHA256        []byte
	MIME          string
	Width, Height int
	Pack          string
	Offset        int64
	Length        int64
}

// PutArt records an image stored in a pack. An image already recorded is
// left as it is.
func (d *DB) PutArt(a Art) error {
	_, err := d.db.Exec(`INSERT INTO art (sha256, mime, width, height, pack, pack_offset, pack_length)
		VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT (sha256) DO NOTHING`,
		a.SHA256, a.MIME, nullInt(a.Width), nullInt(a.Height), a.Pack, a.Offset, a.Length)
	return err
}

// PutFile writes a file, its tags, art links and location, replacing what
// the index held for that path. The file keeps its id if the path was
// already indexed. It returns the file's id.
func (d *DB) PutFile(f File) (string, error) {
	tx, err := d.db.Begin()
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()

	var id string
	err = tx.QueryRow(`SELECT id FROM file WHERE path = ?`, f.Path).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		id = NewID()
	} else if err != nil {
		return "", err
	}
	info := f.Info
	_, err = tx.Exec(`INSERT INTO file (id, path, size, mtime, sha256, format, duration_ms, sample_rate, bit_depth, channels, missing)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0)
		ON CONFLICT (id) DO UPDATE SET size = excluded.size, mtime = excluded.mtime, sha256 = excluded.sha256,
		  audio_sha256 = NULL, format = excluded.format, duration_ms = excluded.duration_ms,
		  sample_rate = excluded.sample_rate, bit_depth = excluded.bit_depth, channels = excluded.channels,
		  header_pack = NULL, header_offset = NULL, header_length = NULL, missing = 0`,
		id, f.Path, f.Size, f.MTime, f.SHA256, info.Format, nullInt64(info.DurationMS),
		nullInt(info.SampleRate), nullInt(info.BitDepth), nullInt(info.Channels))
	if err != nil {
		return "", fmt.Errorf("write file row: %w", err)
	}
	for _, q := range []string{`DELETE FROM tag WHERE file_id = ?`, `DELETE FROM file_art WHERE file_id = ?`} {
		if _, err := tx.Exec(q, id); err != nil {
			return "", err
		}
	}
	seqs := map[string]int{}
	for _, t := range info.Tags {
		seq := seqs[t.Key]
		seqs[t.Key]++
		if _, err := tx.Exec(`INSERT INTO tag (file_id, key, value, seq) VALUES (?, ?, ?, ?)`, id, t.Key, t.Value, seq); err != nil {
			return "", fmt.Errorf("write tag: %w", err)
		}
	}
	for _, a := range f.Art {
		if _, err := tx.Exec(`INSERT INTO file_art (file_id, art_sha256, role) VALUES (?, ?, ?)
			ON CONFLICT DO NOTHING`, id, a.SHA256, a.Role); err != nil {
			return "", fmt.Errorf("write file_art: %w", err)
		}
	}
	if f.Replica != "" {
		if _, err := tx.Exec(`INSERT INTO location (file_id, replica, cloud_id, verified_at) VALUES (?, ?, ?, ?)
			ON CONFLICT (file_id, replica) DO UPDATE SET cloud_id = excluded.cloud_id, verified_at = excluded.verified_at`,
			id, f.Replica, f.CloudID, time.Now().UnixMilli()); err != nil {
			return "", fmt.Errorf("write location: %w", err)
		}
	}
	return id, tx.Commit()
}

// MarkFilesChanged records that file rows changed: meta "dirty" (a new
// version is due) and "files_dirty" (the files differ from the last
// version, as a playlist-only change does not).
func (d *DB) MarkFilesChanged() error {
	_, err := d.db.Exec(`INSERT INTO meta (key, value) VALUES ('dirty', '1'), ('files_dirty', '1')
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`)
	return err
}

// MoveFile gives a file row a new path, for a file that was moved or
// renamed: it keeps its id, so playlists keep it. The path must not be
// indexed already.
func (d *DB) MoveFile(id, path string) error {
	_, err := d.db.Exec(`UPDATE file SET path = ?, missing = 0 WHERE id = ?`, path, id)
	return err
}

// SetMissing marks files as missing (true) or present (false) by id.
func (d *DB) SetMissing(ids []string, missing bool) error {
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, id := range ids {
		if _, err := tx.Exec(`UPDATE file SET missing = ? WHERE id = ?`, missing, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// FilesWithoutArt returns the ids of present files with no image linked,
// by path.
func (d *DB) FilesWithoutArt() (map[string]string, error) {
	rows, err := d.db.Query(`SELECT path, id FROM file f WHERE missing = 0
		AND NOT EXISTS (SELECT 1 FROM file_art a WHERE a.file_id = f.id)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var p, id string
		if err := rows.Scan(&p, &id); err != nil {
			return nil, err
		}
		out[p] = id
	}
	return out, rows.Err()
}

// AddFileArt links an image to files.
func (d *DB) AddFileArt(fileIDs []string, sha []byte, role int) error {
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, id := range fileIDs {
		if _, err := tx.Exec(`INSERT INTO file_art (file_id, art_sha256, role) VALUES (?, ?, ?)
			ON CONFLICT DO NOTHING`, id, sha, role); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ArtPacks lists the art packs the index refers to.
func (d *DB) ArtPacks() ([]string, error) {
	rows, err := d.db.Query(`SELECT DISTINCT pack FROM art ORDER BY pack`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// FileLocation is what streaming needs to find a file in the cloud.
type FileLocation struct {
	Path    string
	Size    int64
	MTime   int64
	Format  string
	CloudID string // "" if no location row for the replica
}

// Locate looks up a file by id.
func (d *DB) Locate(id, replica string) (FileLocation, error) {
	var l FileLocation
	var cloudID sql.NullString
	err := d.db.QueryRow(`SELECT f.path, f.size, f.mtime, f.format, l.cloud_id FROM file f
		LEFT JOIN location l ON l.file_id = f.id AND l.replica = ?
		WHERE f.id = ?`, replica, id).Scan(&l.Path, &l.Size, &l.MTime, &l.Format, &cloudID)
	l.CloudID = cloudID.String
	return l, err
}

// TrackCount is the number of files that are not missing.
func (d *DB) TrackCount() (int64, error) {
	var n int64
	err := d.db.QueryRow(`SELECT count(*) FROM file WHERE missing = 0`).Scan(&n)
	return n, err
}

// Snapshot writes a compacted copy of the index to dst, which must not
// exist.
func (d *DB) Snapshot(dst string) error {
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("snapshot %s already exists", dst)
	}
	_, err := d.db.Exec(`VACUUM INTO ?`, dst)
	return err
}

// NewID makes a file or playlist id: a ULID, which any device can create
// without asking another.
func NewID() string {
	return ulid.MustNew(ulid.Now(), rand.Reader).String()
}

// HexSHA decodes a hex SHA-256 as the cloud reports it, or returns nil.
func HexSHA(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		return nil
	}
	return b
}

func nullInt(v int) any {
	if v == 0 {
		return nil
	}
	return v
}

func nullInt64(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}
