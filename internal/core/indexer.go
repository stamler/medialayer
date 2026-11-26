package core

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // DecodeConfig for cover sizes
	_ "image/png"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"unicode"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/fs/walk"

	"github.com/stamler/medialayer/internal/index"
	"github.com/stamler/medialayer/internal/media"
)

const (
	// replica names this cloud in the index's location table.
	replica = "gdrive"
	// repoDir is the repository folder inside the library folder.
	repoDir = ".medialayer"
	// maxHeader bounds the FLAC header read, against files with huge
	// embedded pictures. A cut header still gives tags and STREAMINFO.
	maxHeader = 16 << 20
	// indexWorkers is how many files are read at once.
	indexWorkers = 8
	// maxFailures is how many failed paths Status keeps.
	maxFailures = 20
)

// mediaFormat returns the format the index stores for a path, or "".
func mediaFormat(p string) string {
	switch strings.ToLower(path.Ext(p)) {
	case ".flac":
		return "flac"
	case ".mp3":
		return "mp3"
	}
	return ""
}

// index lists the library folder and indexes every new or changed file.
// Files already indexed with the same size and modification time are
// skipped, so an interrupted run resumes where it stopped.
func (c *Core) index(ctx context.Context, f fs.Fs) error {
	c.setJob(func(j *jobStatus) { j.Phase, j.Done, j.Total = PhaseListing, 0, 0 })
	var objs []fs.Object
	images := map[string][]fs.Object{} // by folder
	err := walk.ListR(ctx, f, "", true, -1, walk.ListObjects, func(entries fs.DirEntries) error {
		for _, e := range entries {
			o, ok := e.(fs.Object)
			if !ok || strings.HasPrefix(o.Remote(), repoDir+"/") {
				continue
			}
			if isImage(o.Remote()) {
				dir := path.Dir(o.Remote())
				images[dir] = append(images[dir], o)
				continue
			}
			if mediaFormat(o.Remote()) == "" {
				continue
			}
			objs = append(objs, o)
		}
		c.setJob(func(j *jobStatus) { j.Done = int64(len(objs)) })
		return nil
	})
	if err != nil {
		return fmt.Errorf("list %s: %w", f.Root(), err)
	}

	db := c.currentDB()
	known, err := db.KnownFiles()
	if err != nil {
		return err
	}
	var todo []fs.Object
	var found, gone []string
	listed := map[string]bool{}
	for _, o := range objs {
		listed[o.Remote()] = true
		k, ok := known[o.Remote()]
		if ok && k.Size == o.Size() && k.MTime == o.ModTime(ctx).UnixMilli() {
			if k.Missing {
				found = append(found, k.ID)
			}
			continue
		}
		todo = append(todo, o)
	}
	moved, err := matchMoves(ctx, db, known, listed, todo)
	if err != nil {
		return err
	}
	for p, k := range known {
		if !listed[p] && !k.Missing && !moved[p] {
			gone = append(gone, k.ID)
		}
	}
	if len(found)+len(gone) > 0 {
		if err := db.MarkFilesChanged(); err != nil {
			return err
		}
		if err := db.SetMissing(found, false); err != nil {
			return err
		}
		if err := db.SetMissing(gone, true); err != nil {
			return err
		}
		c.countTracks(db)
	}
	pack := &artPack{dir: c.artDir(), db: db}
	defer pack.close()
	if len(todo) > 0 {
		if err := c.indexFiles(ctx, db, pack, todo); err != nil {
			return err
		}
	}
	if err := c.folderArt(ctx, db, pack, images); err != nil {
		return err
	}
	if err := pack.close(); err != nil {
		return fmt.Errorf("art pack: %w", err)
	}
	return ctx.Err()
}

