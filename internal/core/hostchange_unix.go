//go:build linux || darwin

package core

import (
	"fmt"
	"os"
	"syscall"
)

// fileChange names a file's identity and last change on disk, its inode
// and change time, which any write or rename updates even when the size
// and modification time are put back. (Not the device number, which a
// NAS's disks can change at a reboot.) "" if it can't be read.
func fileChange(p string) string {
	st, err := os.Stat(p)
	if err != nil {
		return ""
	}
	s, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	sec, nsec := statCtime(s)
	return fmt.Sprintf("%d:%d.%09d", s.Ino, sec, nsec)
}
