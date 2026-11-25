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
	"github.com/rclone/rclone/fs/hash"
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
			Name:     "store_to_block",
			Default:  fs.SizeSuffix(1024 * 1024),
			Advanced: true,
			Help: `Keep each file's bytes up to the next multiple of this size past its metadata.

Apps read files through a buffer the size of the block size the file
system reports, and macOS reports 1 MiB for NFS mounts, so a tag scan
through nfsmount reads the whole first MiB of every file even though the
tags end well before. Keeping those bytes lets the scan stay local. 0
keeps only the metadata.`,
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
	StoreTo    fs.SizeSuffix   `config:"store_to_block"`
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
	hashType hash.Type // content hash stored with each record, or hash.None
	stats    *stats
	streams  *streamPool // remote streams kept open between NFS reads
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
	reusedStreams    atomic.Int64 // remote reads that continued a parked stream
}

// Parked streams are kept this long, and at most this many at once. A
// sequential reader through nfsmount comes back within milliseconds; the
// limit bounds the idle connections held open to the cloud.
const (
	streamParkTime = 10 * time.Second
	maxParked      = 32
)

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
	if opt.StoreTo < 0 {
		return nil, errors.New("store_to_block must not be negative")
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
		Fs:      baseFs,
		name:    name,
		root:    root,
		opt:     opt,
		store:   store,
		exts:    map[string]bool{},
		stats:   &stats{},
		streams: newStreamPool(streamParkTime, maxParked),
		stop:    make(chan struct{}),
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

	// Mask ANDs these with the wrapped remote's, so setting them here passes
	// the wrapped remote's values through. Only object-level features the
	// overlay forwards are listed.
	f.features = (&fs.Features{
		CaseInsensitive:         true,
		DuplicateFiles:          true,
		CanHaveEmptyDirectories: true,
		BucketBased:             true,
		BucketBasedRootOK:       true,
		ReadMimeType:            true,
		WriteMimeType:           true,
		ReadMetadata:            true,
		WriteMetadata:           true,
		UserMetadata:            true,
		SetTier:                 true,
		GetTier:                 true,
		SlowModTime:             true,
		SlowHash:                true,
		PartialUploads:          true,
		NoMultiThreading:        true,
		DoubleSlash:             true,
	}).Fill(ctx, f).Mask(ctx, f.Fs).WrapsFs(f, f.Fs)
	f.features.ListP = f.ListP

	// A content hash makes the freshness check catch a retag that keeps the
	// size and modification time. Use one only where the remote has it
	// without reading the file (Drive, S3 ...), not where it is computed
	// (local disk).
	// Wrappers (chunker, hasher ...) do not always pass SlowHash on, so the
	// innermost remote is checked too.
	inner := fs.UnWrapFs(f.Fs)
	if !f.Fs.Features().SlowHash && !inner.Features().SlowHash && !inner.Features().IsLocal {
		f.hashType = f.Fs.Hashes().GetOne()
	}

	cache.PinUntilFinalized(f.Fs, f)
	go f.logStats(30 * time.Second)
	return f, err
}