// matchMoves finds files that were moved or renamed: a new path in todo
// whose file matches exactly one indexed file that is gone (from this
// listing or an earlier one), and that no other new path matches. That
// file's row takes the new path and keeps its id, so playlists keep it;
// indexing the new path then updates the row. Files match by size and
// SHA-256 when the index and the cloud both have one, otherwise by size,
// modification time and file name (a move to another folder). It returns
// the old paths that moved.
func matchMoves(ctx context.Context, db *index.DB, known map[string]index.Known, listed map[string]bool,
	todo []fs.Object) (map[string]bool, error) {
	bySize := map[int64][]string{} // gone paths
	for p, k := range known {
		if !listed[p] {
			bySize[k.Size] = append(bySize[k.Size], p)
		}
	}
	if len(bySize) == 0 {
		return nil, nil
	}
	claims := map[string][]fs.Object{} // gone path: new paths that match it
	for _, o := range todo {
		if _, ok := known[o.Remote()]; ok {
			continue // a changed file, not a new path
		}
		candidates := bySize[o.Size()]
		if len(candidates) == 0 {
			continue
		}
		var sha []byte
		if hasSHA(known, candidates) {
			s, _ := o.Hash(ctx, hash.SHA256) // "" where the cloud has none
			sha = index.HexSHA(s)
		}
		var match string
		n := 0
		for _, p := range candidates {
			k := known[p]
			same := false
			if sha != nil && k.SHA256 != nil {
				same = bytes.Equal(sha, k.SHA256)
			} else {
				same = k.MTime == o.ModTime(ctx).UnixMilli() && path.Base(p) == path.Base(o.Remote())
			}
			if same {
				match, n = p, n+1
			}
		}
		if n == 1 {
			claims[match] = append(claims[match], o)
		}
	}
	moved := map[string]bool{}
	marked := false
	for p, objs := range claims {
		if len(objs) != 1 {
			continue // two copies of the file: neither is known to be the move
		}
		// Marked first, so no move is ever in the index unmarked.
		if !marked {
			if err := db.MarkFilesChanged(); err != nil {
				return nil, err
			}
			marked = true
		}
		if err := db.MoveFile(known[p].ID, objs[0].Remote()); err != nil {
			return nil, fmt.Errorf("move %s to %s in the index: %w", p, objs[0].Remote(), err)
		}
		moved[p] = true
	}
	return moved, ctx.Err()
}

// hasSHA reports whether any of the known paths has a SHA-256.
func hasSHA(known map[string]index.Known, paths []string) bool {
	for _, p := range paths {
		if known[p].SHA256 != nil {
			return true
		}
	}
	return false
}

// indexFiles reads and indexes the given files, several at a time.
func (c *Core) indexFiles(ctx context.Context, db *index.DB, pack *artPack, todo []fs.Object) error {
	if err := db.MarkFilesChanged(); err != nil {
		return err
	}
	c.setJob(func(j *jobStatus) { j.Phase, j.Done, j.Total = PhaseIndexing, 0, int64(len(todo)) })

	work := make(chan fs.Object)
	var wg sync.WaitGroup
	for range indexWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for o := range work {
				c.setJob(func(j *jobStatus) { j.Current = o.Remote() })
				err := indexFile(ctx, db, pack, o)
				if err == nil {
					c.countTracks(db)
				}
				c.setJob(func(j *jobStatus) {
					j.Done++
					if err != nil {
						j.Failed++
						if len(j.Failures) < maxFailures {
							j.Failures = append(j.Failures, fmt.Sprintf("%s: %v", o.Remote(), err))
						}
						fs.Errorf(o, "index: %v", err)
					}
				})
			}
		}()
	}
	for _, o := range todo {
		select {
		case work <- o:
		case <-ctx.Done():
		}
	}
	close(work)
	wg.Wait()
	return ctx.Err()
}

// maxFolderImage bounds a folder cover's size; larger files are scans or
// posters, not covers.
const maxFolderImage = 20 << 20

func isImage(p string) bool {
	switch strings.ToLower(path.Ext(p)) {
	case ".jpg", ".jpeg", ".png":
		return true
	}
	return false
}

// pickCover chooses a folder's front cover from its images, or nil. Names
// like cover, folder or front win; then names that mention them; then a
// scene release's "00-....jpg" or a lone image. Backs, inlays, disc and
// booklet scans, and macOS "._" files are never picked.
func pickCover(imgs []fs.Object) fs.Object {
	var best fs.Object
	bestScore := 99
	candidates := 0
	for _, o := range imgs {
		if o.Size() <= 0 || o.Size() > maxFolderImage {
			continue
		}
		score, ok := coverScore(path.Base(o.Remote()))
		if !ok {
			continue
		}
		candidates++
		if score < bestScore || (score == bestScore &&
			strings.ToLower(o.Remote()) < strings.ToLower(best.Remote())) {
			best, bestScore = o, score
		}
	}
	if bestScore == 4 && candidates > 1 {
		return nil // several unnamed images: no way to tell which is the cover
	}
	return best
}

