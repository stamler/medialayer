package core

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"image"
	_ "image/jpeg" // DecodeConfig for cover sizes
	_ "image/png"
	"io"
	"os"
	"path"
	"path/filepath"
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
	for p, k := range known {
		if !listed[p] && !k.Missing {
			gone = append(gone, k.ID)
		}
	}
	if len(found)+len(gone) > 0 {
		if err := db.SetMeta("dirty", "1"); err != nil {
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

// indexFiles reads and indexes the given files, several at a time.
func (c *Core) indexFiles(ctx context.Context, db *index.DB, pack *artPack, todo []fs.Object) error {
	if err := db.SetMeta("dirty", "1"); err != nil {
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
// lone image, or a scene-release "00-....jpg". Backs, discs, inlays and
// other scans are never picked.
func pickCover(imgs []fs.Object) fs.Object {
	var best fs.Object
	bestScore := 99
	candidates := 0
	for _, o := range imgs {
		if o.Size() <= 0 || o.Size() > maxFolderImage {
			continue
		}
		name := strings.ToLower(strings.TrimSuffix(path.Base(o.Remote()), path.Ext(o.Remote())))
		words := map[string]bool{}
		for _, w := range strings.FieldsFunc(name, func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsDigit(r)
		}) {
			words[w] = true
		}
		if words["back"] || words["inlay"] || words["inside"] || words["tray"] {
			continue
		}
		var score int
		switch {
		case name == "cover" || name == "folder" || name == "front" || name == "albumart" || name == "album":
			score = 0
		case words["front"] || words["cover"] || words["folder"]:
			score = 1
		case strings.HasPrefix(name, "00"): // scene releases: 00-artist-album-....jpg
			score = 2
		case words["cd"] || words["disc"] || words["booklet"] || words["matrix"] || words["scan"]:
			continue
		default:
			score = 4
		}
		candidates++
		if score < bestScore || (score == bestScore && o.Remote() < best.Remote()) {
			best, bestScore = o, score
		}
	}
	if bestScore == 4 && candidates > 1 {
		return nil // several unnamed images: no way to tell which is the cover
	}
	return best
}

// folderArt gives tracks with no art of their own the front cover from
// their folder, stored once in the art pack. It runs on every sync, so a
// cover added to a folder later is picked up; each folder's image is read
// at most once per run.
func (c *Core) folderArt(ctx context.Context, db *index.DB, pack *artPack, images map[string][]fs.Object) error {
	bare, err := db.FilesWithoutArt()
	if err != nil || len(bare) == 0 {
		return err
	}
	byDir := map[string][]string{} // folder -> file ids
	for p, id := range bare {
		dir := path.Dir(p)
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
			mime := "image/jpeg"
			if strings.EqualFold(path.Ext(o.Remote()), ".png") {
				mime = "image/png"
			}
			sum := sha256.Sum256(data)
			if err := pack.store(sum[:], media.Picture{Type: 3, MIME: mime, Data: data}); err != nil {
				return err
			}
			if err := db.SetMeta("dirty", "1"); err != nil {
				return err
			}
			return db.AddFileArt(ids, sum[:], 3)
		}()
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
