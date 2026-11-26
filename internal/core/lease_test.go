package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
)

// fakeLeaseClock replaces the lease clock for the test; the returned
// function moves it on.
func fakeLeaseClock(t *testing.T) (advance func(time.Duration)) {
	t.Helper()
	var mu sync.Mutex
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	old := leaseClock
	leaseClock = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	t.Cleanup(func() { leaseClock = old })
	return func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
}

// quietLease keeps a waiting device from retrying during the test and
// skips the random wait between attempts.
func quietLease(t *testing.T) {
	t.Helper()
	oldRetry, oldJitter := leaseRetry, leaseJitter
	leaseRetry, leaseJitter = time.Hour, func() time.Duration { return 0 }
	t.Cleanup(func() { leaseRetry, leaseJitter = oldRetry, oldJitter })
}

func writeClaim(t *testing.T, dir string, rec leaseRecord) {
	t.Helper()
	b, _ := json.Marshal(rec)
	write(t, filepath.Join(dir, leaseFile(rec.Device)), b)
}

func readClaim(t *testing.T, dir, device string) leaseRecord {
	t.Helper()
	var rec leaseRecord
	b, err := os.ReadFile(filepath.Join(dir, leaseFile(device)))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

// runSync runs StartSync and waits for its job.
func runSync(t *testing.T, c *Core) Status {
	t.Helper()
	if err := c.StartSync(); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	done := c.jobDone
	c.mu.Unlock()
	if done != nil {
		<-done
	}
	return c.Status()
}

func TestSyncTakesAndReleasesLease(t *testing.T) {
	quietLease(t)
	c, lib, _ := newMirrorCore(t)
	if st := runSync(t, c); st.Job.Phase != PhaseDone {
		t.Fatalf("job = %+v", st.Job)
	}
	rec := readClaim(t, lib, c.deviceID())
	if rec.State != claimReleased || rec.Name == "" || rec.Token == "" {
		t.Errorf("claim after sync = %+v", rec)
	}
	// Lease files are not library files.
	if s := mirrorOf(t, c, "sec1"); s.Copies != 64 {
		t.Errorf("secondary = %+v", s)
	}
}

func TestSyncWaitsForHolder(t *testing.T) {
	quietLease(t)
	c, lib, sec := newMirrorCore(t)
	writeClaim(t, lib, leaseRecord{Device: "other", Name: "Other Mac", State: claimHeld, Token: "t", Renewed: time.Now().UTC()})
	st := runSync(t, c)
	if st.Job.Phase != PhaseWaiting || st.Job.Holder != "Other Mac" || st.Job.Error != "" {
		t.Fatalf("job = %+v", st.Job)
	}
	if _, err := os.Stat(filepath.Join(lib, ".medialayer/repo.json")); !os.IsNotExist(err) {
		t.Errorf("published while another device held the lease (%v)", err)
	}
	if _, err := os.Stat(sec); !os.IsNotExist(err) {
		t.Errorf("copied to the secondary while another device held the lease (%v)", err)
	}
	if _, err := os.Stat(filepath.Join(lib, leaseFile(c.deviceID()))); !os.IsNotExist(err) {
		t.Errorf("wrote a claim while another device held the lease (%v)", err)
	}
	c.mu.Lock()
	retry := c.retry != nil
	c.mu.Unlock()
	if !retry {
		t.Error("no retry scheduled")
	}
}

// A claim is never stale by its own time alone, as that device's clock
// may be wrong; nor is a claim that can't be read, which may be a
// holder's file caught mid-write.
func TestClaimNotStaleByItsOwnTime(t *testing.T) {
	quietLease(t)
	c, lib, _ := newMirrorCore(t)
	writeClaim(t, lib, leaseRecord{Device: "other", Name: "Slow Mac", State: claimHeld, Token: "t", Renewed: time.Now().Add(-2 * time.Hour).UTC()})
	if st := runSync(t, c); st.Job.Phase != PhaseWaiting || st.Job.Holder != "Slow Mac" {
		t.Fatalf("job = %+v", st.Job)
	}
	write(t, filepath.Join(lib, leaseFile("other")), []byte(`{"device":"oth`))
	if st := runSync(t, c); st.Job.Phase != PhaseWaiting || st.Job.Holder != "Another device" {
		t.Fatalf("job with an unreadable claim = %+v", st.Job)
	}
	// A holder doesn't stop for one, as it may be a waiting claim
	// caught mid-write.
	advance := fakeLeaseClock(t)
	os.Remove(filepath.Join(lib, leaseFile("other")))
	l := heldLease(t, c, lib)
	write(t, filepath.Join(lib, leaseFile("other")), []byte(`{"device":"oth`))
	advance(leaseRenew)
	if err := l.renew(context.Background()); err != nil {
		t.Errorf("renew with an unreadable claim = %v", err)
	}
}

// A claim stamped as fresh by its own clock is stale once this device
// has watched it unchanged for leaseTTL on its own clock.
func TestStaleLeaseJudgedByWatching(t *testing.T) {
	quietLease(t)
	advance := fakeLeaseClock(t)
	c, lib, _ := newMirrorCore(t)
	rec := leaseRecord{Device: "other", Name: "Other Mac", State: claimHeld, Token: "t", Renewed: leaseClock().Add(time.Hour).UTC()} // a fast clock
	writeClaim(t, lib, rec)
	if st := runSync(t, c); st.Job.Phase != PhaseWaiting {
		t.Fatalf("first try: job = %+v", st.Job)
	}
	advance(leaseTTL - time.Minute)
	if st := runSync(t, c); st.Job.Phase != PhaseWaiting {
		t.Fatalf("before leaseTTL: job = %+v", st.Job)
	}
	// A renewal starts the watch again.
	rec.Seq++
	writeClaim(t, lib, rec)
	advance(2 * time.Minute)
	if st := runSync(t, c); st.Job.Phase != PhaseWaiting {
		t.Fatalf("after a renewal: job = %+v", st.Job)
	}
	advance(leaseTTL)
	if st := runSync(t, c); st.Job.Phase != PhaseDone || !strings.Contains(st.Notice, "Other Mac") {
		t.Fatalf("after leaseTTL unchanged: job = %+v, notice %q", st.Job, st.Notice)
	}
	if got := readClaim(t, lib, "other"); got.State != claimReleased {
		t.Errorf("stale claim = %+v", got)
	}
}

// A device that finds another one taking the lease waits for it.
func TestWaitingClaimBlocks(t *testing.T) {
	quietLease(t)
	c, lib, _ := newMirrorCore(t)
	writeClaim(t, lib, leaseRecord{Device: "other", Name: "Other Mac", State: claimWaiting, Token: "t", Renewed: time.Now().UTC()})
	if st := runSync(t, c); st.Job.Phase != PhaseWaiting || st.Job.Holder != "Other Mac" {
		t.Fatalf("job = %+v", st.Job)
	}
	if _, err := os.Stat(filepath.Join(lib, leaseFile(c.deviceID()))); !os.IsNotExist(err) {
		t.Errorf("wrote a claim while another device was taking the lease (%v)", err)
	}
}

// Two devices starting at once each see the other's claim and back off,
// retrying a few times.
func TestRaceBacksOff(t *testing.T) {
	quietLease(t)
	c, lib, _ := newMirrorCore(t)
	tries := 0
	old := leasePut
	leasePut = func(ctx context.Context, f fs.Fs, remote string, b []byte) error {
		var rec leaseRecord
		_ = json.Unmarshal(b, &rec)
		if rec.State == claimWaiting {
			tries++
			writeClaim(t, lib, leaseRecord{Device: "other", Name: "Other Mac", State: claimWaiting, Token: fmt.Sprint(tries), Renewed: time.Now().UTC()})
		} else if rec.State == claimReleased {
			// The other device backs off too.
			writeClaim(t, lib, leaseRecord{Device: "other", Name: "Other Mac", State: claimReleased, Token: fmt.Sprint(tries)})
		}
		return old(ctx, f, remote, b)
	}
	t.Cleanup(func() { leasePut = old })
	if st := runSync(t, c); st.Job.Phase != PhaseWaiting || tries != leaseTries {
		t.Fatalf("job = %+v after %d tries", st.Job, tries)
	}
	if rec := readClaim(t, lib, c.deviceID()); rec.State != claimReleased {
		t.Errorf("own claim = %+v", rec)
	}

	// When the other device backs off for good, this one takes it.
	leasePut = old
	if st := runSync(t, c); st.Job.Phase != PhaseDone {
		t.Fatalf("job = %+v", st.Job)
	}
}

func TestFullPrimaryBlocksSync(t *testing.T) {
	quietLease(t)
	old := leasePut
	leasePut = func(context.Context, fs.Fs, string, []byte) error {
		return fmt.Errorf("upload: %w", syscall.ENOSPC)
	}
	t.Cleanup(func() { leasePut = old })
	c, _, sec := newMirrorCore(t)
	st := runSync(t, c)
	if st.Job.Phase != PhaseError || !strings.Contains(st.Job.Error, "full") {
		t.Fatalf("job = %+v", st.Job)
	}
	if _, err := os.Stat(sec); !os.IsNotExist(err) {
		t.Errorf("synced the secondary without the lease (%v)", err)
	}
}

// heldLease takes the lease on lib for c.
func heldLease(t *testing.T, c *Core, lib string) *lease {
	t.Helper()
	f, err := fs.NewFs(context.Background(), lib)
	if err != nil {
		t.Fatal(err)
	}
	l, err := c.acquire(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestRenew(t *testing.T) {
	ctx := context.Background()
	quietLease(t)
	advance := fakeLeaseClock(t)
	c, lib, _ := newMirrorCore(t)
	me := c.deviceID()

	l := heldLease(t, c, lib)
	seq := readClaim(t, lib, me).Seq
	if err := l.renew(ctx); err != nil || readClaim(t, lib, me).Seq != seq {
		t.Fatalf("renew before due: %v, seq %d -> %d", err, seq, readClaim(t, lib, me).Seq)
	}
	// A device waiting doesn't disturb the holder.
	writeClaim(t, lib, leaseRecord{Device: "other", Name: "Other Mac", State: claimWaiting, Token: "t", Renewed: leaseClock().UTC()})
	advance(leaseRenew)
	if err := l.renew(ctx); err != nil {
		t.Fatal(err)
	}
	if rec := readClaim(t, lib, me); rec.Seq != seq+1 || rec.State != claimHeld || !rec.Renewed.Equal(leaseClock().UTC()) {
		t.Errorf("renewed claim = %+v", rec)
	}

	// Taken over: the claim was released by another device.
	rec := readClaim(t, lib, me)
	rec.State, rec.TakenBy = claimReleased, "other"
	writeClaim(t, lib, rec)
	advance(leaseRenew)
	if err := l.renew(ctx); !errors.Is(err, errLeaseLost) {
		t.Errorf("renew after takeover = %v", err)
	}

	// Another holder: both stop.
	l.release(ctx)
	writeClaim(t, lib, leaseRecord{Device: "other", State: claimReleased})
	l = heldLease(t, c, lib)
	writeClaim(t, lib, leaseRecord{Device: "other", Name: "Other Mac", State: claimHeld, Token: "t", Renewed: leaseClock().UTC()})
	advance(leaseRenew)
	if err := l.renew(ctx); !errors.Is(err, errLeaseLost) || !strings.Contains(err.Error(), "Other Mac") {
		t.Errorf("renew with another holder = %v", err)
	}
	l.release(ctx)

	// Lapsed, as after a long sleep: stop without writing.
	writeClaim(t, lib, leaseRecord{Device: "other", State: claimReleased})
	l = heldLease(t, c, lib)
	seq = readClaim(t, lib, me).Seq
	advance(leaseLapse)
	if err := l.renew(ctx); !errors.Is(err, errLeaseLost) {
		t.Errorf("renew after a lapse = %v", err)
	}
	if rec := readClaim(t, lib, me); rec.Seq != seq || rec.State != claimHeld {
		t.Errorf("claim written after a lapse: %+v", rec)
	}
}

// Work under a lease that is lost is cancelled, and the loss is the
// result.
func TestWithLeaseStopsWorkOnLoss(t *testing.T) {
	quietLease(t)
	advance := fakeLeaseClock(t)
	oldTick := leaseTick
	leaseTick = 5 * time.Millisecond
	t.Cleanup(func() { leaseTick = oldTick })
	c, lib, _ := newMirrorCore(t)
	f, err := fs.NewFs(context.Background(), lib)
	if err != nil {
		t.Fatal(err)
	}
	err = c.withLease(context.Background(), f, func(ctx context.Context) error {
		advance(leaseLapse)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
			return errors.New("work was not stopped")
		}
	})
	if !errors.Is(err, errLeaseLost) {
		t.Fatalf("withLease = %v", err)
	}
}

func TestPromoteWaitsForTargetLease(t *testing.T) {
	quietLease(t)
	ctx := context.Background()
	c, lib, sec := newMirrorCore(t)
	if err := c.sync(ctx); err != nil {
		t.Fatal(err)
	}
	writeClaim(t, sec, leaseRecord{Device: "other", Name: "Other Mac", State: claimHeld, Token: "t", Renewed: time.Now().UTC()})
	if err := c.Promote("sec1"); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	done := c.jobDone
	c.mu.Unlock()
	<-done
	if st := c.Status(); st.Job.Phase != PhaseWaiting || !st.Sources[0].Primary || st.Sources[0].Folder != lib {
		t.Fatalf("status = %+v", st)
	}
	if r := readRepoFile(t, lib); r.Generation != 1 {
		t.Errorf("generation = %d", r.Generation)
	}
	if rec := readClaim(t, lib, c.deviceID()); rec.State != claimReleased {
		t.Errorf("primary's claim = %+v", rec)
	}
}

// A renewal stuck on the network doesn't keep a lapsed lease: writes
// under it fail, and the next tick cancels the work.
func TestStuckRenewalStopsWrites(t *testing.T) {
	quietLease(t)
	advance := fakeLeaseClock(t)
	oldTick := leaseTick
	leaseTick = 5 * time.Millisecond
	t.Cleanup(func() { leaseTick = oldTick })
	old := leasePut
	stuck := make(chan struct{})
	leasePut = func(ctx context.Context, f fs.Fs, remote string, b []byte) error {
		var rec leaseRecord
		_ = json.Unmarshal(b, &rec)
		if rec.State == claimHeld && rec.Seq > 1 { // a renewal
			close(stuck)
			<-ctx.Done()
			return ctx.Err()
		}
		return old(ctx, f, remote, b)
	}
	t.Cleanup(func() { leasePut = old })
	c, lib, _ := newMirrorCore(t)
	f, err := fs.NewFs(context.Background(), lib)
	if err != nil {
		t.Fatal(err)
	}
	err = c.withLease(context.Background(), f, func(ctx context.Context) error {
		advance(leaseRenew)
		<-stuck
		if err := checkLease(ctx); err != nil {
			return fmt.Errorf("checked too soon: %w", err)
		}
		advance(leaseLapse)
		if err := checkLease(ctx); err == nil {
			return errors.New("checkLease passed a lapsed lease")
		}
		// The tick cancels the work while the renewal is still stuck.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
			return errors.New("work was not stopped")
		}
	})
	if !errors.Is(err, errLeaseLost) || strings.Contains(err.Error(), "too soon") {
		t.Fatalf("withLease = %v", err)
	}
	if rec := readClaim(t, lib, c.deviceID()); rec.State != claimReleased {
		t.Errorf("claim after a lapse = %+v", rec)
	}
}

// A device that lost the lease tries again later; a sync started
// meanwhile replaces that retry, and Close stops it.
func TestRetryAfterLoss(t *testing.T) {
	quietLease(t)
	c, _, _ := newMirrorCore(t)
	c.mu.Lock()
	c.job.Phase = PhaseIdle
	err := c.startJobLocked(func(context.Context) error { return fmt.Errorf("%w: test", errLeaseLost) })
	done := c.jobDone
	c.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	<-done
	c.mu.Lock()
	retry := c.retry
	c.mu.Unlock()
	if st := c.Status(); st.Job.Phase != PhaseError || retry == nil {
		t.Fatalf("job = %+v, retry %v", st.Job, retry != nil)
	}
	runSync(t, c)
	if retry.Stop() {
		t.Error("a sync started meanwhile left the retry pending")
	}
	_ = c.Close()
	if err := c.StartSync(); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	running := c.jobDone != nil
	c.mu.Unlock()
	if running {
		t.Error("a sync started after Close")
	}
}
