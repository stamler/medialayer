package core

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/rclone/rclone/fs"

	"github.com/stamler/medialayer/internal/media"
)

// The stream server also serves FLAC decoded, as WAVE: at
// <file id>.wav for a file of format "flac" or "oga". Apple's players
// can't play FLAC in Ogg or 32-bit FLAC, so the app asks for those this
// way. The WAVE file is never made whole: each request decodes just the
// samples its byte range covers, from the frame that holds the first,
// found by bisecting the file (see media.FLACSource.Seek).

// wavHeaderLen is the length of the WAVE header: RIFF, a
// WAVE_FORMAT_EXTENSIBLE fmt chunk and the data chunk's header.
const wavHeaderLen = 68

// maxPCMSources bounds streamServer.pcm.
const maxPCMSources = 16

// pcmSource is a FLAC file's decoding state that outlives one request.
type pcmSource struct {
	version string // objectVersion it was read at
	src     *media.FLACSource
}

// wavLayout is the WAVE file a FLAC stream becomes.
type wavLayout struct {
	width      int  // bytes a sample
	shift      uint // bits each sample is shifted up
	frameBytes int64
	header     []byte
	size       int64
}

func newWAVLayout(src *media.FLACSource) (wavLayout, error) {
	info := src.Info
	l := wavLayout{}
	l.width, l.shift = src.PCMLayout()
	l.frameBytes = int64(l.width) * int64(info.Channels)
	data := int64(src.Total) * l.frameBytes
	if data > 1<<32-1-wavHeaderLen {
		return l, fmt.Errorf("%d samples are too many for a WAVE file", src.Total)
	}
	l.size = wavHeaderLen + data
	h := make([]byte, 0, wavHeaderLen)
	h = append(h, "RIFF"...)
	h = binary.LittleEndian.AppendUint32(h, uint32(l.size-8))
	h = append(h, "WAVEfmt "...)
	h = binary.LittleEndian.AppendUint32(h, 40)
	h = binary.LittleEndian.AppendUint16(h, 0xfffe) // WAVE_FORMAT_EXTENSIBLE
	h = binary.LittleEndian.AppendUint16(h, uint16(info.Channels))
	h = binary.LittleEndian.AppendUint32(h, info.SampleRate)
	h = binary.LittleEndian.AppendUint32(h, info.SampleRate*uint32(l.frameBytes))
	h = binary.LittleEndian.AppendUint16(h, uint16(l.frameBytes))
	h = binary.LittleEndian.AppendUint16(h, uint16(l.width*8))
	h = binary.LittleEndian.AppendUint16(h, 22)
	h = binary.LittleEndian.AppendUint16(h, uint16(l.width*8)) // valid bits: shifted to fill
	// FLAC's channel orders, which are WAVE's.
	masks := [...]uint32{0x4, 0x3, 0x7, 0x33, 0x37, 0x3f, 0x70f, 0x63f}
	h = binary.LittleEndian.AppendUint32(h, masks[info.Channels-1])
	h = append(h, 1, 0, 0, 0, 0, 0, 0x10, 0, 0x80, 0, 0, 0xaa, 0, 0x38, 0x9b, 0x71) // KSDATAFORMAT_SUBTYPE_PCM
	h = append(h, "data"...)
	h = binary.LittleEndian.AppendUint32(h, uint32(data))
	l.header = h
	return l, nil
}

// pcmSourceFor reads (or recalls) the FLAC header of o.
func (s *streamServer) pcmSourceFor(ctx context.Context, id string, o fs.Object, format string) (*media.FLACSource, error) {
	v := objectVersion(o)
	s.mu.Lock()
	p := s.pcm[id]
	s.mu.Unlock()
	if p != nil && p.version == v {
		return p.src, nil
	}
	src, err := media.OpenFLACSource(objectFetch(ctx, o), o.Size(), maxHeader, format == "oga")
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if len(s.pcm) >= maxPCMSources {
		clear(s.pcm)
	}
	s.pcm[id] = &pcmSource{version: v, src: src}
	s.mu.Unlock()
	return src, nil
}

