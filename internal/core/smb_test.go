package core

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stamler/medialayer/internal/index"
)

// smbTestServer is an SMB server to test against, from MEDIALAYER_SMB_TEST:
// "host port user password share dir", where dir is the share's folder
// on this machine (for checking what was written). Without it the tests
// skip. A Samba server for it:
//
//	[rw]
//	  path = /srv/smbtest/rw
//	  read only = no
//	  valid users = smbrw
type smbTestServer struct {
	host, user, pass, share, dir string
	port                         int
}

func smbServerForTest(t *testing.T) smbTestServer {
	t.Helper()
	f := strings.Fields(os.Getenv("MEDIALAYER_SMB_TEST"))
	if len(f) != 6 {
		t.Skip("set MEDIALAYER_SMB_TEST to \"host port user password share dir\" to test against an SMB server")
	}
	port, err := strconv.Atoi(f[1])
	if err != nil {
		t.Fatal(err)
	}
	return smbTestServer{host: f[0], port: port, user: f[2], pass: f[3], share: f[4], dir: f[5]}
}

// folder makes an empty folder on the share for one test, returning its
// path on the share and here.
func (s smbTestServer) folder(t *testing.T) (remote, local string) {
	t.Helper()
	name := "t-" + strings.ToLower(index.NewID())
	local = filepath.Join(s.dir, name)
	t.Cleanup(func() { _ = os.RemoveAll(local) })
	return s.share + "/" + name, local
}

func TestSMBServerSignIn(t *testing.T) {
	srv := smbServerForTest(t)
	ctx := context.Background()
	c := newTestCore(t, t.TempDir())
	if _, err := c.AddSMBServer(ctx, srv.host, srv.port, srv.user, "", "wrong password"); err == nil ||
		!strings.Contains(err.Error(), "didn't accept") {
		t.Errorf("a wrong password gave %v", err)
	}
	remote, err := c.AddSMBServer(ctx, srv.host, srv.port, srv.user, "", srv.pass)
	if err != nil {
		t.Fatal(err)
	}
	shares, err := c.ListFolders(ctx, remote, "")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range shares {
		found = found || s == srv.share
	}
	if !found {
		t.Errorf("shares %v lack %s", shares, srv.share)
	}
	// The same server and user again is the same remote, and the
	// password is in no saved file.
	again, err := c.AddSMBServer(ctx, srv.host, srv.port, srv.user, "", srv.pass)
	if err != nil || again != remote {
		t.Errorf("again = %q, %v; want %q", again, err, remote)
	}
	conf, _ := os.ReadFile(filepath.Join(c.dataDir, "rclone.conf"))
	if !strings.Contains(string(conf), "type = smb") || strings.Contains(string(conf), "pass") {
		t.Errorf("rclone.conf = %s", conf)
	}
	if st := c.Status(); len(st.SMBServers) != 1 || !st.SMBServers[0].HasPassword || st.SMBServers[0].User != srv.user {
		t.Errorf("status = %+v", st.SMBServers)
	}
	// Without the password (the app not having given it), folders don't open.
	c.SetSMBPassword(remote, "")
	if _, err := c.ListFolders(ctx, remote, ""); err != ErrSMBNeedsPassword {
		t.Errorf("without the password: %v", err)
	}
	// A password changed on the server: one logon try, not rclone's ten,
	// and the password is forgotten so the next sync doesn't try again.
	c.SetSMBPassword(remote, "changed on the server")
	start := time.Now()
	_, err = c.ListFolders(ctx, remote, "")
	if !errors.Is(err, ErrSMBNeedsPassword) || !strings.Contains(err.Error(), "didn't accept") {
		t.Errorf("an old password gave %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("an old password took %v: retried", d)
	}
	if _, ok := smbPassword(remote); ok {
		t.Error("the refused password is still held")
	}
	// host:port is split.
	if r, err := c.AddSMBServer(ctx, net.JoinHostPort(srv.host, strconv.Itoa(srv.port)), 0, srv.user, "", srv.pass); err != nil || r != remote {
		t.Errorf("host:port = %q, %v; want %q", r, err, remote)
	}
}

func TestSMBErrors(t *testing.T) {
	for msg, want := range map[string]string{
		"response error: The attempted logon is invalid. This is either due to a bad username or authentication information.": "didn't accept",
		"response error: The user account has been automatically locked because too many invalid logon attempts":              "locked this account",
		"response error: The user account password has expired.":                                                              "expired",
	} {
		err := smbError("nas", errors.New(msg))
		if !errors.Is(err, errSMBLogon) || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v", msg, err)
		}
	}
	err := smbError("nas", errors.New("response error: {Access Denied} A process has requested access to an object but has not been granted those access rights."))
	if errors.Is(err, errSMBLogon) || !strings.Contains(err.Error(), "isn't allowed") {
		t.Errorf("access denied: %v", err)
	}
}

