package core

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/rclone/Proton-API-Bridge/common"
	"github.com/rclone/go-proton-api"
	_ "github.com/rclone/rclone/backend/protondrive" // Proton Drive
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/fshttp"
)

// A Proton Drive folder can be a secondary, and be made the primary (see
// docs/sync.md, Proton Drive). The core signs in itself (protonLogin),
// with the account's username and password, a 2FA code or security key if
// the account uses one, and the mailbox password of a two-password account, and
// hands the session to rclone's backend. Only the session the
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
// the account needs it. pin is the security key's PIN, which the key
// alone sees; the error is ErrProtonNeedsPIN when the key wants it. With
// no code, an account with a security key signs
// in with the key plugged in (ProtonKeyWaiting says when it waits for
// a touch). Signing in to the connected account again renews
// its session; another account replaces it, unless the library uses it.
func (c *Core) ConnectProton(ctx context.Context, user, password, mailbox, code, pin string) (string, error) {
	user = strings.TrimSpace(user)
	if user == "" || password == "" {
		return "", errors.New("enter your Proton username and password")
	}
	if pin != "" {
		if err := pinProblem(pin); err != nil {
			return "", err // before Proton or the key sees anything
		}
	}
	name := protonRemote
	if accounts := protonAccounts(); len(accounts) > 0 {
		a := accounts[0]
		name = a.Remote
		c.mu.Lock()
		inUse := c.remoteInUse(a.Remote)
		c.mu.Unlock()
		if !sameProtonUser(a.User, user) && inUse {
			return "", fmt.Errorf("MediaLayer connects one Proton account at a time, and the library uses %s. Stop syncing to its folders (or make another source the primary) first", a.User)
		}
	}
	// A failed sign-in leaves the connected account as it was.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	c.mu.Lock()
	if c.protonCancel != nil {
		c.protonCancel() // the sign-in before is given up
	}
	c.protonCancel = cancel
	c.mu.Unlock()
	session, err := c.protonLogin(ctx, user, password, mailbox, code, pin, func(k KeyTouch) { c.protonKeyWait.Store(int32(k)) })
	if err != nil {
		return "", err
	}
	// The library may have started using the account during the sign-in.
	c.mu.Lock()
	if ctx.Err() != nil { // cancelled as it finished
		c.mu.Unlock()
		return "", ctx.Err()
	}
	if old := config.GetValue(name, "username"); old != "" && !sameProtonUser(old, user) && c.remoteInUse(name) {
		c.mu.Unlock()
		return "", fmt.Errorf("MediaLayer connects one Proton account at a time, and the library uses %s. Stop syncing to its folders (or make another source the primary) first", old)
	}
	// A new section, so no password or code from an earlier config stays.
	config.LoadedData().DeleteSection(name)
	config.FileSetValue(name, "type", protonType)
	config.FileSetValue(name, "username", user)
	// MediaLayer is the only writer of its folders, so an upload left
	// unfinished (a draft, to Proton) is its own, and is replaced.
	config.FileSetValue(name, "replace_existing_draft", "true")
	config.FileSetValue(name, "app_version", protonAppVersion)
	for k, v := range session {
		config.FileSetValue(name, k, v)
	}
	c.mu.Unlock()
	config.SaveConfig() // may retry for a while: not under the lock
	// rclone's backend must be able to use the session.
	if _, err := openProton(ctx, name, ""); err != nil {
		return "", fmt.Errorf("signed in to Proton, and the account is connected, but Proton Drive wouldn't open: %w", err)
	}
	return name, nil
}

// protonAppVersion identifies the client to Proton on every request, at
// sign-in and through rclone's backend. It is the version a release of
// rclone sends, as the core runs rclone's backend; a build of rclone as a
// module would otherwise send a "-dev" version.
var protonAppVersion = func() string {
	v := strings.TrimPrefix(fs.VersionTag, "v")
	if parts := strings.Split(v, "."); len(parts) != 3 {
		v = "1.0.0"
	}
	return "external-drive-rclone@" + v + "-stable"
}()

