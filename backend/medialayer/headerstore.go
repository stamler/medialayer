package medialayer

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/rclone/rclone/fs/hash"
)

// headerRecord is what the store keeps for one file: the bytes an app reads
// to get its tags and cover art, plus the identity of the file they came
// from (size, modification time and, where the remote has a cheap one, a
// content hash), so a changed file is never answered from stale bytes.
type headerRecord struct {
	Size     int64     // size of the whole file
	ModTime  time.Time // modification time of the file when primed
	HashType hash.Type // hash.None if no hash was stored
	Hash     string    // the remote's hash of the whole file
	Complete bool      // Header holds every byte before the audio
	Header   []byte    // bytes [0, len(Header)) of the file
}

// recordFixed is the fixed-size start of a record file. It is followed by
// HashLen bytes of hash and HeaderLen bytes of header. CRC covers the
// whole record (see recordCRC), so a damaged record is never served.
type recordFixed struct {
	Magic       [4]byte
	Size        int64
	ModTimeSec  int64 // Unix seconds
	ModTimeNsec int32
	HashType    int32
	HashLen     uint16
	Complete    uint8
	HeaderLen   int64
	CRC         uint32
}

// recordMagic starts every record file.
var recordMagic = [4]byte{'M', 'L', 'H', '2'}

// maxRecordHash bounds the stored hash, which is a short hex string.
const maxRecordHash = 1024

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

	info, err := f.Stat()
	if err != nil {
		return nil
	}
	var fixed recordFixed
	if err := binary.Read(f, binary.LittleEndian, &fixed); err != nil {
		return nil
	}
	if fixed.Magic != recordMagic || fixed.HeaderLen < 0 || fixed.HeaderLen > fixed.Size ||
		fixed.HeaderLen > maxRecordHeader || fixed.HashLen > maxRecordHash ||
		info.Size() != int64(binary.Size(fixed))+int64(fixed.HashLen)+fixed.HeaderLen {
		return nil
	}
	body := make([]byte, int64(fixed.HashLen)+fixed.HeaderLen)
	if _, err := io.ReadFull(f, body); err != nil {
		return nil
	}
	if recordCRC(fixed, body) != fixed.CRC {
		return nil
	}
	return &headerRecord{
		Size:     fixed.Size,
		ModTime:  time.Unix(fixed.ModTimeSec, int64(fixed.ModTimeNsec)),
		HashType: hash.Type(fixed.HashType),
		Hash:     string(body[:fixed.HashLen]),
		Complete: fixed.Complete == 1,
		Header:   body[fixed.HashLen:],
	}
}

// recordCRC is the CRC-32 (IEEE) of the fixed fields, with CRC itself
// zeroed, followed by body (the hash and the header).
func recordCRC(fixed recordFixed, body []byte) uint32 {
	fixed.CRC = 0
	var b bytes.Buffer
	_ = binary.Write(&b, binary.LittleEndian, &fixed)
	return crc32.Update(crc32.ChecksumIEEE(b.Bytes()), crc32.IEEETable, body)
}

// maxRecordHeader bounds what get will allocate for one record.
const maxRecordHeader = 256 * 1024 * 1024

// put stores rec under key, replacing any older record atomically. The
// record is synced to disk before the rename, so a crash leaves either the
// old record or the new one.
func (s *headerStore) put(key string, rec *headerRecord) error {
	if len(rec.Hash) > maxRecordHash {
		return fmt.Errorf("hash too long (%d bytes)", len(rec.Hash))
	}
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
	fixed := recordFixed{
		Magic:       recordMagic,
		Size:        rec.Size,
		ModTimeSec:  rec.ModTime.Unix(),
		ModTimeNsec: int32(rec.ModTime.Nanosecond()),
		HashType:    int32(rec.HashType),
		HashLen:     uint16(len(rec.Hash)),
		Complete:    complete,
		HeaderLen:   int64(len(rec.Header)),
	}
	fixed.CRC = recordCRC(fixed, append([]byte(rec.Hash), rec.Header...))
	err = binary.Write(tmp, binary.LittleEndian, &fixed)
	if err == nil {
		_, err = tmp.WriteString(rec.Hash)
	}
	if err == nil {
		_, err = tmp.Write(rec.Header)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if err != nil {
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
