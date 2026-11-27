package core

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	iofs "io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/walk"

	"github.com/stamler/medialayer/internal/index"
	"github.com/stamler/medialayer/internal/media"
)

// The host helper's side of host.go: mlhost runs RunHost on the machine
// that serves the library, reading it from its own disk.

// HostOptions are mlhost's settings.
type HostOptions struct {
	Folder string // the library folder, on this machine's disk
	// Interval is how often the folder is scanned again; 0 scans once,
	// hashing everything, and returns.
	Interval time.Duration
	Version  string // mlhost's, for status.json
}

// HostStatus is status.json, for people and for a device's log.
type HostStatus struct {
	Helper           string    `json:"helper"`
	Version          string    `json:"version"`
	Format           int       `json:"format"`
	ParserVersion    string    `json:"parser_version"`
	AudioHashVersion string    `json:"audio_hash_version"`
	Sequence         int64     `json:"sequence"`
	Time             time.Time `json:"time"`
	Files            int       `json:"files"`  // media files
	Tagged           int       `json:"tagged"` // with tags
	Hashed           int       `json:"hashed"` // with an audio hash
	Failed           []string  `json:"failed,omitempty"`
}

// maxHostFailures is how many failed files status.json names.
const maxHostFailures = 50

// hostWriteEvery is how often hashing publishes what it has.
const hostWriteEvery = 2 * time.Minute

type hostScanner struct {
	opt    HostOptions
	folder string   // absolute
	root   *os.Root // .medialayer/host: every write goes through it
	f      fs.Fs
	inv    hostInventory
	logf   func(format string, args ...any)
}

// ErrNoRepo: the folder has no .medialayer yet. The helper never makes
// it: a device does, as the user it writes the share with, when the
// folder is picked as the music folder.
var ErrNoRepo = errors.New("this folder has no .medialayer folder yet: pick it as the music folder in MediaLayer first, then start mlhost")

// Names inside .medialayer/host.
var (
	hostInventoryName = strings.TrimPrefix(hostInventoryFile, hostDir+"/")
	hostStatusName    = strings.TrimPrefix(hostStatusFile, hostDir+"/")
	hostArtName       = strings.TrimPrefix(hostArtDir, hostDir+"/")
)

// openHostDir opens the library's .medialayer/host, making host and
// host/art if need be, as real folders readable by everyone. It refuses
// a .medialayer, host or art that is a symbolic link: the helper writes
// nowhere else, and os.Root keeps every later path inside host.
func openHostDir(folder string) (*os.Root, error) {
	if err := realDir(filepath.Join(folder, repoDir)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNoRepo
		}
		return nil, err
	}
	before, err := os.Lstat(filepath.Join(folder, repoDir))
	if err != nil {
		return nil, err
	}
	repo, err := os.OpenRoot(filepath.Join(folder, repoDir))
	if err != nil {
		return nil, err
	}
	defer func() { _ = repo.Close() }()
	// Not a link swapped in since the check.
	if now, err := repo.Stat("."); err != nil || !os.SameFile(before, now) {
		return nil, fmt.Errorf("%s changed while it was opened", filepath.Join(folder, repoDir))
	}
	if err := ensureDir(repo, "host"); err != nil {
		return nil, err
	}
	hostBefore, err := repo.Lstat("host")
	if err != nil {
		return nil, err
	}
	root, err := repo.OpenRoot("host")
	if err != nil {
		return nil, err
	}
	if now, err := root.Stat("."); err != nil || !os.SameFile(hostBefore, now) {
		_ = root.Close()
		return nil, fmt.Errorf("%s/host changed while it was opened", repoDir)
	}
	if err := ensureDir(root, hostArtName); err != nil {
		_ = root.Close()
		return nil, err
	}
	return root, nil
}

// realDir checks that p is a folder and not a link to one.
func realDir(p string) error {
	st, err := os.Lstat(p)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("%s is not a folder (a link, or a file)", p)
	}
	return nil
}

