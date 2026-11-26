package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rclone/rclone/fs"
)

// streamServer serves tracks to the player over HTTP on the loopback
// interface. Each HTTP request becomes exactly one ranged request to the
// cloud, streamed straight through: the player asks for the rest of the
// file once, and the cloud sends it as it plays.
//
// URLs are http://127.0.0.1:<port>/<token>/<file id>.<format>. The random
// token and the Host check keep other local processes and web pages (DNS
// rebinding) from reading the library.
type streamServer struct {
	c     *Core
	ln    net.Listener
	srv   *http.Server
	token string
	host  string // "127.0.0.1:<port>"

	mu     sync.Mutex
	objs   map[string]fs.Object   // by file id: avoids a lookup per request
	heads  map[string]*headStream // parked streams by file id
	closed bool

	// prepares bounds background lookups; preparing holds the ids in flight.
	prepares  chan struct{}
	preparing map[string]bool
}

// maxCachedObjects bounds streamServer.objs.
const maxCachedObjects = 256

func startStreamServer(c *Core) (*streamServer, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	tok := make([]byte, 16)
	if _, err := rand.Read(tok); err != nil {
		_ = ln.Close()
		return nil, err
	}
	s := &streamServer{
		c:         c,
		ln:        ln,
		token:     hex.EncodeToString(tok),
		host:      ln.Addr().String(),
		objs:      map[string]fs.Object{},
		heads:     map[string]*headStream{},
		prepares:  make(chan struct{}, 2),
		preparing: map[string]bool{},
	}
	s.srv = &http.Server{Handler: s, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = s.srv.Serve(ln) }()
	return s, nil
}

func (s *streamServer) base() string {
	return "http://" + s.host + "/" + s.token + "/"
}

func (s *streamServer) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = s.srv.Shutdown(ctx)
	s.mu.Lock()
	s.closed = true
	for id, p := range s.heads {
		p.timer.Stop()
		p.close()
		delete(s.heads, id)
	}
	s.mu.Unlock()
}