// coverScore ranks an image file name as a front cover, lower is better;
// ok is false for names that are not a front cover.
func coverScore(base string) (score int, ok bool) {
	if strings.HasPrefix(base, ".") {
		return 0, false // hidden, or macOS AppleDouble "._" metadata
	}
	name := strings.ToLower(strings.TrimSuffix(base, path.Ext(base)))
	var list []string
	words := map[string]bool{}
	for _, w := range strings.FieldsFunc(name, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		w = strings.TrimRightFunc(w, unicode.IsDigit) // "cd1" is "cd"
		list = append(list, w)
		words[w] = true
	}
	for _, w := range []string{"back", "rear", "inlay", "inside", "tray"} {
		if words[w] {
			return 0, false
		}
	}
	if strings.Contains(name, "backcover") || strings.Contains(name, "coverback") {
		return 0, false
	}
	front := words["front"] || words["cover"] || words["folder"] ||
		strings.Contains(name, "frontcover") || strings.Contains(name, "albumcover")
	scans := map[string]bool{"cd": true, "cds": true, "disc": true, "discs": true, "disk": true,
		"booklet": true, "matrix": true, "scan": true, "scans": true, "label": true}
	if strings.HasPrefix(name, "00") {
		// Scene releases name the cover 00-artist-album-source-year-group.jpg,
		// where the source ("cd") is a tag, not what the image shows. A scan
		// of the disc ends in it: 00-artist-album-cd.jpg.
		if len(list) > 0 && scans[list[len(list)-1]] {
			return 0, false
		}
		if front {
			return 1, true
		}
		return 2, true
	}
	if !front { // "CD Cover.jpg" is still a cover; "cd.jpg" is a disc scan
		for w := range scans {
			if words[w] {
				return 0, false
			}
		}
	}
	switch name {
	case "cover", "folder", "front", "albumart", "album":
		return 0, true
	}
	switch {
	case front:
		return 1, true
	}
	return 4, true
}

// discFolder matches the per-disc folders of a multi-disc album, whose
// cover is usually in the album folder above.
var discFolder = regexp.MustCompile(`(?i)^(cd|dis[ck])[\s_-]*\d+(\b|of)`)

// folderArt gives tracks with no art of their own the front cover from
// their folder, stored once in the art pack. It runs on every sync, so a
// cover added to a folder later is picked up; each folder's image is read
// at most once per run.
func (c *Core) folderArt(ctx context.Context, db *index.DB, pack *artPack, images map[string][]fs.Object) error {
	bare, err := db.FilesWithoutArt()
	if err != nil || len(bare) == 0 {
		return err
	}
	// Leave out images that already failed in this session, so the next
	// best one in the folder gets its turn.
	usable := map[string][]fs.Object{}
	for dir, imgs := range images {
		for _, o := range imgs {
			if !c.badCover(o, false) {
				usable[dir] = append(usable[dir], o)
			}
		}
	}
	images = usable
	byDir := map[string][]string{} // folder holding the cover -> file ids
	for p, id := range bare {
		dir := path.Dir(p)
		if pickCover(images[dir]) == nil && discFolder.MatchString(path.Base(dir)) {
			dir = path.Dir(dir) // CD1/01.flac: the cover is in the album folder
		}
		if len(images[dir]) > 0 {
			byDir[dir] = append(byDir[dir], id)
		}
	}
	if len(byDir) == 0 {
		return nil
	}
	c.setJob(func(j *jobStatus) { j.Phase, j.Done, j.Total, j.Current = PhaseIndexing, 0, int64(len(byDir)), "" })
	for dir, ids := range byDir {
		if err := ctx.Err(); err != nil {
			return err
		}
		o := pickCover(images[dir])
		if o == nil {
			c.setJob(func(j *jobStatus) { j.Done++ })
			continue
		}
		c.setJob(func(j *jobStatus) { j.Current = o.Remote() })
		err := func() error {
			rc, err := o.Open(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = rc.Close() }()
			data, err := io.ReadAll(io.LimitReader(rc, maxFolderImage+1))
			if err != nil {
				return err
			}
			if len(data) > maxFolderImage {
				return badImageError{error: fmt.Errorf("larger than %d MiB", maxFolderImage>>20)}
			}
			cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
			if err != nil {
				return badImageError{error: fmt.Errorf("not a readable image: %w", err)}
			}
			// A wide or tall image that isn't named as the cover, such as a
			// scene release's band photo, is not taken for one.
			if score, _ := coverScore(path.Base(o.Remote())); score >= 2 && !squarish(cfg.Width, cfg.Height) {
				return badImageError{error: fmt.Errorf("%d×%d is not a cover's shape", cfg.Width, cfg.Height), notCover: true}
			}
			mime := "image/jpeg"
			if strings.EqualFold(path.Ext(o.Remote()), ".png") {
				mime = "image/png"
			}
			sum := sha256.Sum256(data)
			if err := pack.store(sum[:], media.Picture{Type: 3, MIME: mime, Data: data}); err != nil {
				return err
			}
			if err := db.MarkFilesChanged(); err != nil {
				return err
			}
			return db.AddFileArt(ids, sum[:], 3)
		}()
		if errors.As(err, new(badImageError)) {
			c.badCover(o, true) // a network or disk error is retried next sync
		}
		var bad badImageError
		if errors.As(err, &bad) && bad.notCover {
			err = nil // not a failure: the folder just has no cover
		}
		c.setJob(func(j *jobStatus) {
			j.Done++
			if err != nil {
				j.Failed++
				if len(j.Failures) < maxFailures {
					j.Failures = append(j.Failures, fmt.Sprintf("%s: %v", o.Remote(), err))
				}
				fs.Errorf(o, "folder cover: %v", err)
			}
		})
	}
	return nil
}