// ensureDir makes name in root, or checks it is a real folder, and makes
// it readable by everyone whatever the umask.
func ensureDir(root *os.Root, name string) error {
	st, err := root.Lstat(name)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := root.Mkdir(name, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
	case err != nil:
		return err
	case !st.IsDir():
		return fmt.Errorf("%s in %s is not a folder (a link, or a file)", name, root.Name())
	}
	return root.Chmod(name, 0o755)
}

// RunHost scans the library folder, writes its inventory and keeps it
// current until ctx ends: a full scan at start and every Interval, with
// audio hashing in between. It writes only in the folder's
// .medialayer/host.
func RunHost(ctx context.Context, opt HostOptions, logf func(format string, args ...any)) error {
	abs, err := filepath.Abs(opt.Folder)
	if err != nil {
		return err
	}
	if st, err := os.Stat(abs); err != nil || !st.IsDir() {
		return fmt.Errorf("%s is not a folder", abs)
	}
	f, err := fs.NewFs(ctx, abs)
	if err != nil {
		return err
	}
	root, err := openHostDir(abs)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	h := &hostScanner{opt: opt, folder: abs, root: root, f: f, logf: logf}
	h.load()
	for {
		start := time.Now()
		if err := h.scan(ctx); err != nil {
			return err
		}
		if err := h.write(); err != nil {
			return err
		}
		var until time.Time // hash until the next scan, or all of it
		if opt.Interval > 0 {
			until = start.Add(opt.Interval)
		}
		if err := h.hash(ctx, until); err != nil {
			return err
		}
		if opt.Interval <= 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(time.Until(start.Add(opt.Interval))):
		}
	}
}

// local is a path in the library folder on disk.
func (h *hostScanner) local(rel string) string {
	return filepath.Join(h.folder, filepath.FromSlash(rel))
}

// load reads the last inventory, so a restart doesn't read and hash the
// library again.
func (h *hostScanner) load() {
	h.inv = hostInventory{Format: hostFormat}
	if b, err := h.root.Open(hostInventoryName); err == nil {
		inv, err := decodeHostInventory(b)
		_ = b.Close()
		if err != nil {
			h.logf("the last inventory can't be read (%v); starting a new one", err)
		} else {
			h.inv = *inv
		}
	}
	if h.inv.Helper == "" {
		h.inv.Helper = index.NewID()
	}
	// Rows made by other parsers or hashes are made again.
	if h.inv.ParserVersion != media.ParserVersion {
		for i := range h.inv.Files {
			h.inv.Files[i].Info, h.inv.Files[i].Art, h.inv.Files[i].Error = nil, nil, ""
			h.inv.Files[i].Change = "" // read again
		}
	}
	if h.inv.AudioHashVersion != index.AudioHashVersion {
		for i := range h.inv.Files {
			h.inv.Files[i].AudioSHA256 = ""
		}
	}
	// Files that couldn't be hashed are tried again once a start, in
	// case it was the disk and not the file.
	for i := range h.inv.Files {
		h.inv.Files[i].HashError = ""
	}
	h.inv.ParserVersion, h.inv.AudioHashVersion = media.ParserVersion, index.AudioHashVersion
	if h.inv.Images == nil {
		h.inv.Images = map[string]hostImage{}
	}
}

