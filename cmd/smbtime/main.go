// smbtime measures how fast a music folder on an SMB share is to list,
// index and play, to decide which parts of an SMB source need a helper
// on the server (docs/smb.md, section 4, step 0).
//
// It never writes to the share. The share is opened through
// internal/readonly, which refuses every write before it reaches the
// server, and the tool itself only lists folders and reads byte ranges.
// The server's disk does record when files and folders were last read
// (their access times), as for any reader. On this computer the report
// goes to the terminal; rclone's config package creates an empty
// ~/.config/rclone folder if there is none, and nothing else is
// written. Connect with an SMB user that has read-only access to the
// share, so that the server refuses writes too.
//
//	go run ./cmd/smbtime -host studio -user medialayer-ro -path Music/Library
//
// The password is asked for on the terminal (not shown as it is typed),
// or read from the macOS keychain with -keychain (macOS asks whether to
// allow it), or taken from SMB_PASSWORD.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rclone/rclone/backend/smb"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/obscure"

	"github.com/stamler/medialayer/internal/media"
	"github.com/stamler/medialayer/internal/readonly"
)

func main() {
	host := flag.String("host", "", "the SMB server, as this computer reaches it (a name, a tailnet name or an address)")
	user := flag.String("user", "", "the SMB user name (best one with read-only access)")
	port := flag.Int("port", 445, "the SMB port")
	domain := flag.String("domain", "", "the Windows domain, if any")
	dir := flag.String("path", "", "the share and folder, such as Music/Library")
	keychain := flag.Bool("keychain", false, "read the password from the macOS keychain (Finder's saved password for the server)")
	sample := flag.Int("sample", 200, "how many tracks to read the headers of")
	starts := flag.Int("starts", 10, "how many track starts to time")
	workers := flag.Int("workers", 8, "files read at once, as the indexer does")
	checkers := flag.Int("checkers", 8, "folders listed at once, as rclone does")
	flag.Parse()
	if *host == "" || *user == "" || *dir == "" {
		flag.Usage()
		os.Exit(2)
	}
	pass, err := password(*host, *user, *keychain)
	if err != nil {
		fail(err)
	}
	ctx := context.Background()
	ci := fs.GetConfig(ctx)
	ci.Checkers = *checkers
	ci.LogLevel = fs.LogLevelNotice
	// One logon try: a wrong password retried counts as several failed
	// logons, which a server may lock the account for.
	ci.LowLevelRetries = 1

	fmt.Printf("smbtime: %s on %s, as %s (read-only)\n\n", *dir, *host, *user)
	start := time.Now()
	m := configmap.Simple{
		"host": *host, "user": *user, "port": strconv.Itoa(*port),
		"pass": obscure.MustObscure(pass),
	}
	if *domain != "" {
		m["domain"] = *domain
	}
	// rclone's defaults for everything else (domain, encoding, case),
	// as a configured remote would have.
	ri, err := fs.Find("smb")
	if err != nil {
		fail(err)
	}
	for _, o := range ri.Options {
		if _, ok := m[o.Name]; !ok && o.Default != nil {
			m[o.Name] = fmt.Sprint(o.Default)
		}
	}
	inner, err := smb.NewFs(ctx, "smbtime", strings.Trim(*dir, "/"), m)
	if err != nil {
		fail(fmt.Errorf("connect: %w", err))
	}
	f := readonly.Wrap(inner)
	defer func() { _ = f.Shutdown(ctx) }()
	report("Connect", time.Since(start), "")

	// Listing, as every sync does: folders listed several at once. A
	// folder that can't be read (its permissions keep this user out) is
	// counted and skipped rather than stopping the test.
	start = time.Now()
	files, dirs, unreadable := list(ctx, f, *checkers)
	var bytes int64
	for _, o := range files {
		bytes += o.Size()
	}
	listTime := time.Since(start)
	var tracks []fs.Object
	var trackBytes int64
	for _, o := range files {
		if format(o.Remote()) != "" {
			tracks = append(tracks, o)
			trackBytes += o.Size()
		}
	}
	report("List everything", listTime, fmt.Sprintf("%d folders, %d files (%d tracks), %s; %v a folder, %d at once",
		dirs, len(files), len(tracks), size(bytes), per(listTime, max(dirs, 1)), *checkers))
	if len(unreadable) > 0 {
		fmt.Printf("    %d folders couldn't be read and were skipped, such as %s\n", len(unreadable), unreadable[0])
	}
	if len(tracks) == 0 {
		fmt.Println("\nNo tracks found; nothing more to time.")
		return
	}
	sort.Slice(tracks, func(i, j int) bool { return tracks[i].Remote() < tracks[j].Remote() })

	// Looking up a file (an open and a close, two round trips), as sync
	// would before a removal. Files here, and the track starts below, are
	// picked away from the headers sampled, so the server hasn't just read
	// them; a second run of the test finds them in the server's memory and
	// is faster than a first sync would be.
	var trips []time.Duration
	for i := range 10 {
		o := tracks[(i*len(tracks)+len(tracks)/3)/10%len(tracks)]
		t0 := time.Now()
		if _, err := f.NewObject(ctx, o.Remote()); err != nil {
			fail(fmt.Errorf("look up %s: %w", o.Remote(), err))
		}
		trips = append(trips, time.Since(t0))
	}
	report("Look up one file", median(trips), "median of 10")

	// Headers, as indexing reads them, spread over the library.
	picked := spread(tracks, *sample)
	type result struct {
		took          time.Duration
		reads         int
		read          int64
		err           error
		remote, label string
	}
	results := make([]result, len(picked))
	start = time.Now()
	var wg sync.WaitGroup
	sem := make(chan struct{}, max(*workers, 1))
	for i, o := range picked {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			r := result{remote: o.Remote()}
			t0 := time.Now()
			r.reads, r.read, r.err = parse(ctx, o)
			r.took = time.Since(t0)
			results[i] = r
		}()
	}
	wg.Wait()
	headerTime := time.Since(start)
	var each []time.Duration
	reads, failed := 0, 0
	var read int64
	for _, r := range results {
		each = append(each, r.took)
		reads += r.reads
		read += r.read
		if r.err != nil {
			failed++
			if failed <= 5 {
				fmt.Printf("    couldn't read %s: %v\n", r.remote, r.err)
			}
		}
	}
	rate := float64(len(picked)) / headerTime.Seconds()
	report("Read track headers", headerTime, fmt.Sprintf("%d tracks, %d at once: %.1f a second, %v each (median), %.1f reads (each an open, a read and a close) and %s a track, %d failed",
		len(picked), *workers, rate, round(median(each)), float64(reads)/float64(len(picked)), size(read/int64(len(picked))), failed))
	report("  so indexing all tracks", time.Duration(float64(len(tracks))/rate*float64(time.Second)), "estimated, from scratch")

	// Track starts: the first 256 KB, as a player buffers before playing.
	var startTimes []time.Duration
	for _, o := range spreadAt(tracks, *starts, 0.5) {
		t0 := time.Now()
		if _, err := readRange(ctx, o, 0, 256<<10); err != nil {
			fail(fmt.Errorf("read %s: %w", o.Remote(), err))
		}
		startTimes = append(startTimes, time.Since(t0))
	}
	report("Start a track", median(startTimes), fmt.Sprintf("first 256 KB, median of %d", len(startTimes)))

	// Throughput, reading up to 64 MB of the largest sampled track, to
	// estimate the audio hash pass, which reads every track in full.
	big := picked[0]
	for _, o := range picked {
		if o.Size() > big.Size() {
			big = o
		}
	}
	n := min(big.Size(), 64<<20)
	if n == 0 {
		fmt.Println("\nThe sampled tracks are empty; no read speed to measure.")
		return
	}
	t0 := time.Now()
	if _, err := readRange(ctx, big, 0, n); err != nil {
		fail(fmt.Errorf("read %s: %w", big.Remote(), err))
	}
	took := time.Since(t0)
	mbs := float64(n) / (1 << 20) / took.Seconds()
	report("Read one track in full", took, fmt.Sprintf("%s at %.1f MB/s", size(n), mbs))
	report("  so hashing every track", time.Duration(float64(trackBytes)/(1<<20)/mbs*float64(time.Second)),
		fmt.Sprintf("estimated, %s of tracks, one at a time", size(trackBytes)))
	fmt.Println("\nNothing was written to the share (its disk may record when files were last read).")
}

