package core

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/rclone/rclone/fs"

	"github.com/stamler/medialayer/internal/index"
	"github.com/stamler/medialayer/internal/media"
)

// The host helper, mlhost, runs on the machine that serves an SMB share
// and reads the library from its own disk (see docs/smb.md, "The host
// helper", level 1). It writes only inside the library's
// .medialayer/host: an inventory of the media files, with the tags and
// art its parsers read and each file's audio hash, and the art itself.
// This file is the inventory's format and the device's use of it; the
// helper's side is hostscan.go.
//
// A device uses a row only for the very file it lists: the same path,
// size and modification time to the millisecond, and only when the
// helper's parser (or audio hash) version is its own. Then it takes the
// row's tags and art instead of reading the file's header over the
// network, and the hash pass takes the row's audio hash instead of
// reading the whole file. Nothing else: every sync decision still rests
// on the device's own listing, and a helper's audio hash goes only into
// the index's audio_sha256, never into audio-hashes.json, whose hashes
// decide that a replacement is only a tag edit. A helper that is
// stopped, behind or wrong costs speed, or at worst a wrong tag shown
// until the file changes.

const (
	hostDir           = repoDir + "/host"
	hostInventoryFile = hostDir + "/inventory.json.gz"
	hostStatusFile    = hostDir + "/status.json"
	hostArtDir        = hostDir + "/art"
	// hostFormat is the inventory's format; a device ignores others.
	hostFormat = 1
	// maxHostInventory bounds the inventory, uncompressed.
	maxHostInventory = 1 << 30
)

// hostInventory is inventory.json.gz.
type hostInventory struct {
	Format           int                  `json:"format"`
	Helper           string               `json:"helper"` // this install's id
	Version          string               `json:"version"`
	ParserVersion    string               `json:"parser_version"`     // media.ParserVersion
	AudioHashVersion string               `json:"audio_hash_version"` // index.AudioHashVersion
	Sequence         int64                `json:"sequence"`           // one more each time it is written
	Time             time.Time            `json:"time"`
	Files            []hostFile           `json:"files"`
	Images           map[string]hostImage `json:"images,omitempty"` // by SHA-256, in hex
}

// hostFile is one media file.
type hostFile struct {
	Path  string `json:"path"` // as rclone lists it, from the library folder
	Size  int64  `json:"size"`
	MTime int64  `json:"mtime"` // Unix ms
	// Change is the helper's own: the file's inode and change time, so a
	// file rewritten with its size and time kept is read again.
	Change      string    `json:"change,omitempty"`
	Info        *hostInfo `json:"info,omitempty"`
	Art         []hostArt `json:"art,omitempty"`
	Error       string    `json:"error,omitempty"` // why it couldn't be read
	AudioSHA256 string    `json:"audio_sha256,omitempty"`
	HashError   string    `json:"hash_error,omitempty"`
}

type hostInfo struct {
	Format     string      `json:"format"`
	DurationMS int64       `json:"duration_ms,omitempty"`
	SampleRate int         `json:"sample_rate,omitempty"`
	BitDepth   int         `json:"bit_depth,omitempty"`
	Channels   int         `json:"channels,omitempty"`
	Tags       [][2]string `json:"tags,omitempty"` // key, value, in file order
}

type hostArt struct {
	SHA256 string `json:"sha256"`
	Role   int    `json:"role"` // the picture type
}

// hostImage is an image in art/, at art/<first two hex digits>/<SHA-256>.
type hostImage struct {
	MIME   string `json:"mime"`
	Width  int    `json:"width,omitempty"` // as the file says; 0 if it doesn't
	Height int    `json:"height,omitempty"`
	Size   int64  `json:"size"`
}

func hostArtPath(sha string) string { return hostArtDir + "/" + sha[:2] + "/" + sha }

func toHostInfo(i *media.Info) *hostInfo {
	h := &hostInfo{Format: i.Format, DurationMS: i.DurationMS, SampleRate: i.SampleRate, BitDepth: i.BitDepth,
		Channels: i.Channels}
	for _, t := range i.Tags {
		h.Tags = append(h.Tags, [2]string{t.Key, t.Value})
	}
	return h
}

func (h *hostInfo) mediaInfo() *media.Info {
	i := &media.Info{Format: h.Format, DurationMS: h.DurationMS, SampleRate: h.SampleRate, BitDepth: h.BitDepth,
		Channels: h.Channels}
	for _, t := range h.Tags {
		i.Tags = append(i.Tags, media.Tag{Key: t[0], Value: t[1]})
	}
	return i
}

// decodeHostInventory reads an inventory, gzipped JSON.
func decodeHostInventory(r io.Reader) (*hostInventory, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, err
	}
	defer func() { _ = gz.Close() }()
	var inv hostInventory
	if err := json.NewDecoder(io.LimitReader(gz, maxHostInventory)).Decode(&inv); err != nil {
		return nil, err
	}
	if inv.Format != hostFormat {
		return nil, fmt.Errorf("inventory format %d, want %d", inv.Format, hostFormat)
	}
	return &inv, nil
}

// hostIndex is a helper's inventory as a device uses it. A nil one has
// nothing.
type hostIndex struct {
	f      fs.Fs
	files  map[string]*hostFile
	images map[string]hostImage
	tags   bool // the helper's parser is this device's
	hashes bool // and its audio hashes are made the same way
}