// protonLogin signs in as Proton's own clients do: the password (by SRP),
// then the second step if the account has one (a 2FA code when given, or
// else its security key, see protonKeyStep), then the keys are unlocked
// with the password, or the mailbox password of a two-password account.
// It returns the session for the config, under protonSessionKeys.
//
// A sign-in that stops to ask for more (a code, the mailbox password, the
// key's PIN, or the PIN again after a wrong one) is kept, as pending, so
// the answer goes on from where it stopped: no new sign-in at Proton (and
// no new "new sign-in" email), and no new touch to pick the key. One that
// fails otherwise, or is given up, is signed out at Proton.
func (c *Core) protonLogin(ctx context.Context, user, password, mailbox, code, pin string, waiting func(KeyTouch)) (map[string]string, error) {
	c.mu.Lock()
	s := c.protonPending
	c.protonPending = nil
	c.mu.Unlock()
	if s != nil && !s.matches(user, password) {
		go s.end()
		s = nil
	}
	if s != nil {
		s.stopExpiry()
	} else {
		var err error
		if s, err = startProtonLogin(ctx, user, password); err != nil {
			return nil, err
		}
	}
	session, err := s.finish(ctx, password, mailbox, code, pin, waiting)
	switch {
	case err == nil && ctx.Err() == nil:
		s.close()
	case err == nil: // cancelled as it finished
		s.end()
		return nil, ctx.Err()
	case errors.Is(err, ErrProtonNeedsPIN) || errors.Is(err, ErrProtonNeeds2FA) || errors.Is(err, ErrProtonNeedsMailbox):
		c.keepProtonPending(ctx, s)
	default:
		s.end()
	}
	return session, err
}

// protonPendingFor is how long a sign-in waits for its next answer.
const protonPendingFor = 5 * time.Minute

// keepProtonPending keeps s for the next answer, until it expires.
// A sign-in cancelled meanwhile is signed out instead (CancelProtonSignIn
// cancels under c.mu, so the check here can't miss it).
func (c *Core) keepProtonPending(ctx context.Context, s *protonSignIn) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ctx.Err() != nil {
		go s.end()
		return
	}
	if old := c.protonPending; old != nil && old != s {
		go old.end()
	}
	c.protonPending = s
	s.kept++
	kept := s.kept
	s.expiry = time.AfterFunc(protonPendingFor, func() {
		c.mu.Lock()
		// Not if it was taken up, and kept again, since this timer began.
		mine := c.protonPending == s && s.kept == kept
		if mine {
			c.protonPending = nil
		}
		c.mu.Unlock()
		if mine {
			s.end()
		}
	})
}

// protonSignIn is a sign-in under way at Proton.
type protonSignIn struct {
	user     string
	password [32]byte // its hash, to tell an answer to this sign-in
	m        *proton.Manager
	c        *proton.Client
	auth     proton.Auth
	stepDone bool // the second step (2FA code or key) is done
	expiry   *time.Timer
	kept     int // times kept pending; under the core's mu

	mu     sync.Mutex
	tokens proton.Auth // renewals during the sign-in replace them
}

func startProtonLogin(ctx context.Context, user, password string) (*protonSignIn, error) {
	m := proton.New(
		proton.WithAppVersion(protonAppVersion),
		proton.WithUserAgent(fs.GetConfig(ctx).UserAgent),
		proton.WithTransport(fshttp.NewTransport(ctx)),
	)
	c, auth, err := m.NewClientWithLogin(ctx, user, []byte(password))
	if err != nil {
		m.Close()
		return nil, fmt.Errorf("Proton sign-in: %w", err)
	}
	s := &protonSignIn{user: user, password: sha256.Sum256([]byte(password)), m: m, c: c, auth: auth, tokens: auth}
	c.AddAuthHandler(func(a proton.Auth) {
		s.mu.Lock()
		s.tokens.UID, s.tokens.AccessToken, s.tokens.RefreshToken = a.UID, a.AccessToken, a.RefreshToken
		s.mu.Unlock()
	})
	return s, nil
}

func (s *protonSignIn) matches(user, password string) bool {
	h := sha256.Sum256([]byte(password))
	return s.user == user && subtle.ConstantTimeCompare(h[:], s.password[:]) == 1
}

func (s *protonSignIn) stopExpiry() {
	if s.expiry != nil {
		s.expiry.Stop()
	}
}

// close lets go of the sign-in, keeping its session at Proton.
func (s *protonSignIn) close() {
	s.stopExpiry()
	s.c.Close()
	s.m.Close()
}

// end signs the sign-in out at Proton (best effort) and lets go of it.
func (s *protonSignIn) end() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = s.c.AuthDelete(ctx)
	s.close()
}

