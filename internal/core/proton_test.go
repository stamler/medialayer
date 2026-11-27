package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rclone/Proton-API-Bridge/common"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/hash"
)

// fakeProton puts a signed-in Proton remote in the core's config.
func fakeProton(t *testing.T, name, user string) {
	t.Helper()
	config.FileSetValue(name, "type", protonType)
	config.FileSetValue(name, "username", user)
	for _, k := range protonSessionKeys {
		config.FileSetValue(name, k, "x")
	}
}

func TestProtonAccounts(t *testing.T) {
	newTestCore(t, t.TempDir())
	fakeProton(t, protonRemote, "a@proton.me")
	got := protonAccounts()
	if len(got) != 1 || got[0] != (ProtonAccount{Remote: protonRemote, User: "a@proton.me", SignedIn: true}) {
		t.Fatalf("protonAccounts() = %+v", got)
	}
	if !remoteConnected(protonRemote) {
		t.Error("a Proton remote with a session isn't connected")
	}
	// Proton ending the session clears these keys.
	config.FileSetValue(protonRemote, "client_access_token", "")
	if remoteConnected(protonRemote) || protonAccounts()[0].SignedIn {
		t.Error("a Proton remote without a session counts as signed in")
	}
}

func TestConnectProtonRefusesSecondAccountInUse(t *testing.T) {
	c := newTestCore(t, t.TempDir())
	fakeProton(t, protonRemote, "a@proton.me")
	ctx := context.Background()
	if _, err := c.ConnectProton(ctx, " ", "pw", "", ""); err == nil {
		t.Error("signed in without a username")
	}
	if _, err := c.ConnectProton(ctx, "b@proton.me", "", "", ""); err == nil {
		t.Error("signed in without a password")
	}
	c.mu.Lock()
	c.settings.Secondaries = []source{{ID: "p", Remote: protonRemote, Folder: "Music"}}
	c.mu.Unlock()
	_, err := c.ConnectProton(ctx, "b@proton.me", "pw", "", "")
	if err == nil || !strings.Contains(err.Error(), "one Proton account") {
		t.Errorf("a second account replaced the one in use: %v", err)
	}
	if got := config.GetValue(protonRemote, "username"); got != "a@proton.me" {
		t.Errorf("the account in use became %q", got)
	}
}

func TestProtonSecondaryRules(t *testing.T) {
	c, _, _ := newMirrorCore(t)
	fakeProton(t, protonRemote, "a@proton.me")
	c.mu.Lock()
	c.settings.Secondaries = append(c.settings.Secondaries, source{ID: "p", Remote: protonRemote, Folder: "Music"})
	c.mu.Unlock()
	st := mirrorOf(t, c, "p")
	if st.Type != protonType || !st.CaseBlind {
		t.Errorf("Proton secondary status = %+v", st)
	}
	if st := mirrorOf(t, c, "sec1"); !st.CaseBlind {
		t.Errorf("local secondary status = %+v", st)
	}
}

func TestProtonErrorWhenSignedOut(t *testing.T) {
	for _, err := range []error{common.ErrUsernameAndPasswordRequired, common.Err2FACodeRequired} {
		wrapped := fmt.Errorf("couldn't initialize a new proton drive instance: %w", err)
		if got := protonError(wrapped); !strings.Contains(got.Error(), "saved sign-in") {
			t.Errorf("protonError(%v) = %v", wrapped, got)
		}
	}
	other := errors.New("network down")
	if protonError(other) != other {
		t.Error("protonError changed an unrelated error")
	}
}

// sha1Only shows a local object as on a cloud that keeps SHA-1 only, as
// Proton Drive does.
type sha1Only struct{ fs.Object }

func (o sha1Only) Fs() fs.Info { return sha1Info{o.Object.Fs()} }

type sha1Info struct{ fs.Info }

func (sha1Info) Hashes() hash.Set { return hash.Set(hash.SHA1) }

