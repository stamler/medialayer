package core

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/config/configfile"
)

// The core's rclone config holds every cloud's sign-in: Google's tokens,
// and Proton's session, which can unlock the account's keys. With a
// config key, which the app keeps in the system keychain (as Proton's own
// apps keep their sessions), the file is encrypted with rclone's config
// encryption, so the file alone opens nothing.

// encryptedHeader is the first line of an encrypted rclone config.
const encryptedHeader = "RCLONE_ENCRYPT_V0:"

// setupConfig installs the rclone config at path, encrypted with key
// when key isn't "". A plain config from before is encrypted now. A
// config that can't be read with key (the key was lost or replaced) is
// set aside, as rclone stops the process on a config it can't read; the
// returned notice then says the clouds need signing in again.
func setupConfig(path, key string) (notice string, err error) {
	// rclone would otherwise ask for the config password on the
	// terminal.
	fs.GetConfig(context.Background()).AskPassword = false
	config.ClearConfigPassword()
	if err := config.SetConfigPath(path); err != nil {
		return "", err
	}
	if key != "" {
		if err := config.SetConfigPassword(key); err != nil {
			return "", fmt.Errorf("config key: %w", err)
		}
	}
	// An error opening the file is returned, so the app says so and the
	// user can try again: only a file that opens but can't be read as a
	// config is set aside.
	encrypted, err := isEncryptedConfig(path)
	if err != nil {
		return "", err
	}
	{
		// A failed decrypt clears the key, so it is set again after.
		probe := &configfile.Storage{}
		if err := probe.Load(); err != nil && !errors.Is(err, config.ErrorConfigFileNotFound) {
			aside := path + ".unreadable-" + time.Now().UTC().Format(trashStamp)
			if err := os.Rename(path, aside); err != nil {
				return "", fmt.Errorf("set aside the unreadable %s: %w", path, err)
			}
			fs.Errorf(nil, "couldn't read %s (%v); moved it to %s", path, err, aside)
			notice = "MediaLayer couldn't unlock its saved sign-ins, so its clouds need connecting again."
			encrypted = false
		}
		config.ClearConfigPassword()
		if key != "" {
			if err := config.SetConfigPassword(key); err != nil {
				return "", fmt.Errorf("config key: %w", err)
			}
		}
	}
	configfile.Install()
	if key != "" && !encrypted && fileSize(path) > 0 {
		config.SaveConfig() // encrypts the plain config from before
		if enc, err := isEncryptedConfig(path); err != nil || !enc {
			return notice, fmt.Errorf("couldn't encrypt %s", path)
		}
		// rclone keeps a plain backup when a save fails partway.
		old, _ := filepath.Glob(path + ".old*")
		for _, p := range old {
			if err := os.Remove(p); err != nil {
				fs.Errorf(nil, "remove the plain %s: %v", p, err)
			}
		}
	}
	// rclone reloads the file when it changes, and a file it can't then
	// decrypt clears the key, so its next save would be plain. Nothing but
	// this core writes the file.
	return notice, nil
}

// isEncryptedConfig reports whether the rclone config at path is
// encrypted; a missing file isn't.
func isEncryptedConfig(path string) (bool, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if l == "" || strings.HasPrefix(l, ";") || strings.HasPrefix(l, "#") {
			continue
		}
		return l == encryptedHeader, nil
	}
	return false, sc.Err()
}