// servePCM answers a request for <id>.wav.
func (s *streamServer) servePCM(w http.ResponseWriter, r *http.Request, id string, o fs.Object, format string) {
	ctx := r.Context()
	if format != "flac" && format != "oga" {
		http.Error(w, "only FLAC is served decoded", http.StatusNotFound)
		return
	}
	src, err := s.pcmSourceFor(ctx, id, o, format)
	if err == nil {
		var l wavLayout
		if l, err = newWAVLayout(src); err == nil {
			s.writePCM(w, r, o, src, l)
			return
		}
	}
	if ctx.Err() != nil {
		return
	}
	s.forget(id)
	fs.Errorf(o, "stream decoded: %v", err)
	http.Error(w, err.Error(), http.StatusBadGateway)
}

func (s *streamServer) writePCM(w http.ResponseWriter, r *http.Request, o fs.Object, src *media.FLACSource, l wavLayout) {
	ctx := r.Context()
	start, end, partial, err := parseRange(r.Header.Get("Range"), l.size)
	if err != nil {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", l.size))
		http.Error(w, err.Error(), http.StatusRequestedRangeNotSatisfiable)
		return
	}
	h := w.Header()
	h.Set("Accept-Ranges", "bytes")
	h.Set("Content-Type", "audio/wav")
	h.Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	h.Set("Last-Modified", o.ModTime(ctx).UTC().Format(http.TimeFormat))
	status := http.StatusOK
	if partial {
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, l.size))
		status = http.StatusPartialContent
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(status)
		return
	}

	// Find the first frame before answering, so a failure is an error.
	var frames *media.FLACFrames
	from := max(start, wavHeaderLen) - wavHeaderLen
	sample := uint64(from / l.frameBytes)
	if end >= wavHeaderLen {
		open := func(off, n int64) (io.ReadCloser, error) {
			rc, err := o.Open(ctx, &fs.RangeOption{Start: off, End: off + n - 1})
			if err != nil {
				return nil, err
			}
			return struct {
				io.Reader
				io.Closer
			}{ctxReader{ctx, io.LimitReader(rc, n)}, rc}, nil
		}
		if frames, err = src.Seek(open, sample); err != nil {
			if ctx.Err() == nil {
				fs.Errorf(o, "stream decoded: seek to sample %d: %v", sample, err)
				http.Error(w, err.Error(), http.StatusBadGateway)
			}
			return
		}
		defer func() { _ = frames.Close() }()
	}
	w.WriteHeader(status)
	if start < wavHeaderLen {
		if _, err := w.Write(l.header[start:min(end+1, wavHeaderLen)]); err != nil {
			return
		}
	}
	if frames == nil {
		return
	}
	left := end + 1 - max(start, wavHeaderLen) // bytes still to send
	skip := from % l.frameBytes                // of the first sample
	var buf []byte
	for left > 0 {
		first, n, err := frames.Next()
		if err != nil {
			if !errors.Is(err, io.EOF) && ctx.Err() == nil {
				fs.Errorf(o, "stream decoded: %v", err)
			}
			break
		}
		if first+uint64(n) > src.Total { // past what the header promised
			n = int(max(src.Total, first) - first)
		}
		if first+uint64(n) <= sample {
			continue
		}
		i := 0
		if first < sample {
			i = int(sample - first)
		}
		buf = media.AppendPCM(buf[:0], frames.Samples(), i, n, l.width, l.shift)
		out := buf[min(skip, int64(len(buf))):]
		skip = 0
		out = out[:min(int64(len(out)), left)]
		if _, err := w.Write(out); err != nil {
			return // the player stopped or seeked
		}
		left -= int64(len(out))
		sample = first + uint64(n)
	}
	// A stream that ends early (a damaged file) ends in silence, so the
	// length matches what the header said.
	zeros := make([]byte, 64<<10)
	for left > 0 {
		n := min(left, int64(len(zeros)))
		if _, err := w.Write(zeros[:n]); err != nil {
			return
		}
		left -= n
	}
}
