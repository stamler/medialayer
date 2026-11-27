package core

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/rclone/rclone/fs/fshttp"
)

func TestThrottlesCounted(t *testing.T) {
	watchThrottles()
	fshttp.SetFaultInjector(func(req *http.Request) (int, error) {
		switch req.URL.Host {
		case "drive-api.proton.me":
			return http.StatusTooManyRequests, nil
		case "www.googleapis.com":
			return http.StatusServiceUnavailable, nil
		}
		return http.StatusNotFound, nil // never sent
	})
	defer fshttp.SetFaultInjector(nil)
	checkThrottles(time.Now()) // counts from earlier runs (-count)
	before := map[string]int64{}
	for _, th := range recentThrottles(time.Now()) {
		before[th.Service] = th.Count
	}
	client := fshttp.NewClient(context.Background())
	for _, u := range []string{"https://drive-api.proton.me/a", "https://drive-api.proton.me/b",
		"https://www.googleapis.com/drive", "https://example.com/missing"} { // 404 isn't throttling
		res, err := client.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
	}
	now := time.Now()
	checkThrottles(now)
	got := map[string]int64{}
	for _, th := range recentThrottles(now) {
		got[th.Service] = th.Count - before[th.Service]
	}
	if got["Proton"] != 2 || got["Google Drive"] != 1 || len(got) != 2 {
		t.Fatalf("throttles = %v, want Proton 2 and Google Drive 1", got)
	}
	if ts := recentThrottles(now.Add(throttleRecent + time.Second)); len(ts) != 0 {
		t.Fatalf("throttles two minutes on = %v, want none", ts)
	}
}
