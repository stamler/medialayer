// Package readonly wraps an rclone Fs so that nothing can be written
// through it: listing, looking up and reading files work, and every
// write (a file, a folder, a time, a removal, a move or copy on the
// server) is refused before it reaches the backend.
//
// It is for opening a folder that must not change, such as a user's main
// music folder in a timing test, or later a library shared read-only
// (milestone 6).
package readonly

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/hash"
)

// ErrReadOnly is returned by every write.
var ErrReadOnly = errors.New("refused: this folder is opened read-only")

// Fs is f, read-only. It doesn't embed f, so no method of f other than
// those below can be reached through it, and its Features name none of
// f's optional writers (copy, move, purge, put stream, set metadata and
// so on), so rclone's operations can't find them either.
type Fs struct {
	f    fs.Fs
	feat *fs.Features
}

// Wrap returns f read-only.
func Wrap(f fs.Fs) *Fs {
	r := &Fs{f: f}
	inner := f.Features()
	// Only plain flags are kept; every optional method stays nil.
	r.feat = (&fs.Features{
		CaseInsensitive:         inner.CaseInsensitive,
		DuplicateFiles:          inner.DuplicateFiles,
		ReadMimeType:            inner.ReadMimeType,
		CanHaveEmptyDirectories: inner.CanHaveEmptyDirectories,
		BucketBased:             inner.BucketBased,
		SlowModTime:             inner.SlowModTime,
		SlowHash:                inner.SlowHash,
		IsLocal:                 inner.IsLocal,
	}).Fill(context.Background(), r)
	return r
}

func (r *Fs) Name() string             { return r.f.Name() }
func (r *Fs) Root() string             { return r.f.Root() }
func (r *Fs) String() string           { return "read-only " + r.f.String() }
func (r *Fs) Precision() time.Duration { return r.f.Precision() }
func (r *Fs) Hashes() hash.Set         { return r.f.Hashes() }
func (r *Fs) Features() *fs.Features   { return r.feat }

// List lists dir, with its files read-only.
func (r *Fs) List(ctx context.Context, dir string) (fs.DirEntries, error) {
	entries, err := r.f.List(ctx, dir)
	if err != nil {
		return nil, err
	}
	out := make(fs.DirEntries, 0, len(entries))
	for _, e := range entries {
		switch e := e.(type) {
		case fs.Object:
			out = append(out, &Object{o: e, fs: r})
		case fs.Directory:
			out = append(out, fs.NewDirCopy(ctx, e).SetRemote(e.Remote()))
		default:
			return nil, fmt.Errorf("unexpected entry %T", e)
		}
	}
	return out, nil
}

// NewObject looks up remote, read-only.
func (r *Fs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	o, err := r.f.NewObject(ctx, remote)
	if err != nil {
		return nil, err
	}
	return &Object{o: o, fs: r}, nil
}

func (r *Fs) Put(context.Context, io.Reader, fs.ObjectInfo, ...fs.OpenOption) (fs.Object, error) {
	return nil, ErrReadOnly
}
func (r *Fs) Mkdir(context.Context, string) error { return ErrReadOnly }
func (r *Fs) Rmdir(context.Context, string) error { return ErrReadOnly }

// Shutdown closes the wrapped backend's connections, if it has any.
func (r *Fs) Shutdown(ctx context.Context) error {
	if do := r.f.Features().Shutdown; do != nil {
		return do(ctx)
	}
	return nil
}

// Object is a file, read-only. Like Fs, it doesn't embed the backend's
// object, so none of its optional methods (such as SetMetadata) can be
// found on it.
type Object struct {
	o  fs.Object
	fs *Fs
}

func (o *Object) Fs() fs.Info                           { return o.fs }
func (o *Object) String() string                        { return o.o.String() }
func (o *Object) Remote() string                        { return o.o.Remote() }
func (o *Object) ModTime(ctx context.Context) time.Time { return o.o.ModTime(ctx) }
func (o *Object) Size() int64                           { return o.o.Size() }
func (o *Object) Storable() bool                        { return o.o.Storable() }
func (o *Object) Hash(ctx context.Context, ty hash.Type) (string, error) {
	return o.o.Hash(ctx, ty)
}

// Open opens the file for reading.
func (o *Object) Open(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {
	return o.o.Open(ctx, options...)
}

func (o *Object) SetModTime(context.Context, time.Time) error { return ErrReadOnly }
func (o *Object) Update(context.Context, io.Reader, fs.ObjectInfo, ...fs.OpenOption) error {
	return ErrReadOnly
}
func (o *Object) Remove(context.Context) error { return ErrReadOnly }

var (
	_ fs.Fs     = (*Fs)(nil)
	_ fs.Object = (*Object)(nil)
)