// list lists every folder under the top of f, checkers at a time, and
// returns the files, how many folders it listed, and the folders it
// couldn't list.
func list(ctx context.Context, f fs.Fs, checkers int) (files []fs.Object, dirs int, unreadable []string) {
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, max(checkers, 1))
	var visit func(dir string)
	visit = func(dir string) {
		defer wg.Done()
		sem <- struct{}{}
		entries, err := f.List(ctx, dir)
		<-sem
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			unreadable = append(unreadable, fmt.Sprintf("%q (%v)", dir, err))
			return
		}
		dirs++
		for _, e := range entries {
			switch e := e.(type) {
			case fs.Object:
				files = append(files, e)
			case fs.Directory:
				wg.Add(1)
				go visit(e.Remote())
			}
		}
	}
	wg.Add(1)
	visit("")
	wg.Wait()
	return files, dirs, unreadable
}

// password gets the SMB password: from SMB_PASSWORD, the keychain or the
// terminal.
func password(host, user string, keychain bool) (string, error) {
	if p := os.Getenv("SMB_PASSWORD"); p != "" {
		return p, nil
	}
	if keychain {
		out, err := exec.Command("security", "find-internet-password", "-s", host, "-a", user, "-w").Output()
		if err != nil {
			return "", fmt.Errorf("no password for %s on %s in the keychain (or access was denied): %w", user, host, err)
		}
		return strings.TrimRight(string(out), "\n"), nil
	}
	fmt.Fprintf(os.Stderr, "Password for %s on %s: ", user, host)
	p := config.ReadPassword()
	fmt.Fprintln(os.Stderr)
	return p, nil
}

