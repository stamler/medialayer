package core

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/rclone/rclone/fs"
)

// The log goes to stderr and to a file, as an app opened from Finder has
// no stderr anyone reads. Debug logging, on until turned off in the
// Library menu, adds rclone's info lines and MediaLayer's own timings and
// security key steps (never a PIN). Throttling is logged either way.
var logging struct {
	once  sync.Once
	level slog.LevelVar
	path  string
}

// maxLogSize is how big the log grows before it starts again, keeping
// the last one as MediaLayer.log.1.
const maxLogSize = 10 << 20

func logPath(dataDir string) string {
	// Tests keep to their own folder, out of the app's log.
	if home, err := os.UserHomeDir(); err == nil && runtime.GOOS == "darwin" && !testing.Testing() {
		return filepath.Join(home, "Library", "Logs", "MediaLayer.log")
	}
	return filepath.Join(dataDir, "MediaLayer.log")
}

// setupLogging opens the log file once and sets the level.
func setupLogging(dataDir string, debug bool) {
	logging.once.Do(func() {
		var w io.Writer = os.Stderr
		p := logPath(dataDir)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err == nil {
			if st, err := os.Stat(p); err == nil && st.Size() > maxLogSize {
				_ = os.Rename(p, p+".1")
			}
			if f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
				w, logging.path = io.MultiWriter(os.Stderr, f), p
			}
		}
		fs.SetLogger(slog.NewTextHandler(w, &slog.HandlerOptions{Level: &logging.level, ReplaceAttr: levelName}))
	})
	level := fs.LogLevelNotice
	if debug {
		level = fs.LogLevelInfo
	}
	fs.GetConfig(context.Background()).LogLevel = level
	logging.level.Set(fs.LogLevelToSlog(level))
}

// SetDebug turns debug logging on or off and remembers it.
func (c *Core) SetDebug(on bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	was := c.settings.DebugOff
	c.settings.DebugOff = !on
	if err := c.saveSettings(); err != nil {
		c.settings.DebugOff = was
		return err
	}
	setupLogging(c.dataDir, on)
	fs.Logf(nil, "debug logging %s", map[bool]string{true: "on", false: "off"}[on])
	return nil
}

// levelName names rclone's levels as rclone does (NOTICE, not INFO+2).
func levelName(groups []string, a slog.Attr) slog.Attr {
	if a.Key == slog.LevelKey && len(groups) == 0 {
		if l, ok := a.Value.Any().(slog.Level); ok {
			switch {
			case l >= fs.SlogLevelCritical:
				a.Value = slog.StringValue("CRITICAL")
			case l == fs.SlogLevelNotice:
				a.Value = slog.StringValue("NOTICE")
			}
		}
	}
	return a
}
