// Package medialayer is an rclone overlay backend that makes media metadata
// reads local.
//
// It wraps another remote (for example a Google Drive folder) and keeps,
// for every primed media file, the bytes an app reads to get tags and cover
// art. Reads that fall inside those bytes are answered locally; everything
// else goes to the wrapped remote. Mounting the overlay with rclone's
// nfsmount or mount therefore makes a library scan (Tauon, Picard ...) run at
// local speed while playback still streams from the cloud.
//
// This is spike code for deciding whether MediaLayer builds on rclone. The
// header store is a local directory standing in for the header packs that
// will live in the cloud repository.
package medialayer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rclone/rclone/backend/overview"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/cache"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/fs/fspath"
	"github.com/rclone/rclone/fs/list"
)

func init() {
	fs.Register(&fs.RegInfo{
		Name:        "medialayer",
		Description: "Media-aware overlay: serves tag and cover-art reads locally",
		NewFs:       NewFs,
		CommandHelp: commandHelp,
		// Out-of-tree backends have no entry in rclone's generated
		// overview table; an empty one keeps registration quiet.
		Overview: &overview.BackendConfig{},
		Options: []fs.Option{{
			Name:     "remote",
			Required: true,
			Help:     "Remote holding the media library (e.g. gdrive:Music).",
		}, {
			Name:    "header_dir",
			Default: "",
			Help:    "Local directory for primed headers.\n\nDefaults to medialayer/<remote name> inside rclone's cache directory.",
		}, {
			Name:     "max_header",
			Default:  fs.SizeSuffix(16 * 1024 * 1024),
			Advanced: true,
			Help:     "Most bytes kept per file. Larger metadata is kept up to this size.",
		}, {
			Name:     "extensions",
			Default:  fs.CommaSepList{"flac"},
			Advanced: true,
			Help:     "File extensions to prime and serve from the header store.",
		}},
	})
}

// Options defines the configuration for this backend.
type Options struct {
	Remote     string          `config:"remote"`
	HeaderDir  string          `config:"header_dir"`
	MaxHeader  fs.SizeSuffix   `config:"max_header"`
	Extensions fs.CommaSepList `config:"extensions"`
}

// Fs is the overlay filesystem. It embeds the wrapped remote, so anything
// not overridden here goes straight through.
type Fs struct {
	fs.Fs
	name     string
	root     string
	wrapper  fs.Fs
	features *fs.Features
	opt      Options
	store    *headerStore
	exts     map[string]bool
	stats    *stats
	stopOnce sync.Once
	stop     chan struct{} // closed by Shutdown to end the stats logger
}

// stats counts how reads were served. They are what the spike measures.
type stats struct {
	opens            atomic.Int64 // Open calls on media files with a record
	passthroughOpens atomic.Int64 // Open calls sent straight to the remote
	localOpens       atomic.Int64 // Open calls answered from the header store
	cloudFallbacks   atomic.Int64 // local opens that later had to read past the header
	staleRecords     atomic.Int64 // records ignored because the file changed
}

// NewFs constructs an Fs from the remote:path string.
func NewFs(ctx context.Context, name, root string, m configmap.Mapper) (fs.Fs, error) {
	var opt Options
	if err := configstruct.Set(m, &opt); err != nil {
		return nil, err
	}
	if strings.HasPrefix(opt.Remote, name+":") {
		return nil, errors.New("can't point medialayer remote at itself")
	}
	if opt.MaxHeader <= 0 {
		return nil, errors.New("max_header must be positive")
	}

	remotePath := fspath.JoinRootPath(opt.Remote, root)
	baseFs, err := cache.Get(ctx, remotePath)
	if err != nil && err != fs.ErrorIsFile {
		return nil, fmt.Errorf("failed to make base remote %q: %w", opt.Remote, err)
	}

	headerDir := opt.HeaderDir
	if headerDir == "" {
		headerDir = filepath.Join(config.GetCacheDir(), "medialayer", name)
	}
	store, err := newHeaderStore(headerDir)
	if err != nil {
		return nil, err
	}

	f := &Fs{
		Fs:    baseFs,
		name:  name,
		root:  root,
		opt:   opt,
		store: store,
		exts:  map[string]bool{},
		stats: &stats{},
		stop:  make(chan struct{}),
	}
	for _, ext := range opt.Extensions {
		f.exts["."+strings.ToLower(strings.TrimPrefix(ext, "."))] = true
	}
	if err == fs.ErrorIsFile {
		f.root = path.Dir(f.root)
		if f.root == "." || f.root == "/" {
			f.root = ""
		}
	}

	f.features = (&fs.Features{
		CanHaveEmptyDirectories: true,
		ReadMimeType:            true,
		WriteMimeType:           true,
		ReadMetadata:            true,
		WriteMetadata:           true,
		UserMetadata:            true,
		PartialUploads:          true,
	}).Fill(ctx, f).Mask(ctx, f.Fs).WrapsFs(f, f.Fs)
	f.features.ListP = f.ListP

	cache.PinUntilFinalized(f.Fs, f)
	go f.logStats(30 * time.Second)
	return f, err
}