// prepare looks up a file's cloud object in the background, so that
// playing it doesn't wait for the lookup.
// At most two run at once; when both are busy (the user is scrolling
// through the list) the request is dropped.
func (s *streamServer) prepare(id string) {
	s.mu.Lock()
	if s.closed || s.objs[id] != nil || s.preparing[id] {
		s.mu.Unlock()
		return
	}
	select {
	case s.prepares <- struct{}{}:
	default:
		s.mu.Unlock()
		return
	}
	s.preparing[id] = true
	s.mu.Unlock()
	go func() {
		defer func() {
			s.mu.Lock()
			delete(s.preparing, id)
			s.mu.Unlock()
			<-s.prepares
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, _, err := s.object(ctx, id); err != nil {
			fs.Debugf(nil, "prepare %s: %v", id, err)
		}
	}()
}

// contentTypes maps index formats to MIME types.
var contentTypes = map[string]string{"flac": "audio/flac", "mp3": "audio/mpeg", "m4a": "audio/mp4"}

func (s *streamServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Host != s.host {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	tok, name, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if !ok || tok != s.token {
		http.NotFound(w, r)
		return
	}
	id, _, _ := strings.Cut(name, ".")
	o, format, err := s.object(r.Context(), id)
	if err != nil {
		fs.Errorf(nil, "stream %s: %v", id, err)
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	size := o.Size()

	start, end, partial, err := parseRange(r.Header.Get("Range"), size)
	if err != nil {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
		http.Error(w, err.Error(), http.StatusRequestedRangeNotSatisfiable)
		return
	}
	h := w.Header()
	h.Set("Accept-Ranges", "bytes")
	h.Set("Content-Type", contentTypes[format])
	h.Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	h.Set("Last-Modified", o.ModTime(r.Context()).UTC().Format(http.TimeFormat))
	status := http.StatusOK
	if partial {
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
		status = http.StatusPartialContent
	}
	if r.Method == http.MethodHead || size == 0 {
		w.WriteHeader(status)
		return
	}

	// The player first probes the start of the file (AVPlayer asks for
	// bytes 0-1) and then asks for all of it from 0. A small request at the
	// start opens a stream to the end instead and parks it, so the second
	// request replays the probed bytes and carries on with the same cloud
	// request: one round trip instead of two before the first audio.
	if head := s.peekHead(id, o, start, end); head != nil {
		w.WriteHeader(status) // a repeated probe: leave the stream parked
		_, _ = w.Write(head)
		return
	}
	if p := s.takeHead(id, o, start); p != nil {
		defer p.close()
		stop := context.AfterFunc(r.Context(), p.cancel) // the player gave up
		defer stop()
		w.WriteHeader(status)
		s.copyFromHead(w, o, p, start, end)
		return
	}
	if start == 0 && end < headProbeMax && end < size-1 {
		p, err := openHead(r.Context(), o, end+1)
		if err != nil && r.Context().Err() != nil {
			return // the player cancelled the probe; nothing went wrong
		}
		if err != nil {
			s.forget(id) // the file may have changed or moved
			fs.Errorf(o, "stream: %v", err)
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write(p.head)
		s.parkHead(id, p)
		return
	}

	rc, err := o.Open(r.Context(), &fs.RangeOption{Start: start, End: end})
	if err != nil && r.Context().Err() != nil {
		return // the player cancelled the request
	}
	if err != nil {
		s.forget(id) // the file may have changed or moved
		fs.Errorf(o, "stream: %v", err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = rc.Close() }()
	w.WriteHeader(status)
	if _, err := io.CopyN(w, rc, end-start+1); err != nil && !errors.Is(err, context.Canceled) {
		fs.Debugf(o, "stream ended early: %v", err) // usually the player seeking or stopping
	}
}

const (
	// headProbeMax is the largest request at the start of a file that
	// opens a parked stream.
	headProbeMax = 64 << 10
	// headTTL is how long a parked stream waits for the follow-up request.
	headTTL = 5 * time.Second
	// maxHeads bounds the parked streams.
	maxHeads = 4
)

// headStream is an open cloud stream from byte 0 to the end of a file, of
// which the first len(head) bytes were already read.
type headStream struct {
	head    []byte
	rc      io.ReadCloser
	cancel  context.CancelFunc
	version string // size and modification time it was opened on
	timer   *time.Timer
}

func (p *headStream) close() {
	p.cancel()
	_ = p.rc.Close()
}

func objectVersion(o fs.Object) string {
	return fmt.Sprintf("%d/%d", o.Size(), o.ModTime(context.Background()).UnixNano())
}

// openHead opens o from the start to the end and reads the first n bytes.
// The stream outlives the HTTP request that opened it, so it has its own
// context, tied to the request's only until the first bytes arrive.
func openHead(reqCtx context.Context, o fs.Object, n int64) (*headStream, error) {
	ctx, cancel := context.WithCancel(context.Background())
	stop := context.AfterFunc(reqCtx, cancel)
	defer stop()
	rc, err := o.Open(ctx, &fs.RangeOption{Start: 0, End: o.Size() - 1})
	if err != nil {
		cancel()
		return nil, err
	}
	p := &headStream{head: make([]byte, n), rc: rc, cancel: cancel, version: objectVersion(o)}
	if _, err := io.ReadFull(rc, p.head); err != nil {
		p.close()
		return nil, err
	}
	if !stop() { // the request ended while the bytes arrived
		p.close()
		return nil, context.Canceled
	}
	return p, nil
}

// peekHead returns the parked bytes for [start, end] when they are all
// already read, leaving the stream parked for the request that follows.
func (s *streamServer) peekHead(id string, o fs.Object, start, end int64) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.heads[id]
	if p == nil || end >= int64(len(p.head)) || p.version != objectVersion(o) {
		return nil
	}
	return append([]byte(nil), p.head[start:end+1]...)
}

func (s *streamServer) parkHead(id string, p *headStream) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		p.close()
		return
	}
	if old := s.heads[id]; old != nil {
		old.timer.Stop()
		old.close()
	}
	if len(s.heads) >= maxHeads {
		for k, h := range s.heads { // any one; they live for seconds
			h.timer.Stop()
			h.close()
			delete(s.heads, k)
			break
		}
	}
	s.heads[id] = p
	p.timer = time.AfterFunc(headTTL, func() {
		s.mu.Lock()
		if s.heads[id] == p {
			delete(s.heads, id)
			p.close()
		}
		s.mu.Unlock()
	})
}

// takeHead removes and returns the parked stream for id if it can serve
// a request starting at start.
func (s *streamServer) takeHead(id string, o fs.Object, start int64) *headStream {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.heads[id]
	if p == nil || start > int64(len(p.head)) || p.version != objectVersion(o) {
		return nil
	}
	delete(s.heads, id)
	p.timer.Stop()
	return p
}

// copyFromHead serves [start, end] from a parked stream: the bytes it
// already read, then the stream itself.
func (s *streamServer) copyFromHead(w io.Writer, o fs.Object, p *headStream, start, end int64) {
	headEnd := min(int64(len(p.head)), end+1)
	if _, err := w.Write(p.head[start:headEnd]); err != nil {
		return
	}
	if rest := end + 1 - headEnd; rest > 0 {
		if _, err := io.CopyN(w, p.rc, rest); err != nil && !errors.Is(err, context.Canceled) {
			fs.Debugf(o, "stream ended early: %v", err)
		}
	}
}

// object finds the cloud object for a file id.
func (s *streamServer) object(ctx context.Context, id string) (fs.Object, string, error) {
	db := s.c.currentDB()
	if db == nil {
		return nil, "", errors.New("no library")
	}
	loc, err := db.Locate(id, replica)
	if err != nil {
		return nil, "", fmt.Errorf("file %s not in the index: %w", id, err)
	}
	s.mu.Lock()
	o := s.objs[id]
	s.mu.Unlock()
	if o != nil && o.Size() == loc.Size && o.Remote() == loc.Path {
		return o, loc.Format, nil
	}
	f, err := s.c.libraryFs(ctx)
	if err != nil {
		return nil, "", err
	}
	o, err = f.NewObject(ctx, loc.Path)
	if err != nil {
		return nil, "", err
	}
	s.mu.Lock()
	if len(s.objs) >= maxCachedObjects {
		clear(s.objs)
	}
	s.objs[id] = o
	s.mu.Unlock()
	return o, loc.Format, nil
}

// forgetAll drops the cached objects, as when the primary changes.
func (s *streamServer) forgetAll() {
	s.mu.Lock()
	clear(s.objs)
	s.mu.Unlock()
}

func (s *streamServer) forget(id string) {
	s.mu.Lock()
	delete(s.objs, id)
	s.mu.Unlock()
}

// parseRange parses a single-range Range header against a file of size
// bytes. With no header it returns the whole file and partial = false.
// Multiple ranges are answered with the whole file, which RFC 9110
// allows.
func parseRange(header string, size int64) (start, end int64, partial bool, err error) {
	whole := func() (int64, int64, bool, error) { return 0, size - 1, false, nil }
	spec, ok := strings.CutPrefix(header, "bytes=")
	if header == "" || !ok || strings.Contains(spec, ",") {
		return whole()
	}
	first, last, ok := strings.Cut(strings.TrimSpace(spec), "-")
	if !ok {
		return whole()
	}
	switch {
	case first == "": // suffix: the last N bytes
		n, perr := strconv.ParseInt(last, 10, 64)
		if perr != nil || n <= 0 {
			return 0, 0, false, errors.New("bad range")
		}
		start, end = max(size-n, 0), size-1
	default:
		start, err = strconv.ParseInt(first, 10, 64)
		if err != nil || start < 0 {
			return 0, 0, false, errors.New("bad range")
		}
		end = size - 1
		if last != "" {
			end, err = strconv.ParseInt(last, 10, 64)
			if err != nil || end < start {
				return whole() // RFC 9110: ignore an invalid range
			}
			end = min(end, size-1)
		}
	}
	if start >= size {
		return 0, 0, false, errors.New("range starts past the end of the file")
	}
	return start, end, true, nil
}