// A library copied to an SMB secondary matches the primary, and a second
// sync changes nothing.
func TestSMBSecondary(t *testing.T) {
	srv := smbServerForTest(t)
	ctx := context.Background()
	c, lib, _ := newMirrorCore(t)
	remote, err := c.AddSMBServer(ctx, srv.host, srv.port, srv.user, "", srv.pass)
	if err != nil {
		t.Fatal(err)
	}
	folder, local := srv.folder(t)
	c.mu.Lock()
	c.settings.Secondaries = []source{{ID: "sec1", Remote: remote, Folder: folder}}
	c.mu.Unlock()
	if err := c.sync(ctx); err != nil {
		t.Fatal(err)
	}
	if s := mirrorOf(t, c, "sec1"); s.State != MirrorSynced || s.Copies != 64 || s.Error != "" {
		t.Fatalf("secondary = %+v, want synced with 64 copies", s)
	}
	for _, p := range []string{"A/Album/01.flac", "Extras/59.txt", ".medialayer/repo.json"} {
		want, _ := os.ReadFile(filepath.Join(lib, p))
		got, err := os.ReadFile(filepath.Join(local, p))
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s not copied (%v)", p, err)
		}
		wst, _ := os.Stat(filepath.Join(lib, p))
		gst, _ := os.Stat(filepath.Join(local, p))
		if p[0] != '.' && wst != nil && gst != nil && !wst.ModTime().Truncate(time.Millisecond).Equal(gst.ModTime().Truncate(time.Millisecond)) {
			t.Errorf("%s time %v, primary's %v", p, gst.ModTime(), wst.ModTime())
		}
	}
	entries, _ := filepath.Glob(filepath.Join(local, ".medialayer", "*.partial"))
	if len(entries) > 0 {
		t.Errorf("left %v", entries)
	}
	if err := c.sync(ctx); err != nil {
		t.Fatal(err)
	}
	if s := mirrorOf(t, c, "sec1"); s.State != MirrorSynced || s.Copies+s.Replaces+s.Deletes != 0 {
		t.Errorf("unchanged sync = %+v", s)
	}
}

// An SMB folder can be the primary: it is indexed and published there,
// and a second sync publishes nothing new.
func TestSMBPrimary(t *testing.T) {
	srv := smbServerForTest(t)
	ctx := context.Background()
	lib := writeLibrary(t, nil)
	folder, local := srv.folder(t)
	if err := os.CopyFS(local, os.DirFS(lib)); err != nil {
		t.Fatal(err)
	}
	c, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	remote, err := c.AddSMBServer(ctx, srv.host, srv.port, srv.user, "", srv.pass)
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.settings.PrimaryRemote, c.settings.Folder = remote, folder
	if err := c.openDB(); err != nil {
		t.Fatal(err)
	}
	c.mu.Unlock()
	if err := c.sync(ctx); err != nil {
		t.Fatal(err)
	}
	if n := c.Status().Tracks; n != 3 {
		t.Errorf("tracks = %d, want 3", n)
	}
	snaps, _ := filepath.Glob(filepath.Join(local, ".medialayer/snapshots/*.sqlite"))
	if len(snaps) != 1 {
		t.Fatalf("snapshots %v", snaps)
	}
	if err := c.sync(ctx); err != nil {
		t.Fatal(err)
	}
	again, _ := filepath.Glob(filepath.Join(local, ".medialayer/snapshots/*.sqlite"))
	if len(again) != 1 || again[0] != snaps[0] {
		t.Errorf("a sync with nothing changed published %v", again)
	}
}

// The host helper's rows fit the files as a device lists them over SMB:
// every path, size and time matches, so indexing takes them all.
func TestSMBHostHelper(t *testing.T) {
	srv := smbServerForTest(t)
	ctx := context.Background()
	folder, local := srv.folder(t)
	if err := os.CopyFS(local, os.DirFS(writeHostLibrary(t, bytes.Repeat([]byte("cover"), 2000)))); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(local, "D/Odd name, ünïcode & spaces/01 – track.flac"), encodedFLAC(t, "Odd", 3, 4096))
	runHostOnce(t, local)
	c, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	remote, err := c.AddSMBServer(ctx, srv.host, srv.port, srv.user, "", srv.pass)
	if err != nil {
		t.Fatal(err)
	}
	f, err := openSMB(ctx, remote, folder)
	if err != nil {
		t.Fatal(err)
	}
	inv := loadHostIndex(ctx, f)
	if inv == nil || !inv.tags || !inv.hashes || len(inv.files) != 5 {
		t.Fatalf("inventory over SMB: %+v", inv)
	}
	for p, r := range inv.files {
		o, err := f.NewObject(ctx, p)
		if err != nil {
			t.Errorf("%s: %v", p, err)
			continue
		}
		if inv.row(o.Remote(), o.Size(), o.ModTime(ctx).UnixMilli()) == nil {
			t.Errorf("%s over SMB: size %d time %d; the helper's %d %d", p, o.Size(), o.ModTime(ctx).UnixMilli(), r.Size, r.MTime)
		}
	}
}
