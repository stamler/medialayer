// mlhost is MediaLayer's host helper. It runs on the machine that serves
// a library over SMB, reads the library from its own disk, and keeps an
// inventory of the tags, art and audio hashes in the library's
// .medialayer/host, so a device indexing the share over the network
// needn't read every file's header, or the whole library for its audio
// hashes. It writes nothing else, and never changes a music file. See
// docs/smb.md, "The host helper".
//
//	mlhost -folder /srv/media/music            # keep it current
//	mlhost -folder /srv/media/music -once      # scan and hash once, then exit
//	mlhost -folder /srv/media/music -systemd   # print a systemd unit for it
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	_ "github.com/rclone/rclone/backend/local"
	"github.com/rclone/rclone/fs"

	"github.com/stamler/medialayer/internal/core"
)

const version = "0.1.0"

func main() {
	folder := flag.String("folder", "", "the library folder on this machine (the folder a device uses over SMB)")
	interval := flag.Duration("interval", 10*time.Minute, "how often to scan the folder again")
	once := flag.Bool("once", false, "scan and hash everything once, then exit")
	systemd := flag.Bool("systemd", false, "print a systemd unit that runs mlhost for -folder, and exit")
	unitUser := flag.String("user", "", "with -systemd: the user the unit runs mlhost as (default: you)")
	allowRoot := flag.Bool("allow-root", false, "run as root anyway")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("mlhost", version)
		return
	}
	if *folder == "" {
		fmt.Fprintln(os.Stderr, "mlhost: -folder is required")
		flag.Usage()
		os.Exit(2)
	}
	if *systemd {
		printUnit(*folder, *interval, *unitUser)
		return
	}
	// Root can write anything, and the folder's .medialayer is one the
	// share's users can write too: run as the user who owns the music.
	if os.Geteuid() == 0 && !*allowRoot {
		log.Fatal("mlhost: run it as the user who owns the music folder, not root (or pass -allow-root)")
	}
	fs.GetConfig(context.Background()).LogLevel = fs.LogLevelNotice // rclone's own chatter off
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	opt := core.HostOptions{Folder: *folder, Interval: *interval, Version: version}
	if *once {
		opt.Interval = 0
	}
	log.SetFlags(0)
	log.Printf("mlhost %s: %s", version, *folder)
	if err := core.RunHost(ctx, opt, log.Printf); err != nil && ctx.Err() == nil {
		log.Fatalf("mlhost: %v", err)
	}
}

// printUnit prints a systemd unit running mlhost for folder as name
// (default: the user running this, never root unless asked), at low CPU
// and disk priority.
func printUnit(folder string, interval time.Duration, name string) {
	abs, err := filepath.Abs(folder)
	if err != nil {
		log.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		log.Fatal(err)
	}
	if name == "" {
		u, err := user.Current()
		if err != nil || u.Uid == "0" {
			log.Fatal("mlhost: pass -user with the user who owns the music folder (run under sudo, this would be root)")
		}
		name = u.Username
	}
	rootFlag := ""
	if name == "root" {
		rootFlag = " -allow-root" // asked for by name
	}
	fmt.Printf(`[Unit]
Description=MediaLayer host helper
After=local-fs.target

[Service]
User=%s
ExecStart=%s -folder %s -interval %s%s
Restart=on-failure
RestartSec=60
Nice=10
IOSchedulingClass=idle

[Install]
WantedBy=multi-user.target
`, unitEscape(name), unitQuote(exe), unitQuote(abs), interval, rootFlag)
}

// unitEscape escapes systemd's specifiers (%) and variables ($).
func unitEscape(s string) string {
	return strings.NewReplacer("%", "%%", "$", "$$").Replace(s)
}

// unitQuote quotes a path as one word of an ExecStart line.
func unitQuote(s string) string {
	return `"` + unitEscape(strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s)) + `"`
}