// loadHostIndex reads the helper's inventory from the library folder f,
// or returns nil if there is none or it can't be used.
func loadHostIndex(ctx context.Context, f fs.Fs) *hostIndex {
	o, err := f.NewObject(ctx, hostInventoryFile)
	if err != nil {
		return nil // no helper: the usual case
	}
	// The last one read, if the file hasn't changed since: a sync's
	// indexing and its hash pass both want it.
	key := fmt.Sprintf("%s\x00%d\x00%d", fs.ConfigString(f), o.Size(), o.ModTime(ctx).UnixNano())
	hostCache.Lock()
	defer hostCache.Unlock()
	if hostCache.key == key {
		return hostCache.h
	}
	h := readHostIndex(ctx, f, o)
	if h != nil {
		hostCache.key, hostCache.h = key, h
	}
	return h
}

// hostCache is the last inventory loadHostIndex read.
var hostCache struct {
	sync.Mutex
	key string
	h   *hostIndex
}

func readHostIndex(ctx context.Context, f fs.Fs, o fs.Object) *hostIndex {
	rc, err := o.Open(ctx)
	if err != nil {
		fs.Errorf(nil, "host inventory: %v", err)
		return nil
	}
	defer func() { _ = rc.Close() }()
	// In large reads: on SMB each read is a round trip.
	inv, err := decodeHostInventory(bufio.NewReaderSize(ctxReader{ctx, rc}, 4<<20))
	if err != nil {
		fs.Errorf(nil, "host inventory: %v", err)
		return nil
	}
	h := &hostIndex{f: f, files: make(map[string]*hostFile, len(inv.Files)), images: inv.Images,
		tags:   inv.ParserVersion == media.ParserVersion,
		hashes: inv.AudioHashVersion == index.AudioHashVersion}
	for i := range inv.Files {
		h.files[inv.Files[i].Path] = &inv.Files[i]
	}
	fs.Infof(nil, "host inventory: %d files from helper %s (version %s, sequence %d, %s); tags %v, hashes %v",
		len(inv.Files), inv.Helper, inv.Version, inv.Sequence, inv.Time.Format(time.RFC3339), h.tags, h.hashes)
	return h
}

// row returns the row for exactly this file: path, size and time.
func (h *hostIndex) row(p string, size, mtime int64) *hostFile {
	if h == nil {
		return nil
	}
	r := h.files[p]
	if r == nil || r.Size != size || r.MTime != mtime {
		return nil
	}
	return r
}

// info returns o's tags and art from the inventory, storing the art
// this device lacks in pack, or false to read o's header instead.
func (h *hostIndex) info(ctx context.Context, db *index.DB, pack *artPack, o fs.Object) (*media.Info, []index.FileArt, bool) {
	if h == nil || !h.tags {
		return nil, nil, false
	}
	r := h.row(o.Remote(), o.Size(), o.ModTime(ctx).UnixMilli())
	if r == nil || r.Info == nil {
		return nil, nil, false
	}
	var arts []index.FileArt
	for _, a := range r.Art {
		sha, err := hex.DecodeString(a.SHA256)
		if err != nil || len(sha) != sha256.Size {
			return nil, nil, false
		}
		have, err := db.HasArt(sha)
		if err != nil {
			return nil, nil, false
		}
		if !have {
			if err := h.storeArt(ctx, pack, a, sha); err != nil {
				fs.Debugf(o, "host inventory: art %s: %v; reading the file", a.SHA256, err)
				return nil, nil, false
			}
		}
		arts = append(arts, index.FileArt{SHA256: sha, Role: a.Role})
	}
	return r.Info.mediaInfo(), arts, true
}

// storeArt copies one of the helper's images into pack, checking that
// it is the image its name says.
func (h *hostIndex) storeArt(ctx context.Context, pack *artPack, a hostArt, sha []byte) error {
	img, ok := h.images[a.SHA256]
	if !ok || img.Size <= 0 || img.Size > maxHeader {
		return fmt.Errorf("not in the inventory's images")
	}
	o, err := h.f.NewObject(ctx, hostArtPath(a.SHA256))
	if err != nil {
		return err
	}
	rc, err := o.Open(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	data, err := readSpan(io.LimitReader(ctxReader{ctx, rc}, img.Size+1), img.Size+1)
	if err != nil {
		return err
	}
	if sum := sha256.Sum256(data); !bytes.Equal(sum[:], sha) {
		return fmt.Errorf("its content doesn't match its name")
	}
	return pack.store(sha, media.Picture{Type: a.Role, MIME: img.MIME, Width: img.Width, Height: img.Height, Data: data})
}

// audioSHA returns the helper's audio hash for exactly this file, or nil.
func (h *hostIndex) audioSHA(p string, size, mtime int64) []byte {
	if h == nil || !h.hashes {
		return nil
	}
	r := h.row(p, size, mtime)
	if r == nil || r.AudioSHA256 == "" {
		return nil
	}
	sha, err := hex.DecodeString(r.AudioSHA256)
	if err != nil || len(sha) != sha256.Size {
		return nil
	}
	return sha
}
