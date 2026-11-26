package core

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/rclone/rclone/fs"

	"github.com/stamler/medialayer/internal/index"
)

// The sync lease lets one device at a time sync a library. It lives in
// the primary's .medialayer folder, one file per device
// (lease-<device id>.json), because Drive has no "create only if absent":
// two devices writing one lease.json at once would leave two files of
// that name. The files sit at the top of .medialayer, which every library
// already has, because two devices creating a new lease folder at once
// could leave two folders of one name on Drive, each seeing only its own.
// Each device writes only its own file, except to mark another's stale
// claim released.
//
// To take it, a device writes its claim as "waiting", lists the folder,
// and holds the lease (rewriting its claim as "held") only if no other
// live claim is there. Two devices that start together each see the
// other's claim and both back off, retrying after a short random wait.
//
// The holder renews its claim every leaseRenew. A claim not renewed for
// leaseTTL is stale: another device judges that only by watching the file
// stay unchanged for leaseTTL on its own clock, so a wrong clock on either
// device can't free a lease early. A device taking over marks the stale
// claim released; its old holder notices on its next renewal and stops.
// A holder that couldn't renew for leaseLapse (a Mac asleep, or a
// renewal stuck on the network) stops before anyone could take over: its
// writes check the lease first (checkLease), and a renewal gives up at
// that time.
//
// Holders ignore "waiting" claims, as their writers back off. A holder
// that finds another "held" claim releases its own and stops; the other
// does the same at its next renewal, and both try again later. A claim
// that can't be read as one counts as held, as it may be a holder's file
// caught mid-write.

const (
	leaseTTL   = 10 * time.Minute
	leaseRenew = 2 * time.Minute
	// leaseLapse is how long a holder may go without renewing before it
	// stops: short of leaseTTL, so it stops before anyone takes over.
	leaseLapse = leaseTTL - time.Minute
	leaseTries = 3 // attempts when two devices start at once
)

// Overridden in tests.
var (
	leaseClock  = func() time.Time { return time.Now().Round(0) } // wall clock: it counts time asleep
	leaseTick   = 15 * time.Second                                // how often a holder checks its lease
	leaseRetry  = leaseRenew                                      // when a device kept waiting tries again
	leaseJitter = func() time.Duration { return time.Second + rand.N(4*time.Second) }
	leasePut    = putBytes
)

// Claim states.
const (
	claimWaiting  = "waiting"
	claimHeld     = "held"
	claimReleased = "released"
)

// leaseFile is a device's claim file.
func leaseFile(device string) string { return repoDir + "/lease-" + device + ".json" }

// leaseRecord is one device's claim, .medialayer/lease-<device id>.json.
type leaseRecord struct {
	Device  string    `json:"device"`
	Name    string    `json:"name,omitempty"` // shown to other devices
	State   string    `json:"state"`
	Token   string    `json:"token"` // one take of the lease
	Seq     int64     `json:"seq"`   // raised on every write, so a renewal always changes the file
	Taken   time.Time `json:"taken"`
	Renewed time.Time `json:"renewed"`
	// TakenBy is the device that found this claim stale and released it.
	TakenBy string `json:"taken_by,omitempty"`
}

// leaseBusyError means another device holds the lease, or is taking it.
type leaseBusyError struct{ name string }

func (e *leaseBusyError) Error() string {
	return fmt.Sprintf("%s is syncing this library; this device will sync after it", e.name)
}

// errLeaseLost ends a sync whose lease another device took or that
// couldn't be renewed in time.
var errLeaseLost = errors.New("this device lost the sync lock")

// leaseSighting is when this device first saw a claim file as it is now.
type leaseSighting struct {
	raw   string
	since time.Time
}

// claim is another device's claim as last listed.
type claim struct {
	rec leaseRecord
	raw []byte
	// unreadable claims count as held when taking the lease; a holder
	// ignores them, as it ignores waiting claims.
	unreadable bool
}