// format is the indexer's media format for a path (core's mediaFormat).
func format(p string) string {
	switch strings.ToLower(path.Ext(p)) {
	case ".flac":
		return "flac"
	case ".mp3":
		return "mp3"
	case ".m4a", ".mp4", ".m4b":
		return "m4a"
	case ".caf":
		return "caf"
	case ".oga", ".ogg":
		return "oga"
	}
	return ""
}

// parse reads o's header as the indexer does, counting the reads.
func parse(ctx context.Context, o fs.Object) (reads int, read int64, err error) {
	const maxHeader = 16 << 20 // as the indexer
	fetch := func(offset, length int64) ([]byte, error) {
		if length <= 0 {
			return nil, nil
		}
		b, err := readRange(ctx, o, offset, length)
		reads++
		read += int64(len(b))
		return b, err
	}
	switch format(o.Remote()) {
	case "flac":
		var h []byte
		if h, _, err = media.FLACHeader(fetch, o.Size(), maxHeader); err == nil {
			_, err = media.ParseFLAC(h)
		}
	case "mp3":
		_, err = media.ParseMP3(fetch, o.Size())
	case "m4a":
		_, err = media.ParseMP4(fetch, o.Size())
	case "caf":
		_, err = media.ParseCAF(fetch, o.Size())
	case "oga":
		_, err = media.ParseOggFLAC(fetch, o.Size(), maxHeader)
	}
	return reads, read, err
}

// readRange reads length bytes of o from offset (fewer at its end), in
// as few reads as the backend allows: one buffer of the whole length,
// where io.ReadAll's small, growing buffer costs a round trip each.
func readRange(ctx context.Context, o fs.Object, offset, length int64) ([]byte, error) {
	rc, err := o.Open(ctx, &fs.RangeOption{Start: offset, End: offset + length - 1})
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	b := make([]byte, length)
	n, err := io.ReadFull(rc, b)
	if err == io.ErrUnexpectedEOF || err == io.EOF {
		err = nil
	}
	return b[:n], err
}

// spread picks n of objs, evenly spaced, so every part of the library is
// sampled.
func spread(objs []fs.Object, n int) []fs.Object { return spreadAt(objs, n, 0) }

// spreadAt is spread starting phase (0 to 1) of a step in, so samples
// taken at different phases are different files.
func spreadAt(objs []fs.Object, n int, phase float64) []fs.Object {
	if n >= len(objs) {
		return objs
	}
	out := make([]fs.Object, 0, n)
	step := float64(len(objs)) / float64(n)
	for i := range n {
		out = append(out, objs[int((float64(i)+phase)*step)%len(objs)])
	}
	return out
}

func median(d []time.Duration) time.Duration {
	s := append([]time.Duration(nil), d...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[len(s)/2]
}

func per(d time.Duration, n int) time.Duration {
	return round(d / time.Duration(n))
}

// round keeps a duration to three figures or so.
func round(d time.Duration) time.Duration {
	if d < 10*time.Millisecond {
		return d.Round(10 * time.Microsecond)
	}
	return d.Round(time.Millisecond)
}

func size(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
}

func report(what string, d time.Duration, detail string) {
	r := round(d)
	if d > time.Minute {
		r = d.Round(time.Second)
	}
	fmt.Printf("%-26s %10v   %s\n", what, r, detail)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "smbtime:", err)
	os.Exit(1)
}
