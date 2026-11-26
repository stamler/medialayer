// Package media reads metadata from audio files: FLAC (STREAMINFO, Vorbis
// comments, pictures) and MP3 (ID3v2), from the bytes at the start of a file.
package media

import (
	"bytes"
	"errors"
	"fmt"
	"io"
)

// FetchFunc reads length bytes of a file starting at offset. It may return
// fewer bytes only at the end of the file.
type FetchFunc func(offset, length int64) ([]byte, error)

// ErrNotFLAC is returned when a file does not start with a FLAC stream.
var ErrNotFLAC = errors.New("not a FLAC stream")

const (
	// initialFetch is the size of the first read made when finding where a
	// FLAC file's audio starts. Most metadata chains without large pictures
	// fit in it.
	initialFetch = 64 * 1024
	// extraFetch is added to each follow-up read so that the next block
	// header usually arrives with the bytes we already need.
	extraFetch = 64 * 1024
	// maxBlocks bounds the metadata chain walk against corrupt files.
	maxBlocks = 1024
)

// FLACHeader reads the start of a FLAC file through fetch and returns the
// bytes that precede the first audio frame: any ID3v2 tags, the
// "fLaC" marker and every metadata block (STREAMINFO, VORBIS_COMMENT,
// PICTURE, SEEKTABLE, CUESHEET, PADDING ...).
//
// If those bytes would exceed maxLen, the first maxLen bytes are returned
// with complete set to false. A truncated header is still useful: the bytes
// it holds are correct, only the remainder has to come from the cloud.
//
// size is the length of the whole file; reads never go past it.
func FLACHeader(fetch FetchFunc, size, maxLen int64) (header []byte, complete bool, err error) {
	if size < 4 {
		return nil, false, ErrNotFLAC
	}
	// A fetch that returns fewer bytes than asked for, inside the file, is
	// a failed read, not a sign the file isn't FLAC.
	read := func(offset, length int64) ([]byte, error) {
		got, err := fetch(offset, length)
		if err == nil && int64(len(got)) < length {
			err = fmt.Errorf("read %d of %d bytes at offset %d: %w", len(got), length, offset, io.ErrUnexpectedEOF)
		}
		return got, err
	}

	buf, err := read(0, min(initialFetch, size))
	if err != nil {
		return nil, false, err
	}

	// ensure makes sure buf holds bytes [0, end), reading more if needed.
	// Reads stop at maxLen; the caller checks whether it got enough.
	ensure := func(end int64) error {
		end = min(end, size, maxLen)
		if int64(len(buf)) >= end {
			return nil
		}
		want := min(end+extraFetch, size, maxLen) - int64(len(buf))
		more, err := read(int64(len(buf)), want)
		if err != nil {
			return err
		}
		buf = append(buf, more...)
		return nil
	}

	var pos int64

	// Some taggers put ID3v2 tags before the FLAC marker. Skip them.
	for range maxBlocks {
		if err := ensure(pos + 10); err != nil {
			return nil, false, err
		}
		if int64(len(buf)) < pos+10 || !bytes.Equal(buf[pos:pos+3], []byte("ID3")) {
			break
		}
		h := buf[pos : pos+10]
		next := pos + 10 + int64(syncsafe(h[6:10]))
		if h[5]&0x10 != 0 { // footer present
			next += 10
		}
		pos = next
	}

	if err := ensure(pos + 4); err != nil {
		return nil, false, err
	}
	if int64(len(buf)) < pos+4 || !bytes.Equal(buf[pos:pos+4], []byte("fLaC")) {
		if pos+4 > maxLen {
			return nil, false, fmt.Errorf("ID3v2 tag larger than the header limit: %w", ErrNotFLAC)
		}
		return nil, false, ErrNotFLAC
	}
	pos += 4

	for range maxBlocks {
		if err := ensure(pos + 4); err != nil {
			return nil, false, err
		}
		if int64(len(buf)) < pos+4 {
			// Either the file ends inside the metadata or we hit maxLen.
			if pos+4 > maxLen {
				return buf[:min(int64(len(buf)), maxLen)], false, nil
			}
			return nil, false, fmt.Errorf("file ends inside FLAC metadata: %w", ErrNotFLAC)
		}
		last := buf[pos]&0x80 != 0
		blockType := buf[pos] & 0x7f
		length := int64(buf[pos+1])<<16 | int64(buf[pos+2])<<8 | int64(buf[pos+3])
		if blockType == 127 {
			return nil, false, fmt.Errorf("invalid FLAC metadata block type: %w", ErrNotFLAC)
		}
		pos += 4 + length
		if pos > size {
			return nil, false, fmt.Errorf("FLAC metadata block runs past end of file: %w", ErrNotFLAC)
		}
		if last {
			if pos > maxLen {
				if err := ensure(maxLen); err != nil {
					return nil, false, err
				}
				return buf[:min(int64(len(buf)), maxLen)], false, nil
			}
			if err := ensure(pos); err != nil {
				return nil, false, err
			}
			if int64(len(buf)) < pos {
				return nil, false, fmt.Errorf("short read of FLAC metadata: %w", ErrNotFLAC)
			}
			return buf[:pos], true, nil
		}
	}
	return nil, false, fmt.Errorf("more than %d FLAC metadata blocks: %w", maxBlocks, ErrNotFLAC)
}