// finish takes the sign-in on from where it stopped.
func (s *protonSignIn) finish(ctx context.Context, password, mailbox, code, pin string, waiting func(KeyTouch)) (map[string]string, error) {
	c, auth := s.c, s.auth
	// Asked before the 2FA code is spent, as a code is good once.
	if auth.PasswordMode == proton.TwoPasswordMode && mailbox == "" {
		return nil, ErrProtonNeedsMailbox
	}
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if tfa := auth.TwoFA.Enabled; tfa != 0 && !s.stepDone {
		switch {
		case tfa&proton.HasTOTP != 0 && code != "":
			if err := c.Auth2FA(ctx, proton.Auth2FAReq{TwoFactorCode: code}); err != nil {
				return nil, fmt.Errorf("Proton didn't accept the 2FA code (codes change every 30 seconds): %w", err)
			}
		case tfa&proton.HasFIDO2 != 0:
			if err := protonKeyStep(ctx, c, auth, pin, waiting); err != nil {
				if tfa&proton.HasTOTP != 0 && ctx.Err() == nil {
					msg := err.Error() + ". Or enter the code from your authenticator app"
					switch {
					case errors.Is(err, ErrProtonNoKey):
						err = needError{msg: "plug in your security key and sign in again, or enter the code from your authenticator app", need: ErrProtonNeeds2FA}
					case errors.Is(err, ErrProtonNeedsPIN):
						err = needError{msg: msg, need: ErrProtonNeedsPIN, also: ErrProtonNeeds2FA}
					default:
						err = needError{msg: msg, need: ErrProtonNeeds2FA}
					}
				}
				return nil, err
			}
		default:
			return nil, ErrProtonNeeds2FA
		}
		s.stepDone = true
	}
	keyPass := []byte(password)
	if auth.PasswordMode == proton.TwoPasswordMode {
		keyPass = []byte(mailbox)
	}
	u, err := c.GetUser(ctx)
	if err != nil {
		return nil, protonScopeError(err, auth)
	}
	addrs, err := c.GetAddresses(ctx)
	if err != nil {
		return nil, protonScopeError(err, auth)
	}
	salts, err := c.GetSalts(ctx)
	if err != nil {
		return nil, protonScopeError(err, auth)
	}
	if len(u.Keys) == 0 {
		return nil, errors.New("this Proton account has no keys yet: sign in once on proton.me to set it up")
	}
	salted, err := salts.SaltForKey(keyPass, u.Keys.Primary().ID)
	if err != nil {
		return nil, fmt.Errorf("Proton sign-in: %w", err)
	}
	userKR, _, err := proton.Unlock(u, addrs, salted, nil)
	if err != nil || userKR.CountDecryptionEntities(0) == 0 {
		what := "password"
		if auth.PasswordMode == proton.TwoPasswordMode {
			what = "mailbox password"
		}
		if err == nil {
			err = errors.New("no key unlocked")
		}
		return nil, fmt.Errorf("the %s didn't unlock this Proton account's keys: %w", what, err)
	}
	// Drive itself must answer too, before anything is saved.
	if _, err := c.ListVolumes(ctx); err != nil {
		return nil, protonScopeError(err, auth)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]string{
		"client_uid":             s.tokens.UID,
		"client_access_token":    s.tokens.AccessToken,
		"client_refresh_token":   s.tokens.RefreshToken,
		"client_salted_key_pass": base64.StdEncoding.EncodeToString(salted),
	}, nil
}

// protonScopeError explains Proton's "Access token does not have
// sufficient scope" (code 9100), which means the sign-in still lacks a
// step Proton wants.
func protonScopeError(err error, auth proton.Auth) error {
	var apiErr *proton.APIError
	if errors.As(err, &apiErr) && apiErr.Code == 9100 {
		return fmt.Errorf("Proton signed MediaLayer in with limited access (scope %q, two-step sign-in %d), so it needs another step that MediaLayer doesn't know yet: %w",
			auth.Scope, auth.TwoFA.Enabled, err)
	}
	return fmt.Errorf("Proton sign-in: %w", err)
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
	// Always sends protonAppVersion, also for accounts connected before
	// the core saved it.
	m := fs.ConfigMap(ri.Prefix, ri.Options, remote, configmap.Simple{"app_version": protonAppVersion})
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

// ProtonKeyWaiting reports what a Proton sign-in waits for its security
// key's touch for, if anything.
func (c *Core) ProtonKeyWaiting() KeyTouch { return KeyTouch(c.protonKeyWait.Load()) }

// CancelProtonSignIn gives up the Proton sign-in under way, if any, as
// while it waits for a security key.
func (c *Core) CancelProtonSignIn() {
	c.mu.Lock()
	if c.protonCancel != nil {
		c.protonCancel()
	}
	s := c.protonPending
	c.protonPending = nil
	c.mu.Unlock()
	if s != nil {
		go s.end() // signed out at Proton, off the caller's (the UI's) thread
	}
}
