package medialayer

import (
	"bytes"
	"errors"
	"fmt"
)

// fetchFunc reads length bytes of a file starting at offset. It may return
// fewer bytes only at the end of the file.
type fetchFunc func(offset, length int64) ([]byte, error)

// errNotFLAC is returned when a file does not start with a FLAC stream.
var errNotFLAC = errors.New("not a FLAC stream")

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

// flacHeader reads the start of a FLAC file through fetch and returns the
// bytes that precede the first audio frame: an optional ID3v2 tag, the
// "fLaC" marker and every metadata block (STREAMINFO, VORBIS_COMMENT,
// PICTURE, SEEKTABLE, CUESHEET, PADDING ...).
//
// If those bytes would exceed maxLen, the first maxLen bytes are returned
// with complete set to false. A truncated header is still useful: the bytes
// it holds are correct, only the remainder has to come from the cloud.
//
// size is the length of the whole file; reads never go past it.
func flacHeader(fetch fetchFunc, size, maxLen int64) (header []byte, complete bool, err error) {
	if size < 4 {
		return nil, false, errNotFLAC
	}
	buf, err := fetch(0, min(initialFetch, size))
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
		more, err := fetch(int64(len(buf)), want)
		if err != nil {
			return err
		}
		buf = append(buf, more...)
		return nil
	}

	var pos int64

	// Some taggers put an ID3v2 tag before the FLAC marker. Skip it.
	if len(buf) >= 10 && bytes.Equal(buf[:3], []byte("ID3")) {
		tagSize := int64(buf[6]&0x7f)<<21 | int64(buf[7]&0x7f)<<14 | int64(buf[8]&0x7f)<<7 | int64(buf[9]&0x7f)
		pos = 10 + tagSize
		if buf[5]&0x10 != 0 { // footer present
			pos += 10
		}
	}

	if err := ensure(pos + 4); err != nil {
		return nil, false, err
	}
	if int64(len(buf)) < pos+4 || !bytes.Equal(buf[pos:pos+4], []byte("fLaC")) {
		if pos+4 > maxLen {
			return nil, false, fmt.Errorf("ID3v2 tag larger than the header limit: %w", errNotFLAC)
		}
		return nil, false, errNotFLAC
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
			return nil, false, fmt.Errorf("file ends inside FLAC metadata: %w", errNotFLAC)
		}
		last := buf[pos]&0x80 != 0
		blockType := buf[pos] & 0x7f
		length := int64(buf[pos+1])<<16 | int64(buf[pos+2])<<8 | int64(buf[pos+3])
		if blockType == 127 {
			return nil, false, fmt.Errorf("invalid FLAC metadata block type: %w", errNotFLAC)
		}
		pos += 4 + length
		if pos > size {
			return nil, false, fmt.Errorf("FLAC metadata block runs past end of file: %w", errNotFLAC)
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
				return nil, false, fmt.Errorf("short read of FLAC metadata: %w", errNotFLAC)
			}
			return buf[:pos], true, nil
		}
	}
	return nil, false, fmt.Errorf("more than %d FLAC metadata blocks: %w", maxBlocks, errNotFLAC)
}
