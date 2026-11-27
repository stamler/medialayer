package core

import (
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/fshttp"
)

// Throttle is a cloud that asked us to slow down recently: it answered
// with 429 Too Many Requests or 503 Service Unavailable. Both rclone's
// Drive backend and Proton's library wait and retry those, so they show
// up only as a slow sync unless counted here. (Drive also rate limits
// with 403, which can't be told from its other refusals by status alone.)
type Throttle struct {
	Service string    `json:"service"` // "Proton", "Google Drive", or the host
	Count   int64     `json:"count"`   // since the app started
	Last    time.Time `json:"last"`
}

// throttleRecent is how long after the last slow-down reply a cloud
// still shows as throttling.
const throttleRecent = 2 * time.Minute

// Every HTTP response rclone's transports get, Proton's and Drive's
// included, is counted by host and status. This must be set before the
// first transport is made, so it is set as the package loads.
var httpMetrics = func() *fshttp.Metrics {
	m := fshttp.NewMetrics("medialayer")
	fshttp.DefaultMetrics = m
	return m
}()

var throttles struct {
	once sync.Once
	reg  *prometheus.Registry
	mu   sync.Mutex
	seen map[string]int64 // service: slow-down replies counted so far
	last map[string]time.Time
}

// watchThrottles checks the counts every few seconds and logs each
// rise, so the log says when and how often a cloud throttled a sync.
func watchThrottles() {
	throttles.once.Do(func() {
		throttles.reg = prometheus.NewRegistry()
		throttles.reg.MustRegister(httpMetrics.StatusCode)
		throttles.seen = map[string]int64{}
		throttles.last = map[string]time.Time{}
		go func() {
			for range time.Tick(5 * time.Second) {
				checkThrottles(time.Now())
			}
		}()
	})
}

func checkThrottles(now time.Time) {
	counts := throttleCounts()
	throttles.mu.Lock()
	defer throttles.mu.Unlock()
	for svc, n := range counts {
		if d := n - throttles.seen[svc]; d > 0 {
			throttles.seen[svc], throttles.last[svc] = n, now
			fs.Logf(nil, "%s asked us to slow down (%d more replies, %d since the app started); each request is retried after a wait", svc, d, n)
		}
	}
}

// throttleCounts adds up the slow-down replies by cloud.
func throttleCounts() map[string]int64 {
	counts := map[string]int64{}
	mfs, err := throttles.reg.Gather()
	if err != nil {
		return counts
	}
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			var host, code string
			for _, l := range m.GetLabel() {
				switch l.GetName() {
				case "host":
					host = l.GetValue()
				case "code":
					code = l.GetValue()
				}
			}
			svc := throttleService(host)
			if code == "429" || code == "503" {
				counts[svc] += int64(m.GetCounter().GetValue())
			}
		}
	}
	return counts
}

func throttleService(host string) string {
	h := strings.ToLower(host)
	switch {
	case strings.Contains(h, "proton"):
		return "Proton"
	case strings.Contains(h, "google"):
		return "Google Drive"
	}
	return host
}

// recentThrottles is the clouds that throttled in the last two minutes.
func recentThrottles(now time.Time) []Throttle {
	throttles.mu.Lock()
	defer throttles.mu.Unlock()
	var ts []Throttle
	for svc, at := range throttles.last {
		if now.Sub(at) < throttleRecent {
			ts = append(ts, Throttle{Service: svc, Count: throttles.seen[svc], Last: at})
		}
	}
	sort.Slice(ts, func(i, j int) bool { return ts[i].Service < ts[j].Service })
	return ts
}
