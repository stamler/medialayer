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
	err := walk.ListR(ctx, f, "", true, -1, walk.ListObjects, func(entries fs.DirEntries) error {
		for _, e := range entries {
			o, ok := e.(fs.Object)
			if !ok || strings.HasPrefix(o.Remote(), repoDir+"/") || mediaFormat(o.Remote()) == "" {
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
	if len(todo) == 0 {
		return nil
	}
	if err := db.SetMeta("dirty", "1"); err != nil {
		return err
	}

	c.setJob(func(j *jobStatus) { j.Phase, j.Done, j.Total = PhaseIndexing, 0, int64(len(todo)) })
	pack := &artPack{dir: c.artDir(), db: db}
	defer pack.close()

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
	if err := pack.close(); err != nil {
		return fmt.Errorf("art pack: %w", err)
	}
	return ctx.Err()
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