func TestSameFileBySHA1(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	write(t, filepath.Join(dir, "remote.bin"), []byte("same bytes"))
	f, err := fs.NewFs(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	o, err := f.NewObject(ctx, "remote.bin")
	if err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(t.TempDir(), "local.bin")
	write(t, local, []byte("same bytes"))
	if !sameFile(ctx, sha1Only{o}, local) {
		t.Error("files with one SHA-1 differ")
	}
	if err := os.WriteFile(local, []byte("other byte"), 0o644); err != nil {
		t.Fatal(err)
	}
	if sameFile(ctx, sha1Only{o}, local) {
		t.Error("files with different SHA-1s are the same")
	}
}

func TestSameProtonUser(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		same bool
	}{
		{"alice@proton.me", "Alice@Proton.me", true},
		{"alice@proton.me", "alice", true},
		{"alice", "alice@pm.me", true},
		{"alice", "alice@gmail.com", false},
		{"alice@proton.me", "alice@pm.me", false},
		{"alice", "bob", false},
	} {
		if got := sameProtonUser(tc.a, tc.b); got != tc.same {
			t.Errorf("sameProtonUser(%q, %q) = %v", tc.a, tc.b, got)
		}
	}
}

func TestOpenProtonKeepsSession(t *testing.T) {
	newTestCore(t, t.TempDir())
	fakeProton(t, protonRemote, "a@proton.me")
	k := keepSession(protonRemote)
	k.Set("client_access_token", "")
	if !protonSignedIn(protonRemote) {
		t.Error("a cleared session was saved")
	}
	k.Set("client_access_token", "renewed")
	if got := config.GetValue(protonRemote, "client_access_token"); got != "renewed" {
		t.Errorf("a renewed session wasn't saved: %q", got)
	}
}

// coarseFs reports times kept to the second, as Proton Drive does.
type coarseFs struct{ fs.Fs }

func (coarseFs) Precision() time.Duration { return time.Second }

// A primary that keeps times to the second (a Proton folder made the
// primary) keeps every file's id and doesn't read it again: each row
// takes the time the cloud reports.
func TestIndexKeepsFilesOnCoarseTimes(t *testing.T) {
	ctx := context.Background()
	lib := writeLibrary(t, nil)
	base := time.Date(2026, 1, 2, 3, 4, 5, 678_000_000, time.UTC)
	for _, p := range []string{"A/Album/01.flac", "A/Album/02.flac", "B/Other/01.flac"} {
		if err := os.Chtimes(filepath.Join(lib, p), base, base); err != nil {
			t.Fatal(err)
		}
	}
	c := newTestCore(t, lib)
	f, err := fs.NewFs(ctx, lib)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.index(ctx, f); err != nil {
		t.Fatal(err)
	}
	before := fileIDs(t, c)
	whole := base.Truncate(time.Second)
	for p := range before {
		if err := os.Chtimes(filepath.Join(lib, p), whole, whole); err != nil {
			t.Fatal(err)
		}
	}
	// Unreadable now: indexing it again would fail.
	if err := os.WriteFile(filepath.Join(lib, "B/Other/01.flac"), make([]byte, fileSize(filepath.Join(lib, "B/Other/01.flac"))), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(lib, "B/Other/01.flac"), whole, whole); err != nil {
		t.Fatal(err)
	}
	if err := c.index(ctx, coarseFs{f}); err != nil {
		t.Fatal(err)
	}
	known, err := c.currentDB().KnownFiles()
	if err != nil {
		t.Fatal(err)
	}
	for p, id := range before {
		k := known[p]
		if k.ID != id || k.MTime != whole.UnixMilli() {
			t.Errorf("%s: id %s -> %s, mtime %d (want %d)", p, id, k.ID, k.MTime, whole.UnixMilli())
		}
	}
	// On a cloud with fine times, a time off by under a second is a change.
	if withinPrecision(f, whole.UnixMilli(), base.UnixMilli()) {
		t.Error("a local folder's times count as the same within a second")
	}
}