// logStats logs the read statistics at INFO level (-v) whenever they
// change, so a mount's behaviour can be followed without the rc API.
func (f *Fs) logStats(every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	var last [5]int64
	for {
		select {
		case <-f.stop:
			return
		case <-ticker.C:
		}
		now := [5]int64{
			f.stats.opens.Load(), f.stats.localOpens.Load(), f.stats.passthroughOpens.Load(),
			f.stats.cloudFallbacks.Load(), f.stats.staleRecords.Load(),
		}
		if now != last {
			fs.Infof(f, "medialayer stats: %d opens with a stored header, %d served locally, %d passed through, %d read past the header, %d stale records",
				now[0], now[1], now[2], now[3], now[4])
			last = now
		}
	}
}

// Name of the remote (as passed into NewFs).
func (f *Fs) Name() string { return f.name }

// Root of the remote (as passed into NewFs).
func (f *Fs) Root() string { return f.root }

// Features returns the optional features of this Fs.
func (f *Fs) Features() *fs.Features { return f.features }

// String returns a description of the Fs.
func (f *Fs) String() string { return fmt.Sprintf("medialayer::%s:%s", f.name, f.root) }

// UnWrap returns the Fs this Fs wraps.
func (f *Fs) UnWrap() fs.Fs { return f.Fs }

// WrapFs returns the Fs wrapping this Fs.
func (f *Fs) WrapFs() fs.Fs { return f.wrapper }

// SetWrapper sets the Fs wrapping this Fs.
func (f *Fs) SetWrapper(wrapper fs.Fs) { f.wrapper = wrapper }

// key names a file in the header store: the wrapped remote plus the file's
// full path on it, so two overlays on the same folder share records.
func (f *Fs) key(remote string) string {
	return f.Fs.Name() + ":" + path.Join(f.Fs.Root(), remote)
}

// isMedia reports whether remote has one of the configured extensions.
func (f *Fs) isMedia(remote string) bool {
	return f.exts[strings.ToLower(path.Ext(remote))]
}

// forget drops the stored header for remote, after the file was written,
// moved or deleted through this overlay.
func (f *Fs) forget(remote string) {
	if !f.isMedia(remote) {
		return
	}
	if err := f.store.remove(f.key(remote)); err != nil {
		fs.Errorf(f, "failed to drop header for %q: %v", remote, err)
	}
}

func (f *Fs) wrapEntries(entries fs.DirEntries) fs.DirEntries {
	for i, entry := range entries {
		if o, ok := entry.(fs.Object); ok {
			entries[i] = f.wrapObject(o)
		}
	}
	return entries
}

// List the objects and directories in dir into entries.
func (f *Fs) List(ctx context.Context, dir string) (fs.DirEntries, error) {
	return list.WithListP(ctx, dir, f)
}

// ListP lists dir, calling callback for each batch of entries.
func (f *Fs) ListP(ctx context.Context, dir string, callback fs.ListRCallback) error {
	wrapped := func(entries fs.DirEntries) error {
		return callback(f.wrapEntries(entries))
	}
	if listP := f.Fs.Features().ListP; listP != nil {
		return listP(ctx, dir, wrapped)
	}
	entries, err := f.Fs.List(ctx, dir)
	if err != nil {
		return err
	}
	return wrapped(entries)
}

// ListR lists dir recursively. It is only offered when the wrapped remote
// offers it (Features.Mask removes it otherwise).
func (f *Fs) ListR(ctx context.Context, dir string, callback fs.ListRCallback) error {
	return f.Fs.Features().ListR(ctx, dir, func(entries fs.DirEntries) error {
		return callback(f.wrapEntries(entries))
	})
}

