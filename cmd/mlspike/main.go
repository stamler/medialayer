// Command mlspike is rclone with the medialayer overlay backend built in.
//
// Every rclone command works (nfsmount, mount, serve, backend, cat ...), and
// remotes of type "medialayer" can wrap any other remote. See docs/spike.md.
package main

import (
	_ "github.com/rclone/rclone/backend/all" // all rclone backends
	"github.com/rclone/rclone/cmd"
	_ "github.com/rclone/rclone/cmd/all"    // all rclone commands
	_ "github.com/rclone/rclone/lib/plugin" // rclone plugins

	_ "github.com/stamler/medialayer/backend/medialayer" // the overlay
)

func main() {
	cmd.Main()
}
