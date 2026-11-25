package medialayer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/fs/walk"

	"github.com/stamler/medialayer/internal/media"
)

var commandHelp = []fs.CommandHelp{{
	Name:  "prime",
	Short: "Store the metadata bytes of every media file.",
	Long: `Walks the remote (or the directory given as an argument), reads the
bytes before the audio of every media file with ranged reads, and stores
them so later tag and cover reads are answered locally. Files whose stored
header still matches their size and modification time are skipped.

    rclone backend prime ml:
    rclone backend prime ml: "Artist/Album"
`,
}, {
	Name:  "stats",
	Short: "Show how reads were served since the remote was created.",
}, {
	Name:  "drop",
	Short: "Delete every stored header.",
}}

// Command runs a backend command.
func (f *Fs) Command(ctx context.Context, name string, arg []string, opt map[string]string) (any, error) {
	switch name {
	case "prime":
		dir := ""
		if len(arg) > 0 {
			dir = arg[0]
		}
		return f.prime(ctx, dir)
	case "stats":
		return map[string]int64{
			"opens_with_header":  f.stats.opens.Load(),
			"served_locally":     f.stats.localOpens.Load(),
			"passed_through":     f.stats.passthroughOpens.Load(),
			"streams_continued":  f.stats.reusedStreams.Load(),
			"read_past_header":   f.stats.cloudFallbacks.Load(),
			"stale_records_seen": f.stats.staleRecords.Load(),
		}, nil
	case "drop":
		return nil, f.store.drop()
	default:
		return nil, fs.ErrorCommandNotFound
	}
}

// primeResult summarises a prime run. It is JSON-encoded for the user.
type primeResult struct {
	Primed        int64    `json:"primed"`
	AlreadyFresh  int64    `json:"already_fresh"`
	NotMedia      int64    `json:"skipped_not_flac"`
	Truncated     int64    `json:"truncated_at_max_header"`
	Failed        int64    `json:"failed"`
	Requests      int64    `json:"range_requests"`
	BytesRead     int64    `json:"bytes_read"`
	BytesStored   int64    `json:"bytes_stored"`
	Seconds       float64  `json:"seconds"`
	FirstFailures []string `json:"first_failures,omitempty"`
}

// prime stores headers for every media file under dir.
func (f *Fs) prime(ctx context.Context, dir string) (*primeResult, error) {
	if !f.canTrackChanges() {
		return nil, errors.New("the wrapped remote has neither modification times nor a quick content hash, so a changed file could not be told apart from its stored header")
	}
	start := time.Now()
	var objects []fs.Object
	err := walk.ListR(ctx, f.Fs, dir, true, -1, walk.ListObjects, func(entries fs.DirEntries) error {
		for _, entry := range entries {
			if o, ok := entry.(fs.Object); ok && f.isMedia(o.Remote()) {
				objects = append(objects, o)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("listing %q: %w", dir, err)
	}

	var (
		res       primeResult
		primed    atomic.Int64
		fresh     atomic.Int64
		notMedia  atomic.Int64
		truncated atomic.Int64
		failed    atomic.Int64
		requests  atomic.Int64
		bytesRead atomic.Int64
		stored    atomic.Int64
		failMu    sync.Mutex
		work      = make(chan fs.Object)
		wg        sync.WaitGroup
		workers   = max(fs.GetConfig(ctx).Checkers, 1)
	)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for base := range work {
				// Checked here rather than with freshRecord, so priming
				// does not count towards the mount's stale_records_seen.
				if rec := f.store.get(f.key(base.Remote())); rec != nil && f.matches(ctx, rec, base) &&
					(!rec.Complete || int64(len(rec.Header)) >= f.storeLen(int64(len(rec.Header)), base.Size())) {
					fresh.Add(1)
					continue
				}
				fetch := func(offset, length int64) ([]byte, error) {
					requests.Add(1)
					rc, err := base.Open(ctx, &fs.RangeOption{Start: offset, End: offset + length - 1})
					if err != nil {
						return nil, err
					}
					defer func() { _ = rc.Close() }()
					buf, err := io.ReadAll(io.LimitReader(rc, length))
					bytesRead.Add(int64(len(buf)))
					return buf, err
				}
				header, complete, err := media.FLACHeader(fetch, base.Size(), int64(f.opt.MaxHeader))
				if err == nil && complete {
					if want := f.storeLen(int64(len(header)), base.Size()); want > int64(len(header)) {
						more, ferr := fetch(int64(len(header)), want-int64(len(header)))
						if ferr == nil && int64(len(more)) != want-int64(len(header)) {
							ferr = io.ErrUnexpectedEOF
						}
						if ferr == nil {
							header = append(header, more...)
						} else {
							// Keep the metadata; the next prime tops it up.
							fs.Debugf(base, "medialayer: storing metadata only: %v", ferr)
						}
					}
				}
				if errors.Is(err, media.ErrNotFLAC) {
					notMedia.Add(1)
					fs.Debugf(base, "medialayer: not primed: %v", err)
					continue
				}
				if err == nil {
					rec := &headerRecord{
						Size:     base.Size(),
						ModTime:  base.ModTime(ctx),
						Complete: complete,
						Header:   header,
					}
					if f.hashType != hash.None {
						if sum, herr := base.Hash(ctx, f.hashType); herr == nil && sum != "" {
							rec.HashType, rec.Hash = f.hashType, sum
						}
					}
					if f.matches(ctx, rec, base) {
						err = f.store.put(f.key(base.Remote()), rec)
					} else {
						err = errors.New("no modification time or hash to check this file against later")
					}
				}
				if err != nil {
					failed.Add(1)
					fs.Errorf(base, "medialayer: prime failed: %v", err)
					failMu.Lock()
					if len(res.FirstFailures) < 10 {
						res.FirstFailures = append(res.FirstFailures, fmt.Sprintf("%s: %v", base.Remote(), err))
					}
					failMu.Unlock()
					continue
				}
				if !complete {
					truncated.Add(1)
				}
				primed.Add(1)
				stored.Add(int64(len(header)))
			}
		}()
	}
	for _, o := range objects {
		select {
		case work <- o:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
	}
	close(work)
	wg.Wait()

	res.Primed = primed.Load()
	res.AlreadyFresh = fresh.Load()
	res.NotMedia = notMedia.Load()
	res.Truncated = truncated.Load()
	res.Failed = failed.Load()
	res.Requests = requests.Load()
	res.BytesRead = bytesRead.Load()
	res.BytesStored = stored.Load()
	res.Seconds = time.Since(start).Seconds()
	return &res, ctx.Err()
}

// storeLen is how many bytes of a file of this size to keep when its
// metadata takes metaLen: the metadata rounded up to the next multiple of
// store_to_block, within the file and max_header. An app reading through a
// buffer of that block size then never needs a byte that is not stored.
func (f *Fs) storeLen(metaLen, size int64) int64 {
	n := metaLen
	if b := int64(f.opt.StoreTo); b > 0 {
		n = (metaLen + b - 1) / b * b
	}
	return max(metaLen, min(n, size, int64(f.opt.MaxHeader)))
}
