package core

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/hash"

	"github.com/stamler/medialayer/internal/index"
	"github.com/stamler/medialayer/internal/media"
)

// Audio hashes tell a tag edit from a change to the audio, and later
// the same track in two places. A file's audio_sha256 never covers its
// tags, so retagging a file leaves it as it was:
//
//   - Lossless files (FLAC) hash their decoded samples (see
//     media.HashFLACAudio), as the same audio can be encoded in several
//     ways.
//   - Lossy files (MP3) hash their audio bytes, the part left once the
//     tags at both ends are skipped (see media.AudioSpan): decoding them
//     again would not give back other audio.
//
// Indexing never reads audio. A background pass (hashpass.go) hashes
// every track of the primary after a sync, and sync hashes the two
// copies of a replaced file when it needs to (see mirror). Each file is
// read in full, once: the results are kept on this device in
// audio-hashes.json, by the file's MD5 where the cloud lists one (so a
// copy on another cloud, or the primary's file once it is on a
// secondary, is known too) and otherwise by folder, path, size and
// modification time. The primary's hash also goes into the index's
// audio_sha256, which reaches other devices with the next index version.

const (
	audioHashFile = "audio-hashes.json"
	// maxAudioHashes bounds audio-hashes.json, about 120 bytes an entry.
	maxAudioHashes = 50_000
)

// audioKey is the key a file's audio hash is kept under. It starts with
// index.AudioHashVersion, so hashes made another way are never reused.
func audioKey(ctx context.Context, f fs.Fs, o fs.Object) string {
	if !f.Features().SlowHash {
		if md5, _ := o.Hash(ctx, hash.MD5); md5 != "" {
			return fmt.Sprintf("v%s:md5:%s:%d", index.AudioHashVersion, md5, o.Size())
		}
	}
	return fmt.Sprintf("v%s:at:%s\x00%s:%d:%d", index.AudioHashVersion, fs.ConfigString(f), o.Remote(), o.Size(), o.ModTime(ctx).UnixNano())
}

// loadAudioHashes reads audio-hashes.json once. The caller holds c.mu.
func (c *Core) loadAudioHashes() {
	if c.audioHashes != nil {
		return
	}
	c.audioHashes = map[string]string{}
	b, err := os.ReadFile(filepath.Join(c.dataDir, audioHashFile))
	if err == nil {
		err = json.Unmarshal(b, &c.audioHashes)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		fs.Errorf(nil, "%s: %v", audioHashFile, err)
		c.audioHashes = map[string]string{}
	}
}

// saveAudioHashes writes audio-hashes.json if it changed, dropping
// entries past the bound except those of this sync, keep.
func (c *Core) saveAudioHashes(keep map[string]bool) error {
	c.mu.Lock()
	if !c.audioHashesDirty {
		c.mu.Unlock()
		return nil
	}
	for k := range c.audioHashes {
		if len(c.audioHashes) <= maxAudioHashes {
			break
		}
		if !keep[k] {
			delete(c.audioHashes, k)
		}
	}
	b, err := json.Marshal(c.audioHashes)
	c.audioHashesDirty = false
	c.mu.Unlock()
	if err == nil {
		// A job's save and a stopping pass's can overlap.
		c.audioFileMu.Lock()
		err = writeFileAtomic(filepath.Join(c.dataDir, audioHashFile), b)
		c.audioFileMu.Unlock()
	}
	if err != nil {
		c.mu.Lock()
		c.audioHashesDirty = true
		c.mu.Unlock()
	}
	return err
}

// audioSHA returns the audio hash of o, a file in f, reading it in full
// unless the hash is known.
func (c *Core) audioSHA(ctx context.Context, f fs.Fs, o fs.Object) ([]byte, error) {
	key := audioKey(ctx, f, o)
	c.mu.Lock()
	c.loadAudioHashes()
	known := c.audioHashes[key]
	c.mu.Unlock()
	// The index's audio_sha256 is never read back here: a file rewritten
	// with its size and time kept would still match its row.
	sha, err := hex.DecodeString(known)
	if known == "" || err != nil || len(sha) != sha256.Size {
		if sha, err = hashAudio(ctx, o); err != nil {
			return nil, err
		}
		c.rememberAudioSHA(key, sha)
	}
	return sha, nil
}

// storeAudioSHA records the primary file o's hash in db, reporting
// whether a row took it.
func storeAudioSHA(ctx context.Context, db *index.DB, o fs.Object, sha []byte) (bool, error) {
	return db.SetAudioSHA(o.Remote(), o.Size(), o.ModTime(ctx).UnixMilli(), sha)
}