// scan lists the folder and reads every media file that is new or
// changed since its row was made.
func (h *hostScanner) scan(ctx context.Context) error {
	old := map[string]hostFile{}
	for _, r := range h.inv.Files {
		old[r.Path] = r
	}
	var files []hostFile
	var read []fs.Object
	// The change of each file to read. A row gets it only once it is
	// read: a row written before then (the writes while reading) with
	// it would be kept unread after a restart.
	changes := map[string]string{}
	err := walk.ListR(ctx, h.f, "", true, -1, walk.ListObjects, func(entries fs.DirEntries) error {
		for _, e := range entries {
			o, ok := e.(fs.Object)
			if !ok || strings.HasPrefix(o.Remote(), repoDir+"/") || mediaFormat(o.Remote()) == "" {
				continue
			}
			r := hostFile{Path: o.Remote(), Size: o.Size(), MTime: o.ModTime(ctx).UnixMilli(),
				Change: fileChange(h.local(o.Remote()))}
			prev, ok := old[r.Path]
			if ok && prev.Size == r.Size && prev.MTime == r.MTime && r.Change != "" {
				if prev.Change == r.Change {
					files = append(files, prev)
					continue
				}
				// Only the change time moved (a chmod, Samba's extended
				// attributes, a hard link made by a backup): the header
				// is read again, but the same file, size and time keep
				// its audio hash. The hash only ever reaches the index's
				// audio_sha256, never a sync decision.
				if sameInode(prev.Change, r.Change) {
					r.AudioSHA256 = prev.AudioSHA256
				}
			}
			changes[r.Path], r.Change = r.Change, ""
			files = append(files, r)
			read = append(read, o)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("list %s: %w", h.folder, err)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	byPath := map[string]int{}
	for i, r := range files {
		byPath[r.Path] = i
	}
	// Rows of changed files have lost their tags; the writes while
	// reading publish that, and what is read so far.
	h.inv.Files = files
	if len(read) > 0 {
		h.logf("reading %d new or changed files", len(read))
	}
	last := time.Now()
	for n, o := range read {
		if err := ctx.Err(); err != nil {
			return err
		}
		r := &h.inv.Files[byPath[o.Remote()]]
		info, err := readInfo(ctx, o)
		switch {
		case err == nil && fileChange(h.local(r.Path)) != changes[r.Path]:
			// Changed while it was read: no Change, so read again next scan.
		case err == nil && !validUTF8(info):
			// JSON would change the bytes; the device reads it itself.
		case err == nil:
			r.Info = toHostInfo(info)
			for _, p := range info.Pictures {
				sha, err := h.storeImage(p)
				if err != nil {
					return err // the disk: not this file's fault
				}
				r.Art = append(r.Art, hostArt{SHA256: sha, Role: p.Type})
			}
		case !errors.Is(err, media.ErrUnsupported):
			r.Error = err.Error()
		}
		if r.Info != nil || r.Error != "" || errors.Is(err, media.ErrUnsupported) {
			r.Change = changes[r.Path] // read: kept until the file changes
		}
		if time.Since(last) > hostWriteEvery {
			h.logf("read %d of %d", n+1, len(read))
			if err := h.write(); err != nil {
				return err
			}
			last = time.Now()
		}
	}
	h.pruneImages()
	return nil
}

// validUTF8 reports whether every string the inventory would hold for
// info is valid UTF-8, which JSON keeps as it is.
func validUTF8(info *media.Info) bool {
	ok := utf8.ValidString(info.Format)
	for _, t := range info.Tags {
		ok = ok && utf8.ValidString(t.Key) && utf8.ValidString(t.Value)
	}
	for _, p := range info.Pictures {
		ok = ok && utf8.ValidString(p.MIME)
	}
	return ok
}

// sameInode reports whether two fileChange values are of one inode.
func sameInode(a, b string) bool {
	ia, _, _ := strings.Cut(a, ":")
	ib, _, _ := strings.Cut(b, ":")
	return ia != "" && ia == ib
}

// storeImage writes an embedded picture to art/, once, and returns its
// SHA-256.
func (h *hostScanner) storeImage(p media.Picture) (string, error) {
	sum := sha256.Sum256(p.Data)
	sha := hex.EncodeToString(sum[:])
	name := strings.TrimPrefix(hostArtPath(sha), hostDir+"/")
	if _, ok := h.inv.Images[sha]; ok {
		if st, err := h.root.Lstat(name); err == nil && st.Mode().IsRegular() && st.Size() == int64(len(p.Data)) {
			return sha, nil
		}
	}
	if err := ensureDir(h.root, path.Dir(name)); err != nil {
		return "", err
	}
	if err := writePublic(h.root, name, p.Data); err != nil {
		return "", err
	}
	h.inv.Images[sha] = hostImage{MIME: p.MIME, Width: p.Width, Height: p.Height, Size: int64(len(p.Data))}
	return sha, nil
}

// pruneImages drops images no row uses, from the list and from art/.
func (h *hostScanner) pruneImages() {
	used := map[string]bool{}
	for _, r := range h.inv.Files {
		for _, a := range r.Art {
			used[a.SHA256] = true
		}
	}
	for sha := range h.inv.Images {
		if !used[sha] {
			delete(h.inv.Images, sha)
		}
	}
	// Through the root, which never leaves host; links aren't followed.
	_ = iofs.WalkDir(h.root.FS(), hostArtName, func(p string, d iofs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if _, ok := h.inv.Images[d.Name()]; !ok {
			_ = h.root.Remove(p) // unused, or a temporary file left by a crash
		}
		return nil
	})
	// Temporary files a crash left beside the inventory.
	if entries, err := iofs.ReadDir(h.root.FS(), "."); err == nil {
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".") && strings.HasSuffix(e.Name(), ".tmp") && e.Type().IsRegular() {
				_ = h.root.Remove(e.Name())
			}
		}
	}
}

// hash makes the audio hashes the inventory lacks, writing the
// inventory every few minutes, until it is done or the time until (if
// set) has passed.
func (h *hostScanner) hash(ctx context.Context, until time.Time) error {
	var todo []int
	for i, r := range h.inv.Files {
		if r.AudioSHA256 == "" && r.HashError == "" && r.Info != nil {
			todo = append(todo, i)
		}
	}
	if len(todo) == 0 {
		return nil
	}
	h.logf("hashing the audio of %d files", len(todo))
	last := time.Now()
	changed := false
	for n, i := range todo {
		if ctx.Err() != nil || (!until.IsZero() && time.Now().After(until)) {
			break
		}
		r := &h.inv.Files[i]
		o, err := h.f.NewObject(ctx, r.Path)
		if err != nil || o.Size() != r.Size || o.ModTime(ctx).UnixMilli() != r.MTime {
			continue // changed: the next scan reads it again
		}
		sha, err := hashAudio(ctx, o)
		if ctx.Err() != nil {
			break
		}
		// Only a file unchanged while it was read is vouched for.
		if fileChange(h.local(r.Path)) != r.Change {
			continue
		}
		if err != nil {
			r.HashError = err.Error()
		} else {
			r.AudioSHA256 = hex.EncodeToString(sha)
		}
		changed = true
		if time.Since(last) > hostWriteEvery {
			h.logf("hashed %d of %d", n+1, len(todo))
			if err := h.write(); err != nil {
				return err
			}
			last, changed = time.Now(), false
		}
	}
	if changed {
		return h.write()
	}
	return nil
}

// write publishes the inventory and status.json, each replaced whole.
func (h *hostScanner) write() error {
	h.inv.Sequence++
	h.inv.Time = time.Now().UTC()
	h.inv.Version = h.opt.Version
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if err := json.NewEncoder(gz).Encode(&h.inv); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	if err := writePublic(h.root, hostInventoryName, buf.Bytes()); err != nil {
		return err
	}
	st := HostStatus{Helper: h.inv.Helper, Version: h.opt.Version, Format: hostFormat,
		ParserVersion: h.inv.ParserVersion, AudioHashVersion: h.inv.AudioHashVersion,
		Sequence: h.inv.Sequence, Time: h.inv.Time, Files: len(h.inv.Files)}
	for _, r := range h.inv.Files {
		if r.Info != nil {
			st.Tagged++
		}
		if r.AudioSHA256 != "" {
			st.Hashed++
		}
		for _, e := range []string{r.Error, r.HashError} {
			if e != "" && len(st.Failed) < maxHostFailures {
				st.Failed = append(st.Failed, path.Clean(r.Path)+": "+e)
			}
		}
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return writePublic(h.root, hostStatusName, append(b, '\n'))
}

// writePublic replaces the file name in root with b, readable by
// everyone (the share's users read it), through a temporary file and a
// rename, so a reader gets the old file or the new one.
func writePublic(root *os.Root, name string, b []byte) error {
	tmp := path.Join(path.Dir(name), "."+path.Base(name)+"."+strings.ToLower(index.NewID())+".tmp")
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = root.Remove(tmp) }() // gone after the rename
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := root.Chmod(tmp, 0o644); err != nil {
		return err
	}
	return root.Rename(tmp, name)
}