// deviceName is the name other devices show for this one.
func deviceName() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "another device"
	}
	h = strings.TrimSuffix(h, ".local")
	return strings.ReplaceAll(h, "-", " ")
}

// others lists the live claims of other devices on f: not released, and
// split into fresh and stale. It records when each claim was first seen
// as it is, which is how staleness is judged.
func (c *Core) others(ctx context.Context, f fs.Fs, me string) (fresh, stale []claim, err error) {
	entries, err := f.List(ctx, repoDir)
	if errors.Is(err, fs.ErrorDirNotFound) {
		entries, err = nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	now := leaseClock()
	prefix := fs.ConfigString(f) + "|"
	seen := map[string]bool{}
	for _, e := range entries {
		o, ok := e.(fs.Object)
		if !ok {
			continue
		}
		name := path.Base(o.Remote())
		device, ok := strings.CutPrefix(name, "lease-")
		device, ok2 := strings.CutSuffix(device, ".json")
		if !ok || !ok2 || device == me {
			continue
		}
		b, err := readSmall(ctx, f, repoDir+"/"+name)
		if absent(err) {
			continue // removed by hand, say
		}
		if err != nil {
			return nil, nil, err
		}
		var rec leaseRecord
		bad := json.Unmarshal(b, &rec) != nil
		if bad {
			// Perhaps caught mid-write: it counts as held until it has
			// stayed unchanged for leaseTTL.
			fs.Errorf(f, "lease %s can't be read", name)
			rec = leaseRecord{State: claimHeld}
		}
		rec.Device = device
		key := prefix + name
		seen[key] = true
		c.mu.Lock()
		if c.leaseSeen == nil {
			c.leaseSeen = map[string]leaseSighting{}
		}
		s, ok := c.leaseSeen[key]
		if !ok || s.raw != string(b) {
			s = leaseSighting{raw: string(b), since: now}
			c.leaseSeen[key] = s
		}
		c.mu.Unlock()
		if rec.State == claimReleased {
			continue
		}
		cl := claim{rec: rec, raw: b, unreadable: bad}
		if now.Sub(s.since) >= leaseTTL {
			stale = append(stale, cl)
		} else {
			fresh = append(fresh, cl)
		}
	}
	c.mu.Lock()
	for key := range c.leaseSeen {
		if strings.HasPrefix(key, prefix) && !seen[key] {
			delete(c.leaseSeen, key)
		}
	}
	c.mu.Unlock()
	return fresh, stale, nil
}

// lease is a lease this device holds on one folder.
type lease struct {
	c *Core
	f fs.Fs

	mu   sync.Mutex // held through a renewal
	rec  leaseRecord
	lost error

	okMu   sync.Mutex
	okWall time.Time // when the last good renewal started, by leaseClock
	okMono time.Time // the same, by the monotonic clock
}

func (l *lease) put(ctx context.Context) error {
	b, err := json.MarshalIndent(l.rec, "", "  ")
	if err != nil {
		return err
	}
	return leasePut(ctx, l.f, leaseFile(l.rec.Device), b)
}

// renewed records a good renewal that started at wall.
func (l *lease) renewed(wall time.Time, mono time.Time) {
	l.okMu.Lock()
	l.okWall, l.okMono = wall, mono
	l.okMu.Unlock()
}

// since is how long ago the last good renewal started. It takes the
// longer of the wall clock (which counts time asleep) and the monotonic
// clock (which a clock set back can't shorten).
func (l *lease) since() time.Duration {
	l.okMu.Lock()
	defer l.okMu.Unlock()
	return max(leaseClock().Sub(l.okWall), time.Since(l.okMono))
}

// valid reports a lease that may have lapsed.
func (l *lease) valid() error {
	if d := l.since(); d >= leaseLapse {
		return fmt.Errorf("%w: it couldn't be renewed for %s, so another device may have taken it", errLeaseLost, d.Round(time.Second))
	}
	return nil
}

type leasesKey struct{}

// checkLease returns an error if a lease that ctx's work holds may have
// lapsed. Sync calls it before writing.
func checkLease(ctx context.Context) error {
	ls, _ := ctx.Value(leasesKey{}).([]*lease)
	for _, l := range ls {
		if err := l.valid(); err != nil {
			return err
		}
	}
	return nil
}

// acquire takes the lease on f, or returns a *leaseBusyError.
func (c *Core) acquire(ctx context.Context, f fs.Fs) (*lease, error) {
	me := c.deviceID()
	l := &lease{c: c, f: f}
	// A release while taking the lease must happen even when the job is
	// being cancelled, or the claim would block other devices.
	rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer rcancel()
	busy := func(cl claim) error {
		return &leaseBusyError{name: cmp.Or(cl.rec.Name, "Another device")}
	}
	for attempt := 1; ; attempt++ {
		fresh, stale, err := c.others(ctx, f, me)
		if err != nil {
			return nil, fmt.Errorf("read the sync lock: %w", err)
		}
		var waiting *claim
		for i, cl := range fresh {
			if cl.rec.State == claimHeld {
				return nil, busy(cl)
			}
			waiting = &fresh[i]
		}
		if waiting == nil {
			var took []string
			for _, cl := range stale {
				rec := cl.rec
				rec.State, rec.TakenBy, rec.Seq = claimReleased, me, rec.Seq+1
				b, err := json.MarshalIndent(rec, "", "  ")
				if err != nil {
					return nil, err
				}
				if err := leasePut(ctx, f, leaseFile(cl.rec.Device), b); err != nil {
					return nil, c.leaseWriteErr(err)
				}
				if cl.rec.State == claimHeld {
					took = append(took, cmp.Or(cl.rec.Name, "another device"))
				}
			}
			now := leaseClock()
			l.rec = leaseRecord{Device: me, Name: deviceName(), State: claimWaiting, Token: index.NewID(), Taken: now.UTC(), Renewed: now.UTC()}
			if err := l.put(ctx); err != nil {
				return nil, c.leaseWriteErr(err)
			}
			fresh, _, err = c.others(ctx, f, me)
			if err != nil {
				l.release(rctx)
				return nil, fmt.Errorf("read the sync lock: %w", err)
			}
			if len(fresh) == 0 {
				wall, mono := leaseClock(), time.Now()
				l.rec.State, l.rec.Seq, l.rec.Renewed = claimHeld, l.rec.Seq+1, wall.UTC()
				if err := l.put(ctx); err != nil {
					l.release(rctx)
					return nil, c.leaseWriteErr(err)
				}
				if len(took) > 0 {
					c.mu.Lock()
					c.notice = fmt.Sprintf("Took over syncing from %s, which stopped without finishing.", strings.Join(took, ", "))
					c.mu.Unlock()
				}
				l.renewed(wall, mono)
				return l, nil
			}
			l.release(rctx)
			waiting = nil
			for i, cl := range fresh {
				if cl.rec.State == claimHeld {
					return nil, busy(cl)
				}
				waiting = &fresh[i]
			}
		}
		if attempt >= leaseTries {
			return nil, busy(*waiting)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(leaseJitter()):
		}
	}
}

// leaseWriteErr explains a failed write of a claim.
func (c *Core) leaseWriteErr(err error) error {
	if isFull(err) {
		return fmt.Errorf("the primary's cloud is full, so this device can't take the sync lock; free up space there to sync (%w)", err)
	}
	return fmt.Errorf("write the sync lock: %w", err)
}

// release marks the claim released, best effort.
func (l *lease) release(ctx context.Context) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.rec.State == claimReleased {
		return
	}
	l.rec.State, l.rec.Seq = claimReleased, l.rec.Seq+1
	if err := l.put(ctx); err != nil {
		fs.Errorf(l.f, "release the sync lock: %v", err)
	}
}

