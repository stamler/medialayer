package core

import (
	"fmt"
	"runtime/debug"

	"github.com/rclone/rclone/fs"
)

// noPanic runs a write to a cloud, turning a panic in the backend into
// an error. go-proton-api v1.0.4's CreateFile and CreateFolder read the
// status of a response that is nil when the request got no answer
// (dropped connection, timeout, cancel), which crashed the app mid-sync.
// The panic happens on the calling goroutine, so it can be caught here
// until that is fixed upstream.
func noPanic(what string, f func() error) (err error) {
	defer func() {
		if p := recover(); p != nil {
			fs.Errorf(nil, "%s: recovered from a crash in the cloud's library: %v\n%s", what, p, debug.Stack())
			err = fmt.Errorf("%s: the cloud's library failed (%v); the next sync tries again", what, p)
		}
	}()
	return f()
}
