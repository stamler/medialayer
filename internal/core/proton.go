package core

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/rclone/Proton-API-Bridge/common"
	_ "github.com/rclone/rclone/backend/protondrive" // Proton Drive
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/obscure"
)

// Proton Drive is a secondary only, for now (see docs/sync.md, Proton
// Drive). Signing in goes through rclone's backend, with the account's
// username and password, a 2FA code if the account uses one, and the
// mailbox password of a two-password account. Only the session the
// sign-in makes is kept in the core's rclone config, never the password
// (the session's salted key password does unlock the account's keys):
// when Proton ends the session, the user signs in again.
//
// MediaLayer connects one Proton account at a time. rclone's backend
// keeps the config a renewed session is saved to in a package variable,
// set by whichever Proton remote was opened last, so with two accounts
// open at once (a sync to one while browsing the other) one account's
// session could be saved under the other's remote.

const (
	protonType   = "protondrive" // rclone's backend
	protonRemote = "proton"      // the remote the core creates
)

// protonSessionKeys are the backend's config keys that hold a session.
var protonSessionKeys = []string{"client_uid", "client_access_token", "client_refresh_token", "client_salted_key_pass"}

// Errors from a Proton sign-in that the app answers by asking for more.
var (
	ErrProtonNeeds2FA     = errors.New("this Proton account uses two-factor authentication: enter the code from your authenticator app")
	ErrProtonNeedsMailbox = errors.New("this Proton account has a second password for its mailbox: enter it too")
)

// ProtonAccount is a connected Proton account.
type ProtonAccount struct {
	Remote string `json:"remote"`
	User   string `json:"user"`
	// SignedIn is false when the config holds no session. The core keeps
	// a session even after Proton ends it (see openProton), so a session
	// Proton ended still shows as signed in, and opening its folders
	// fails with a message to sign in again.
	SignedIn bool `json:"signed_in"`
}

// protonAccounts lists the Proton remotes in the core's config.
func protonAccounts() []ProtonAccount {
	out := []ProtonAccount{}
	for _, name := range config.FileSections() {
		if config.GetValue(name, "type") == protonType {
			out = append(out, ProtonAccount{Remote: name, User: config.GetValue(name, "username"), SignedIn: protonSignedIn(name)})
		}
	}
	return out
}

// protonSignedIn reports whether a Proton remote holds a session.
func protonSignedIn(name string) bool {
	for _, k := range protonSessionKeys {
		if config.GetValue(name, k) == "" {
			return false
		}
	}
	return true
}

// remoteInUse reports whether the primary or a secondary is on remote.
// The caller holds c.mu.
func (c *Core) remoteInUse(remote string) bool {
	if c.settings.Folder != "" && c.primaryRemote() == remote {
		return true
	}
	for _, s := range c.settings.Secondaries {
		if s.Remote == remote {
			return true
		}
	}
	return false
}

