package medialayer

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// headerRecord is what the store keeps for one file: the bytes an app reads
// to get its tags and cover art, plus the size and modification time of the
// file they came from, so a changed file is never answered from stale bytes.
type headerRecord struct {
	Size     int64     // size of the whole file
	ModTime  time.Time // modification time of the file when primed
	Complete bool      // Header holds every byte before the audio
	Header   []byte    // bytes [0, len(Header)) of the file
}

// recordFixed is the fixed-size start of a record file, followed by
// HeaderLen bytes of header.
type recordFixed struct {
	Magic     [4]byte
	Size      int64
	ModTime   int64 // Unix nanoseconds
	Complete  uint8
	HeaderLen int64
}

// recordMagic starts every record file.
var recordMagic = [4]byte{'M', 'L', 'H', '1'}

// headerStore keeps one small file per primed media file in a local
// directory. It is the spike's stand-in for the header packs that will live
// in the cloud repository; the lookup logic is the same.
type headerStore struct {
	dir string
}

func newHeaderStore(dir string) (*headerStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create header store: %w", err)
	}
	return &headerStore{dir: dir}, nil
}

// pathFor maps a file's key to its record file. The key already names the
// remote and the file, so equal keys mean the same file.
func (s *headerStore) pathFor(key string) string {
	sum := sha256.Sum256([]byte(key))
	name := hex.EncodeToString(sum[:])
	return filepath.Join(s.dir, name[:2], name+".mlh")
}

// get returns the record for key, or nil if there is none or it is
// unreadable. A damaged record is treated as missing, never as an error,
// because the cloud copy can always answer instead.
func (s *headerStore) get(key string) *headerRecord {
	f, err := os.Open(s.pathFor(key))
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()

	var fixed recordFixed
	if err := binary.Read(f, binary.LittleEndian, &fixed); err != nil {
		return nil
	}
	if fixed.Magic != recordMagic || fixed.HeaderLen < 0 || fixed.HeaderLen > fixed.Size || fixed.HeaderLen > maxRecordHeader {
		return nil
	}
	header := make([]byte, fixed.HeaderLen)
	if _, err := io.ReadFull(f, header); err != nil {
		return nil
	}
	return &headerRecord{
		Size:     fixed.Size,
		ModTime:  time.Unix(0, fixed.ModTime),
		Complete: fixed.Complete == 1,
		Header:   header,
	}
}

// maxRecordHeader bounds what get will allocate for one record.
const maxRecordHeader = 256 * 1024 * 1024

// put stores rec under key, replacing any older record atomically.
func (s *headerStore) put(key string, rec *headerRecord) error {
	path := s.pathFor(key)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op after a successful rename

	complete := uint8(0)
	if rec.Complete {
		complete = 1
	}
	fixed := recordFixed{recordMagic, rec.Size, rec.ModTime.UnixNano(), complete, int64(len(rec.Header))}
	if err := binary.Write(tmp, binary.LittleEndian, &fixed); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(rec.Header); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// remove deletes the record for key. A missing record is not an error.
func (s *headerStore) remove(key string) error {
	err := os.Remove(s.pathFor(key))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// drop deletes every record.
func (s *headerStore) drop() error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(s.dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}