// renew renews the lease when it is due. An error means the lease is
// lost and the work under it must stop. A renewal stuck on the network
// gives up when the lease would lapse.
func (l *lease) renew(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.valid(); err != nil {
		return err
	}
	since := l.since()
	if since < leaseRenew {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, leaseLapse-since)
	defer cancel()
	wall, mono := leaseClock(), time.Now()
	b, err := readSmall(ctx, l.f, leaseFile(l.rec.Device))
	if absent(err) {
		return fmt.Errorf("%w: its claim was removed", errLeaseLost)
	}
	if err != nil {
		fs.Errorf(l.f, "renew the sync lock: %v", err)
		return l.valid() // try again at the next tick, until it lapses
	}
	var rec leaseRecord
	if err := json.Unmarshal(b, &rec); err != nil || rec.Token != l.rec.Token || rec.State != claimHeld {
		return fmt.Errorf("%w: another device took it over", errLeaseLost)
	}
	l.rec.Seq, l.rec.Renewed = l.rec.Seq+1, wall.UTC()
	if err := l.put(ctx); err != nil {
		if isFull(err) {
			return fmt.Errorf("%w: %v", errLeaseLost, l.c.leaseWriteErr(err))
		}
		fs.Errorf(l.f, "renew the sync lock: %v", err)
		return l.valid()
	}
	fresh, _, err := l.c.others(ctx, l.f, l.rec.Device)
	if err != nil {
		fs.Errorf(l.f, "renew the sync lock: %v", err)
		return l.valid()
	}
	for _, cl := range fresh {
		if cl.rec.State == claimHeld && !cl.unreadable {
			return fmt.Errorf("%w: %s holds it too", errLeaseLost, cmp.Or(cl.rec.Name, "another device"))
		}
	}
	l.renewed(wall, mono)
	return nil
}