func (c *Core) rememberAudioSHA(key string, sha []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.audioHashes[key] = hex.EncodeToString(sha)
	c.audioHashesDirty = true
}

// hashAudio reads o in full and returns its audio hash.
func hashAudio(ctx context.Context, o fs.Object) ([]byte, error) {
	switch mediaFormat(o.Remote()) {
	case "flac":
		rc, err := o.Open(ctx)
		if err != nil {
			return nil, err
		}
		defer func() { _ = rc.Close() }()
		return media.HashFLACAudio(bufio.NewReaderSize(ctxReader{ctx, rc}, 1<<20))
	case "mp3":
		return hashAudioBytes(ctx, o, "mp3")
	case "m4a":
		return media.HashMP4Audio(objectFetch(ctx, o), objectOpen(ctx, o), o.Size())
	case "caf":
		return media.HashCAFAudio(objectFetch(ctx, o), objectOpen(ctx, o), o.Size())
	}
	return nil, fmt.Errorf("%s is not an audio file MediaLayer reads", path.Base(o.Remote()))
}

// ctxReader stops reading once ctx is done, as some backends' reads
// (a local disk's) don't watch it.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (r ctxReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

// objectFetch reads small pieces of o with range requests.
func objectFetch(ctx context.Context, o fs.Object) media.FetchFunc {
	return func(offset, length int64) ([]byte, error) {
		if length <= 0 {
			return nil, nil
		}
		rc, err := o.Open(ctx, &fs.RangeOption{Start: offset, End: offset + length - 1})
		if err != nil {
			return nil, err
		}
		defer func() { _ = rc.Close() }()
		return io.ReadAll(io.LimitReader(ctxReader{ctx, rc}, length))
	}
}

// objectOpen opens a stretch of o for a long read.
func objectOpen(ctx context.Context, o fs.Object) media.OpenFunc {
	return func(offset, length int64) (io.ReadCloser, error) {
		rc, err := o.Open(ctx, &fs.RangeOption{Start: offset, End: offset + length - 1})
		if err != nil {
			return nil, err
		}
		return struct {
			io.Reader
			io.Closer
		}{bufio.NewReaderSize(ctxReader{ctx, io.LimitReader(rc, length)}, 1<<20), rc}, nil
	}
}

// hashAudioBytes returns the SHA-256 of o's audio bytes, for a lossy
// format.
func hashAudioBytes(ctx context.Context, o fs.Object, format string) ([]byte, error) {
	fetch := objectFetch(ctx, o)
	span, err := media.AudioSpan(fetch, o.Size(), format)
	if err != nil {
		return nil, err
	}
	start, end := span.Start, span.End
	rc, err := o.Open(ctx, &fs.RangeOption{Start: start, End: end - 1})
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(ctxReader{ctx, rc}, end-start))
	if err != nil {
		return nil, err
	}
	if n != end-start {
		return nil, fmt.Errorf("read %d of %d audio bytes: %w", n, end-start, io.ErrUnexpectedEOF)
	}
	return h.Sum(nil), nil
}

// tagOnly returns the replacements whose two copies, the primary's file
// and the secondary's, hold the same audio: tag edits. A file that can't
// be read or parsed counts as a real change.
func (c *Core) tagOnly(ctx context.Context, f, dst fs.Fs, src *listing, replaces []string,
	lookup func(string) (fs.Object, bool)) (map[string]bool, error) {
	out := map[string]bool{}
	keep := map[string]bool{}
	defer func() {
		if err := c.saveAudioHashes(keep); err != nil {
			fs.Errorf(nil, "save %s: %v", audioHashFile, err)
		}
	}()
	for _, p := range replaces {
		if mediaFormat(p) == "" {
			continue
		}
		old, ok := lookup(p)
		if !ok {
			continue
		}
		c.setJob(func(j *jobStatus) { j.Current = "Comparing audio: " + path.Base(p) })
		keep[audioKey(ctx, f, src.files[p])], keep[audioKey(ctx, dst, old)] = true, true
		a, err := c.audioSHA(ctx, f, src.files[p])
		if err == nil {
			if db := c.currentDB(); db != nil { // a job: nothing else writes the index
				if _, err := storeAudioSHA(ctx, db, src.files[p], a); err != nil {
					fs.Errorf(src.files[p], "store audio hash: %v", err)
				}
			}
		}
		var b []byte
		if err == nil {
			b, err = c.audioSHA(ctx, dst, old)
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err != nil {
			fs.Logf(nil, "secondary %s: can't compare the audio of %s, so it counts as changed: %v", dst.Root(), p, err)
			continue
		}
		if string(a) == string(b) {
			out[p] = true
		}
	}
	return out, nil
}