// ConnectProton signs in to Proton Drive and returns the remote's name.
// code is the 2FA code, and mailbox the mailbox password; either may be
// empty, and the error is ErrProtonNeeds2FA or ErrProtonNeedsMailbox when
// the account needs it. Signing in to the connected account again renews
// its session; another account replaces it, unless the library uses it.
func (c *Core) ConnectProton(ctx context.Context, user, password, mailbox, code string) (string, error) {
	user = strings.TrimSpace(user)
	if user == "" || password == "" {
		return "", errors.New("enter your Proton username and password")
	}
	name := protonRemote
	if accounts := protonAccounts(); len(accounts) > 0 {
		a := accounts[0]
		name = a.Remote
		c.mu.Lock()
		inUse := c.remoteInUse(a.Remote)
		c.mu.Unlock()
		if !sameProtonUser(a.User, user) && inUse {
			return "", fmt.Errorf("MediaLayer connects one Proton account at a time, and the library uses %s. Stop syncing to its folders first", a.User)
		}
	}
	// The sign-in runs on a config of its own, so a failed one leaves the
	// connected account as it was.
	ri, err := fs.Find(protonType)
	if err != nil {
		return "", err
	}
	values := configmap.Simple{"username": user, "password": obscure.MustObscure(password)}
	if mailbox != "" {
		values["mailbox_password"] = obscure.MustObscure(mailbox)
	}
	if code = strings.ReplaceAll(strings.TrimSpace(code), " ", ""); code != "" {
		values["2fa"] = code
	}
	m := fs.ConfigMap(ri.Prefix, ri.Options, "", values)
	m.ClearSetters()
	m.AddSetter(values) // the session the sign-in makes lands here
	if _, err := ri.NewFs(ctx, name, "", m); err != nil {
		switch {
		case errors.Is(err, common.Err2FACodeRequired):
			return "", ErrProtonNeeds2FA
		case errors.Is(err, common.ErrMailboxPasswordRequired):
			return "", ErrProtonNeedsMailbox
		}
		return "", fmt.Errorf("Proton sign-in: %w", err)
	}
	// The library may have started using the account during the sign-in.
	c.mu.Lock()
	if old := config.GetValue(name, "username"); old != "" && !sameProtonUser(old, user) && c.remoteInUse(name) {
		c.mu.Unlock()
		return "", fmt.Errorf("MediaLayer connects one Proton account at a time, and the library uses %s. Stop syncing to its folders first", old)
	}
	session := map[string]string{}
	for _, k := range protonSessionKeys {
		if session[k] = values[k]; session[k] == "" {
			c.mu.Unlock()
			return "", errors.New("Proton sign-in did not finish")
		}
	}
	// A new section, so no password or code from an earlier config stays.
	config.LoadedData().DeleteSection(name)
	config.FileSetValue(name, "type", protonType)
	config.FileSetValue(name, "username", user)
	// MediaLayer is the only writer of its folders, so an upload left
	// unfinished (a draft, to Proton) is its own, and is replaced.
	config.FileSetValue(name, "replace_existing_draft", "true")
	for k, v := range session {
		config.FileSetValue(name, k, v)
	}
	c.mu.Unlock()
	config.SaveConfig() // may retry for a while: not under the lock
	return name, nil
}

// openProton opens folder on a Proton remote. The backend clears the
// saved session whenever opening with it fails, offline or cancelled as
// much as when Proton has ended it, and then signs in with the password,
// which the core doesn't keep. So the session is never cleared here: a
// failed open fails again until the user signs in again, and works again
// once Proton can be reached.
func openProton(ctx context.Context, remote, folder string) (fs.Fs, error) {
	ri, err := fs.Find(protonType)
	if err != nil {
		return nil, err
	}
	m := fs.ConfigMap(ri.Prefix, ri.Options, remote, nil)
	m.ClearSetters()
	m.AddSetter(keepSession(remote))
	f, err := ri.NewFs(ctx, remote, folder, m)
	if errors.Is(err, fs.ErrorIsFile) {
		return nil, fmt.Errorf("%s is a file, not a folder", folder)
	}
	if err != nil {
		return nil, protonError(err)
	}
	return f, nil
}

// keepSession saves what the Proton backend sets in a remote's config
// (a renewed session), except clearing the session.
type keepSession string

func (k keepSession) Set(key, value string) {
	if value == "" {
		for _, s := range protonSessionKeys {
			if key == s {
				return
			}
		}
	}
	if !fs.ConfigFileHasSection(string(k)) {
		return // the account was replaced or removed meanwhile
	}
	if err := fs.ConfigFileSet(string(k), key, value); err != nil {
		fs.Errorf(nil, "save Proton session: %v", err)
	}
}

// sameProtonUser reports whether two usernames name one account: Proton
// takes the name with or without its address's domain.
func sameProtonUser(a, b string) bool {
	if strings.EqualFold(a, b) {
		return true
	}
	// A bare name matches only an address on Proton's own domains: an
	// account whose login is an outside address is another account.
	proton := func(domain string) bool {
		switch strings.ToLower(domain) {
		case "proton.me", "protonmail.com", "protonmail.ch", "pm.me":
			return true
		}
		return false
	}
	la, doma, hasA := strings.Cut(a, "@")
	lb, domb, hasB := strings.Cut(b, "@")
	if hasA == hasB || !strings.EqualFold(la, lb) {
		return false
	}
	return proton(doma) || proton(domb)
}

// protonError explains an error opening a Proton remote. Without a
// session the backend falls back to signing in with a password, which
// the core doesn't keep.
func protonError(err error) error {
	if errors.Is(err, common.ErrUsernameAndPasswordRequired) || errors.Is(err, common.Err2FACodeRequired) ||
		errors.Is(err, common.ErrMailboxPasswordRequired) {
		return errors.New("couldn't open Proton Drive with the saved sign-in: MediaLayer may be offline, or Proton ended the session. If this keeps happening, sign in again from Add Source, Proton Drive")
	}
	return err
}