// withLease runs work while holding the lease on f. If the lease is
// lost, work's context is cancelled and the loss is returned. The claim
// is released when work ends, however it ends.
func (c *Core) withLease(ctx context.Context, f fs.Fs, work func(context.Context) error) error {
	l, err := c.acquire(ctx, f)
	if err != nil {
		return err
	}
	held, _ := ctx.Value(leasesKey{}).([]*lease)
	wctx, cancel := context.WithCancelCause(context.WithValue(ctx, leasesKey{}, append(held[:len(held):len(held)], l)))
	done := make(chan struct{})
	// Each tick checks the lease apart from the renewal, which runs on
	// its own and may be stuck on the network: a lapse, such as after a
	// long sleep, cancels the work within a tick.
	go func() {
		defer close(done)
		t := time.NewTicker(leaseTick)
		defer t.Stop()
		renewed := make(chan error, 1)
		renewing := false
		stop := func(err error) {
			l.mu.Lock() // waits for a renewal in progress, which the cancel ends
			l.lost = cmp.Or(l.lost, err)
			l.mu.Unlock()
			cancel(err)
		}
		defer func() {
			if renewing {
				<-renewed
			}
		}()
		for {
			select {
			case <-wctx.Done():
				return
			case err := <-renewed:
				renewing = false
				if err != nil && wctx.Err() == nil {
					stop(err)
					return
				}
			case <-t.C:
				if err := l.valid(); err != nil {
					cancel(err)
					stop(err)
					return
				}
				if !renewing {
					renewing = true
					go func() { renewed <- l.renew(wctx) }()
				}
			}
		}
	}()
	err = work(wctx)
	cancel(nil)
	<-done
	if verr := l.valid(); verr != nil {
		l.mu.Lock()
		l.lost = cmp.Or(l.lost, verr)
		l.mu.Unlock()
	}
	// Released even when lost: another device that took over has already
	// marked it released, and after a lapse this frees the lock at once.
	rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer rcancel()
	l.release(rctx)
	l.mu.Lock()
	lost := l.lost
	l.mu.Unlock()
	if lost != nil {
		fs.Errorf(f, "sync stopped: %v", lost)
		return lost
	}
	return err
}