// NewObject finds the Object at remote.
func (f *Fs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	o, err := f.Fs.NewObject(ctx, remote)
	if err != nil {
		return nil, err
	}
	return f.wrapObject(o), nil
}

// Put uploads a new object.
func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	f.forget(src.Remote())
	o, err := f.Fs.Put(ctx, in, src, options...)
	if err != nil {
		return nil, err
	}
	return f.wrapObject(o), nil
}

// PutStream uploads an object of unknown size.
func (f *Fs) PutStream(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	do := f.Fs.Features().PutStream
	if do == nil {
		return nil, errors.New("PutStream not supported")
	}
	f.forget(src.Remote())
	o, err := do(ctx, in, src, options...)
	if err != nil {
		return nil, err
	}
	return f.wrapObject(o), nil
}

// Copy src to remote with a server-side copy.
func (f *Fs) Copy(ctx context.Context, src fs.Object, remote string) (fs.Object, error) {
	do := f.Fs.Features().Copy
	srcObj, ok := src.(*Object)
	if do == nil || !ok {
		return nil, fs.ErrorCantCopy
	}
	f.forget(remote)
	o, err := do(ctx, srcObj.Object, remote)
	if err != nil {
		return nil, err
	}
	return f.wrapObject(o), nil
}

// Move src to remote with a server-side move.
func (f *Fs) Move(ctx context.Context, src fs.Object, remote string) (fs.Object, error) {
	do := f.Fs.Features().Move
	srcObj, ok := src.(*Object)
	if do == nil || !ok {
		return nil, fs.ErrorCantMove
	}
	srcObj.f.forget(srcObj.Remote())
	f.forget(remote)
	o, err := do(ctx, srcObj.Object, remote)
	if err != nil {
		return nil, err
	}
	return f.wrapObject(o), nil
}

// DirMove moves a directory with a server-side move. Records under the old
// path are left behind and simply never match again; prime the new path.
func (f *Fs) DirMove(ctx context.Context, src fs.Fs, srcRemote, dstRemote string) error {
	do := f.Fs.Features().DirMove
	srcFs, ok := src.(*Fs)
	if do == nil || !ok {
		return fs.ErrorCantDirMove
	}
	return do(ctx, srcFs.Fs, srcRemote, dstRemote)
}

// Purge deletes a directory and its contents.
func (f *Fs) Purge(ctx context.Context, dir string) error {
	if do := f.Fs.Features().Purge; do != nil {
		return do(ctx, dir)
	}
	return fs.ErrorCantPurge
}

// CleanUp empties the remote's trash.
func (f *Fs) CleanUp(ctx context.Context) error {
	if do := f.Fs.Features().CleanUp; do != nil {
		return do(ctx)
	}
	return errors.New("not supported by underlying remote")
}

// About returns quota information.
func (f *Fs) About(ctx context.Context) (*fs.Usage, error) {
	if do := f.Fs.Features().About; do != nil {
		return do(ctx)
	}
	return nil, errors.New("not supported by underlying remote")
}

// ChangeNotify passes change notifications through from the remote.
func (f *Fs) ChangeNotify(ctx context.Context, notify func(string, fs.EntryType), pollInterval <-chan time.Duration) {
	if do := f.Fs.Features().ChangeNotify; do != nil {
		do(ctx, notify, pollInterval)
	}
}

// DirCacheFlush resets the remote's directory cache.
func (f *Fs) DirCacheFlush() {
	if do := f.Fs.Features().DirCacheFlush; do != nil {
		do()
	}
}

// Shutdown the wrapped remote.
func (f *Fs) Shutdown(ctx context.Context) error {
	f.stopOnce.Do(func() { close(f.stop) })
	if do := f.Fs.Features().Shutdown; do != nil {
		return do(ctx)
	}
	return nil
}

// Object wraps an object of the wrapped remote.
type Object struct {
	fs.Object
	f *Fs
}

func (f *Fs) wrapObject(o fs.Object) *Object { return &Object{Object: o, f: f} }

// Fs returns the overlay this object belongs to.
func (o *Object) Fs() fs.Info { return o.f }

// UnWrap returns the wrapped object.
func (o *Object) UnWrap() fs.Object { return o.Object }

// String returns a description of the object.
func (o *Object) String() string {
	if o == nil {
		return "<nil>"
	}
	return o.Object.String()
}

// ID returns the wrapped object's ID, if it has one.
func (o *Object) ID() string {
	if doer, ok := o.Object.(fs.IDer); ok {
		return doer.ID()
	}
	return ""
}

