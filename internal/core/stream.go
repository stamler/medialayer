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

	mu   sync.Mutex
	objs map[string]fs.Object // by file id: avoids a lookup per request
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
		c:     c,
		ln:    ln,
		token: hex.EncodeToString(tok),
		host:  ln.Addr().String(),
		objs:  map[string]fs.Object{},
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
}

// contentTypes maps index formats to MIME types.
var contentTypes = map[string]string{"flac": "audio/flac", "mp3": "audio/mpeg"}

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

	rc, err := o.Open(r.Context(), &fs.RangeOption{Start: start, End: end})
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
				return 0, 0, false, errors.New("bad range")
			}
			end = min(end, size-1)
		}
	}
	if start >= size {
		return 0, 0, false, errors.New("range starts past the end of the file")
	}
	return start, end, true, nil
}
