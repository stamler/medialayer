package medialayer

import (
	"bytes"
	"os"
	"testing"
	"time"

	"github.com/rclone/rclone/fs/hash"
)

func TestHeaderStoreRoundTrip(t *testing.T) {
	s, err := newHeaderStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// A time outside the range UnixNano can represent must survive.
	when := time.Date(2300, 1, 2, 3, 4, 5, 6, time.UTC)
	in := &headerRecord{Size: 5000, ModTime: when, HashType: hash.MD5, Hash: "abc123", Complete: true, Header: randomBytes(3000, 4)}
	if err := s.put("k", in); err != nil {
		t.Fatal(err)
	}
	out := s.get("k")
	if out == nil {
		t.Fatal("record not found")
	}
	if out.Size != in.Size || !out.ModTime.Equal(when) || out.HashType != in.HashType || out.Hash != in.Hash ||
		out.Complete != in.Complete || !bytes.Equal(out.Header, in.Header) {
		t.Fatalf("got %+v, want %+v", out, in)
	}
}

func TestHeaderStoreIgnoresDamagedRecords(t *testing.T) {
	s, err := newHeaderStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.put("k", &headerRecord{Size: 5000, ModTime: time.Now(), Header: randomBytes(3000, 4)}); err != nil {
		t.Fatal(err)
	}
	good, err := os.ReadFile(s.pathFor("k"))
	if err != nil {
		t.Fatal(err)
	}
	damage := map[string][]byte{
		"flipped byte":   append(append([]byte(nil), good[:len(good)-10]...), append([]byte{good[len(good)-10] ^ 1}, good[len(good)-9:]...)...),
		"truncated":      good[:len(good)-1],
		"flipped field":  append(append(append([]byte(nil), good[:24]...), good[24]^1), good[25:]...), // HashType
		"trailing bytes": append(append([]byte(nil), good...), 0),
	}
	for name, b := range damage {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(s.pathFor("k"), b, 0o600); err != nil {
				t.Fatal(err)
			}
			if rec := s.get("k"); rec != nil {
				t.Fatal("damaged record was returned")
			}
		})
	}
}