// MimeType returns the wrapped object's MIME type, if known.
func (o *Object) MimeType(ctx context.Context) string {
	if doer, ok := o.Object.(fs.MimeTyper); ok {
		return doer.MimeType(ctx)
	}
	return ""
}

// Update replaces the object's content and drops its stored header.
func (o *Object) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) error {
	o.f.forget(o.Remote())
	return o.Object.Update(ctx, in, src, options...)
}

// Remove deletes the object and its stored header.
func (o *Object) Remove(ctx context.Context) error {
	o.f.forget(o.Remote())
	return o.Object.Remove(ctx)
}

// freshRecord returns the stored header for this object if it still
// matches the file's size and modification time, or nil.
func (o *Object) freshRecord(ctx context.Context) *headerRecord {
	if !o.f.isMedia(o.Remote()) {
		return nil
	}
	rec := o.f.store.get(o.f.key(o.Remote()))
	if rec == nil {
		return nil
	}
	if rec.Size != o.Size() || !sameModTime(rec.ModTime, o.ModTime(ctx), o.f.Fs.Precision()) {
		o.f.stats.staleRecords.Add(1)
		return nil
	}
	return rec
}

// sameModTime compares modification times within the remote's precision.
func sameModTime(a, b time.Time, precision time.Duration) bool {
	if precision == fs.ModTimeNotSupported {
		return true
	}
	d := a.Sub(b)
	if d < 0 {
		d = -d
	}
	return d <= max(precision, time.Nanosecond)
}

// Open opens the object for reading. A read that starts inside the stored
// header is answered from it, and the cloud is contacted only if the reader
// goes past the header.
func (o *Object) Open(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {
	rec := o.freshRecord(ctx)
	if rec == nil {
		return o.Object.Open(ctx, options...)
	}
	o.f.stats.opens.Add(1)

	size := o.Size()
	fs.FixRangeOption(options, size)
	var offset, limit int64 = 0, -1
	var rest []fs.OpenOption // options other than the range, kept for the remote
	for _, option := range options {
		switch x := option.(type) {
		case *fs.SeekOption:
			offset = x.Offset
		case *fs.RangeOption:
			offset, limit = x.Decode(size)
		default:
			rest = append(rest, option)
		}
	}
	if offset < 0 || offset >= int64(len(rec.Header)) {
		o.f.stats.passthroughOpens.Add(1)
		return o.Object.Open(ctx, options...)
	}
	end := size
	if limit >= 0 {
		end = min(offset+limit, size)
	}

	o.f.stats.localOpens.Add(1)
	fs.Debugf(o, "medialayer: range %d-%d starts in the %d stored header bytes", offset, end, len(rec.Header))
	openAt := func(off, lim int64) (io.ReadCloser, error) {
		opts := append([]fs.OpenOption{&fs.RangeOption{Start: off, End: off + lim - 1}}, rest...)
		return o.Object.Open(ctx, opts...)
	}
	onOpen := func() {
		o.f.stats.cloudFallbacks.Add(1)
		fs.Debugf(o, "medialayer: read went past the stored header, opening the remote")
	}
	return newStitchReader(rec.Header, offset, end, openAt, onOpen), nil
}

// Check the interfaces are satisfied.
var (
	_ fs.Fs              = (*Fs)(nil)
	_ fs.Purger          = (*Fs)(nil)
	_ fs.PutStreamer     = (*Fs)(nil)
	_ fs.Copier          = (*Fs)(nil)
	_ fs.Mover           = (*Fs)(nil)
	_ fs.DirMover        = (*Fs)(nil)
	_ fs.Commander       = (*Fs)(nil)
	_ fs.CleanUpper      = (*Fs)(nil)
	_ fs.UnWrapper       = (*Fs)(nil)
	_ fs.ListRer         = (*Fs)(nil)
	_ fs.ListPer         = (*Fs)(nil)
	_ fs.Abouter         = (*Fs)(nil)
	_ fs.Wrapper         = (*Fs)(nil)
	_ fs.DirCacheFlusher = (*Fs)(nil)
	_ fs.ChangeNotifier  = (*Fs)(nil)
	_ fs.Shutdowner      = (*Fs)(nil)
	_ fs.Object          = (*Object)(nil)
	_ fs.IDer            = (*Object)(nil)
	_ fs.MimeTyper       = (*Object)(nil)
	_ fs.ObjectUnWrapper = (*Object)(nil)
)