// badImageError is a folder image that can't be a cover: too large, not
// decodable, or (notCover) not shaped like one.
type badImageError struct {
	error
	notCover bool // readable, but not shaped like a cover
}

// squarish reports whether an image is near enough to square to be a
// front cover.
func squarish(w, h int) bool {
	return w > 0 && h > 0 && w*10 <= h*13 && h*10 <= w*13
}

func (e badImageError) Unwrap() error { return e.error }

// badCover reports whether this version of an image failed as a cover
// earlier in this session, and with add, records that it did.
func (c *Core) badCover(o fs.Object, add bool) bool {
	key := fmt.Sprintf("%s|%d|%d", o.Remote(), o.Size(), o.ModTime(context.Background()).UnixNano())
	c.mu.Lock()
	defer c.mu.Unlock()
	if add {
		if c.badCovers == nil {
			c.badCovers = map[string]bool{}
		}
		c.badCovers[key] = true
	}
	return c.badCovers[key]
}

// currentDB returns the open index.
func (c *Core) currentDB() *index.DB {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.db
}

// indexFile reads one file's header with range requests and writes it to
// the index.
func indexFile(ctx context.Context, db *index.DB, pack *artPack, o fs.Object) error {
	fetch := func(offset, length int64) ([]byte, error) {
		if length <= 0 {
			return nil, nil
		}
		rc, err := o.Open(ctx, &fs.RangeOption{Start: offset, End: offset + length - 1})
		if err != nil {
			return nil, err
		}
		defer func() { _ = rc.Close() }()
		return io.ReadAll(io.LimitReader(rc, length))
	}
	var info *media.Info
	var err error
	switch mediaFormat(o.Remote()) {
	case "flac":
		var header []byte
		header, _, err = media.FLACHeader(fetch, o.Size(), maxHeader)
		if err == nil {
			info, err = media.ParseFLAC(header)
		}
	case "mp3":
		info, err = media.ParseMP3(fetch, o.Size())
	}
	if err != nil {
		return err
	}

	var arts []index.FileArt
	for _, p := range info.Pictures {
		sum := sha256.Sum256(p.Data)
		if err := pack.store(sum[:], p); err != nil {
			return err
		}
		arts = append(arts, index.FileArt{SHA256: sum[:], Role: p.Type})
	}
	sha, _ := o.Hash(ctx, hash.SHA256) // "" where the cloud has none
	var cloudID string
	if ider, ok := o.(fs.IDer); ok {
		cloudID = ider.ID()
	}
	info.Pictures = nil // stored in the pack; don't hold them in memory
	_, err = db.PutFile(index.File{
		Path:    o.Remote(),
		Size:    o.Size(),
		MTime:   o.ModTime(ctx).UnixMilli(),
		SHA256:  index.HexSHA(sha),
		Replica: replica,
		CloudID: cloudID,
		Info:    info,
		Art:     arts,
	})
	return err
}

// artPack appends new images to one pack file per indexing run. Packs are
// named with a ULID, so two devices never make the same name, and are
// immutable once the run ends.
type artPack struct {
	dir string
	db  *index.DB

	mu   sync.Mutex
	name string
	f    *os.File
	size int64
}

// store writes the image to the pack and records it, unless it is
// already stored.
func (a *artPack) store(sha []byte, p media.Picture) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if have, err := a.db.HasArt(sha); err != nil || have {
		return err
	}
	if a.f == nil {
		a.name = "pack-" + index.NewID() + ".bin"
		f, err := os.OpenFile(filepath.Join(a.dir, a.name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		a.f = f
	}
	if _, err := a.f.Write(p.Data); err != nil {
		// A partial write would shift every later offset: start a new
		// pack for the next image instead.
		_ = a.f.Sync() // earlier images in it are already in the index
		_ = a.f.Close()
		a.f, a.size = nil, 0
		return err
	}
	w, h := p.Width, p.Height
	if w == 0 || h == 0 {
		if cfg, _, err := image.DecodeConfig(bytes.NewReader(p.Data)); err == nil {
			w, h = cfg.Width, cfg.Height
		}
	}
	offset := a.size
	a.size += int64(len(p.Data))
	return a.db.PutArt(index.Art{
		SHA256: sha, MIME: p.MIME, Width: w, Height: h,
		Pack: a.name, Offset: offset, Length: int64(len(p.Data)),
	})
}

// close flushes the pack to disk. It is safe to call twice.
func (a *artPack) close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.f == nil {
		return nil
	}
	err := a.f.Sync()
	if cerr := a.f.Close(); err == nil {
		err = cerr
	}
	a.f = nil
	return err
}