// logStats logs the read statistics at INFO level (-v) whenever they
// change, so a mount's behaviour can be followed without the rc API.
func (f *Fs) logStats(every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	var last [6]int64
	for {
		select {
		case <-f.stop:
			return
		case <-ticker.C:
		}
		now := [6]int64{
			f.stats.opens.Load(), f.stats.localOpens.Load(), f.stats.passthroughOpens.Load(),
			f.stats.cloudFallbacks.Load(), f.stats.staleRecords.Load(), f.stats.reusedStreams.Load(),
		}
		if now != last {
			fs.Infof(f, "medialayer stats: %d opens with a stored header, %d served locally, %d passed through, %d read past the header, %d stale records, %d cloud reads continued an open stream",
				now[0], now[1], now[2], now[3], now[4], now[5])
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
	f.streams.drop(f.key(remote))
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
	defer f.forget(src.Remote()) // and again after, in case a prime ran meanwhile
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
	defer f.forget(src.Remote()) // and again after, in case a prime ran meanwhile
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
	defer f.forget(remote) // and again after, in case a prime ran meanwhile
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
	defer srcObj.f.forget(srcObj.Remote()) // and again after, in case a prime ran meanwhile
	f.forget(remote)
	defer f.forget(remote) // and again after, in case a prime ran meanwhile
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
	f.stopOnce.Do(func() {
		close(f.stop)
		f.streams.close()
	})
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

// Metadata returns the wrapped object's metadata.
func (o *Object) Metadata(ctx context.Context) (fs.Metadata, error) {
	return fs.GetMetadata(ctx, o.Object)
}

// SetMetadata sets the wrapped object's metadata and, once that worked,
// drops its stored header.
func (o *Object) SetMetadata(ctx context.Context, metadata fs.Metadata) error {
	do, ok := o.Object.(fs.SetMetadataer)
	if !ok {
		return fs.ErrorNotImplemented
	}
	if err := do.SetMetadata(ctx, metadata); err != nil {
		return err
	}
	o.f.forget(o.Remote())
	return nil
}

// GetTier returns the wrapped object's storage tier.
func (o *Object) GetTier() string {
	if do, ok := o.Object.(fs.GetTierer); ok {
		return do.GetTier()
	}
	return ""
}

// SetTier sets the wrapped object's storage tier.
func (o *Object) SetTier(tier string) error {
	if do, ok := o.Object.(fs.SetTierer); ok {
		return do.SetTier(tier)
	}
	return fs.ErrorNotImplemented
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
	defer o.f.forget(o.Remote()) // and again after, in case a prime ran meanwhile
	return o.Object.Update(ctx, in, src, options...)
}

// Remove deletes the object and its stored header.
func (o *Object) Remove(ctx context.Context) error {
	o.f.forget(o.Remote())
	defer o.f.forget(o.Remote()) // and again after, in case a prime ran meanwhile
	return o.Object.Remove(ctx)
}

// freshRecord returns the stored header for this object if it still
// describes the file, or nil.
func (o *Object) freshRecord(ctx context.Context) *headerRecord {
	if !o.f.isMedia(o.Remote()) {
		return nil
	}
	rec := o.f.store.get(o.f.key(o.Remote()))
	if rec == nil {
		return nil
	}
	if !o.f.matches(ctx, rec, o.Object) {
		o.f.stats.staleRecords.Add(1)
		return nil
	}
	return rec
}

// canTrackChanges reports whether this remote gives a way to tell that a
// file changed: modification times, or a content hash.
func (f *Fs) canTrackChanges() bool {
	return f.Fs.Precision() != fs.ModTimeNotSupported || f.hashType != hash.None
}

// matches reports whether rec was made from o's current content. The size
// must match, the modification time must match exactly where the remote has
// them (the record came from the same remote, so an unchanged file reports
// the same time), and the content hash must match where one was stored. A
// record is never trusted on size alone: retaggers reuse FLAC padding, so a
// retagged file usually keeps its size.
func (f *Fs) matches(ctx context.Context, rec *headerRecord, o fs.Object) bool {
	if rec.Size != o.Size() {
		return false
	}
	checked := false
	if f.Fs.Precision() != fs.ModTimeNotSupported {
		if !rec.ModTime.Equal(o.ModTime(ctx)) {
			return false
		}
		checked = true
	}
	if rec.HashType != hash.None {
		sum, err := o.Hash(ctx, rec.HashType)
		if err != nil || sum == "" || sum != rec.Hash {
			return false
		}
		checked = true
	}
	return checked
}

// Open opens the object for reading. A read that starts inside the stored
// header is answered from it, and the cloud is contacted only if the reader
// goes past the header. Ranged reads that do reach the cloud continue a
// parked stream where one stops at the right offset (see streamPool).
func (o *Object) Open(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {
	size := o.Size()
	// FixRangeOption replaces entries of the slice it is given, so fix a
	// copy and keep the caller's options for a pass-through open.
	fixed := append([]fs.OpenOption(nil), options...)
	fs.FixRangeOption(fixed, size)
	var offset, limit int64 = 0, -1
	ranged := false
	var rest []fs.OpenOption // options other than the range, kept for the remote
	for _, option := range fixed {
		switch x := option.(type) {
		case *fs.SeekOption:
			offset, ranged = x.Offset, true
		case *fs.RangeOption:
			offset, limit = x.Decode(size)
			ranged = true
		default:
			rest = append(rest, option)
		}
	}
	end := size
	if limit >= 0 {
		end = min(offset+limit, size)
	}
	// Unknown sizes and ranges outside the file go to the remote as asked,
	// so it reports them as it normally would.
	usable := size >= 0 && offset >= 0 && offset <= end

	rec := o.freshRecord(ctx)
	if rec == nil {
		if !ranged || !usable || offset == end {
			return o.Object.Open(ctx, options...)
		}
		return o.openRemote(ctx, offset, end, rest)
	}
	o.f.stats.opens.Add(1)
	if !usable {
		o.f.stats.passthroughOpens.Add(1)
		return o.Object.Open(ctx, options...)
	}
	if offset >= int64(len(rec.Header)) {
		o.f.stats.passthroughOpens.Add(1)
		return o.openRemote(ctx, offset, end, rest)
	}

	// The header bytes come from the record and any later bytes from a
	// remote open made when the reader gets there. If the file is replaced
	// between the freshness check above and that open, the reader gets the
	// old header followed by the new file's bytes. rclone's VFS hands out
	// an object it listed earlier (up to --dir-cache-time), so this window
	// exists; the cloud repository's index will close it by pinning a
	// version (revision or etag) where the remote has one.
	o.f.stats.localOpens.Add(1)
	fs.Debugf(o, "medialayer: range %d-%d starts in the %d stored header bytes", offset, end, len(rec.Header))
	openAt := func(off, lim int64) (io.ReadCloser, error) {
		return o.openRemote(ctx, off, off+lim, rest)
	}
	onOpen := func() {
		o.f.stats.cloudFallbacks.Add(1)
		fs.Debugf(o, "medialayer: read went past the stored header, opening the remote")
	}
	return newStitchReader(rec.Header, offset, end, openAt, onOpen), nil
}

// reusableOptions reports whether a stream opened with these options (on
// top of its range) returns plain file bytes, so it can be parked and
// continued. The VFS adds an empty hash request, which changes nothing.
// Anything else (headers, options of other backends) might change what the
// remote returns, so such streams are neither reused nor parked.
func reusableOptions(options []fs.OpenOption) bool {
	for _, option := range options {
		switch x := option.(type) {
		case fs.NullOption:
		case *fs.HashesOption:
			if x.Hashes.Count() != 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// openRemote reads bytes [offset, end) of the object from the wrapped
// remote. It continues a parked stream that stops at offset if there is
// one, and parks its own stream when closed with bytes left.
func (o *Object) openRemote(ctx context.Context, offset, end int64, rest []fs.OpenOption) (io.ReadCloser, error) {
	open := func(off, lim int64) (io.ReadCloser, error) {
		opts := append([]fs.OpenOption{&fs.RangeOption{Start: off, End: off + lim - 1}}, rest...)
		return o.Object.Open(ctx, opts...)
	}
	r := &pooledReader{
		pool: o.f.streams,
		key:  o.f.key(o.Remote()),
		// A file replaced by one of the same size is told apart by its
		// modification time.
		version: fmt.Sprintf("%d/%d", o.Size(), o.ModTime(ctx).UnixNano()),
		open:    open,
		pos:     offset,
		limit:   end,
		noPark:  !reusableOptions(rest),
	}
	if offset >= end {
		return io.NopCloser(strings.NewReader("")), nil
	}
	if !r.noPark {
		if s := r.pool.take(r.key, r.version, offset); s != nil {
			o.f.stats.reusedStreams.Add(1)
			fs.Debugf(o, "medialayer: continuing an open stream at %d", offset)
			r.rc, r.streamEnd, r.reused = s.rc, s.end, true
			return r, nil
		}
	}
	rc, err := open(offset, end-offset)
	if err != nil {
		return nil, err
	}
	r.rc, r.streamEnd = rc, end
	return r, nil
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
	_ fs.Metadataer      = (*Object)(nil)
	_ fs.SetMetadataer   = (*Object)(nil)
	_ fs.GetTierer       = (*Object)(nil)
	_ fs.SetTierer       = (*Object)(nil)
	_ fs.ObjectUnWrapper = (*Object)(nil)
)
