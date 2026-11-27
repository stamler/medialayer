package core

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"

	_ "github.com/rclone/rclone/backend/smb" // SMB shares
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/obscure"
)

// An SMB share's folder can be a secondary, or the primary (see
// docs/smb.md). The server, port, user and domain are kept in the core's
// rclone config, as a remote of type smb; the password never is. The app
// keeps it in the keychain and hands it to the core at launch
// (SetSMBPassword), and the core keeps it in memory only, adding it to
// the remote's options each time a folder is opened.

// smbType is rclone's SMB backend.
const smbType = "smb"

// smbRemotePrefix names the remotes the core creates: smb, smb2, smb3...
const smbRemotePrefix = "smb"

// smbPasswords are the SMB remotes' passwords, by remote, in memory only.
var smbPasswords = struct {
	sync.Mutex
	m map[string]string
}{m: map[string]string{}}

func smbPassword(remote string) (string, bool) {
	smbPasswords.Lock()
	defer smbPasswords.Unlock()
	p, ok := smbPasswords.m[remote]
	return p, ok
}

// ErrSMBNeedsPassword: the core has no password for an SMB remote; the
// app asks for it, or reads it from the keychain, and calls
// SetSMBPassword.
var ErrSMBNeedsPassword = errors.New("MediaLayer needs the password for this SMB server")

// errSMBLogon: the server refused the user name and password.
var errSMBLogon = errors.New("logon refused")

// smbLogonError is a refused logon: why, in words, after the host.
type smbLogonError struct{ host, why string }

func (e smbLogonError) Error() string        { return e.host + " " + e.why }
func (e smbLogonError) Is(target error) bool { return target == errSMBLogon }

// SMBServer is an SMB server and user the core has a remote for.
type SMBServer struct {
	Remote string `json:"remote"`
	Host   string `json:"host"`
	Port   int    `json:"port"`
	User   string `json:"user"`
	Domain string `json:"domain,omitempty"`
	// HasPassword is set once the app has given the password this session.
	HasPassword bool `json:"has_password"`
}

// smbServers lists the SMB remotes in the core's config, by name.
func smbServers() []SMBServer {
	out := []SMBServer{}
	for _, name := range config.FileSections() {
		if config.GetValue(name, "type") != smbType {
			continue
		}
		port, _ := strconv.Atoi(config.GetValue(name, "port"))
		if port == 0 {
			port = 445
		}
		_, has := smbPassword(name)
		out = append(out, SMBServer{Remote: name, Host: config.GetValue(name, "host"), Port: port,
			User: config.GetValue(name, "user"), Domain: config.GetValue(name, "domain"), HasPassword: has})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Remote < out[j].Remote })
	return out
}

// AddSMBServer connects to an SMB server as user, checking the password
// by listing the server's shares, and returns its remote: the one the
// core already has for this server, port and user, or a new one. port 0
// means 445. The password is kept for this session only; the app saves
// it in the keychain.
func (c *Core) AddSMBServer(ctx context.Context, host string, port int, user, domain, password string) (string, error) {
	host, user, domain = strings.TrimSpace(host), strings.TrimSpace(user), strings.TrimSpace(domain)
	host = strings.TrimSuffix(strings.TrimPrefix(host, "smb://"), "/")
	if host == "" || user == "" || password == "" {
		return "", errors.New("enter the server, a user name and its password")
	}
	if strings.ContainsAny(host, "/\\ ") {
		return "", errors.New("enter the server's name or address only, such as studio.local, not a folder in it")
	}
	if h, p, err := net.SplitHostPort(host); err == nil { // studio.local:4455, [fd7a::1]:445
		n, err := strconv.Atoi(p)
		if err != nil || n <= 0 || n > 65535 || (port != 0 && port != 445 && port != n) {
			return "", errors.New("enter the port once, under More options")
		}
		host, port = h, n
	}
	// rclone dials host + ":" + port, so an IPv6 address keeps brackets.
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	if port == 0 {
		port = 445
	}
	// One sign-in at a time, so two never take the same new remote.
	smbAddMu.Lock()
	defer smbAddMu.Unlock()
	name := ""
	for _, s := range smbServers() {
		if strings.EqualFold(s.Host, host) && s.Port == port && s.User == user {
			name = s.Remote
		}
	}
	if name == "" {
		for i := 1; name == ""; i++ {
			n := smbRemotePrefix
			if i > 1 {
				n = fmt.Sprintf("%s%d", smbRemotePrefix, i)
			}
			if config.GetValue(n, "type") == "" {
				name = n
			}
		}
	}
	values := map[string]string{"host": host, "user": user}
	if port != 445 {
		values["port"] = strconv.Itoa(port)
	}
	if domain != "" {
		values["domain"] = domain
	}
	// Checked with exactly what is saved below, not over what the
	// remote's config held before, and with one logon try.
	f, err := openSMBWith(onceCtx(ctx), name, "", values, password, false)
	if err == nil {
		_, err = f.List(ctx, "") // the shares
		if sh, ok := f.(fs.Shutdowner); ok {
			_ = sh.Shutdown(ctx)
		}
	}
	if err != nil {
		return "", smbError(host, err)
	}
	isNew := config.GetValue(name, "type") == ""
	if isNew {
		config.FileSetValue(name, "type", smbType)
	}
	for _, k := range []string{"host", "user", "port", "domain"} {
		if v, ok := values[k]; ok {
			config.FileSetValue(name, k, v)
		} else {
			config.FileDeleteKey(name, k)
		}
	}
	config.SaveConfig()
	c.SetSMBPassword(name, password)
	return name, nil
}

// SetSMBPassword gives the core an SMB remote's password, for this
// session. The app calls it at launch with the password from the
// keychain. An empty password forgets it.
func (c *Core) SetSMBPassword(remote, password string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	smbPasswords.Lock()
	old, had := smbPasswords.m[remote]
	if password == "" {
		delete(smbPasswords.m, remote)
	} else {
		smbPasswords.m[remote] = password
	}
	smbPasswords.Unlock()
	if had && old == password || !had && password == "" {
		return // as it was: nothing to open again
	}
	// Folders on this server opened with the old password open again.
	if c.primaryRemote() == remote {
		c.fsys = nil
		c.stream.forgetAll()
	}
	if c.jobFs != nil {
		for k := range c.jobFs {
			if strings.HasPrefix(k, remote+"\x00") {
				delete(c.jobFs, k)
			}
		}
	}
}

// openSMB opens folder ("share/path", or "" for the list of shares) on an
// SMB remote, with the password the app gave.
//
// rclone retries a refused logon as it does any error, up to the low
// level retries (10), and a server may lock out an account after a few
// failed logons. So the logon is first tried once, by a client that
// makes one try (its pacer keeps that for good), and only then is the
// client that does the work, with the usual retries, opened. A password
// changed on the server while that one is open can still cost its
// retries, once: the next open forgets the password.
func openSMB(ctx context.Context, remote, folder string) (fs.Fs, error) {
	pass, ok := smbPassword(remote)
	if !ok {
		return nil, ErrSMBNeedsPassword
	}
	// The check opens the folder's share (or, for the list of shares,
	// the server), so a share that isn't there costs one try too.
	share, _, _ := strings.Cut(strings.Trim(folder, "/"), "/")
	f, err := openSMBWith(onceCtx(ctx), remote, share, nil, pass, true)
	if err == nil {
		_, err = f.List(ctx, "") // logs on: NewFs doesn't, at the top
		if sh, ok := f.(fs.Shutdowner); ok {
			_ = sh.Shutdown(ctx)
		}
	}
	if err == nil {
		f, err = openSMBWith(ctx, remote, folder, nil, pass, true)
	}
	if err != nil {
		err = smbError(config.GetValue(remote, "host"), err)
		if errors.Is(err, errSMBLogon) {
			// Not tried again until the app gives a password, so a
			// changed password doesn't add failed logons every sync.
			smbPasswords.Lock()
			if smbPasswords.m[remote] == pass {
				delete(smbPasswords.m, remote)
			}
			smbPasswords.Unlock()
			return nil, fmt.Errorf("%w: %w", ErrSMBNeedsPassword, err)
		}
		return nil, err
	}
	return f, nil
}

// onceCtx is ctx with one low level try, for logons.
func onceCtx(ctx context.Context) context.Context {
	ctx, ci := fs.AddConfig(ctx)
	ci.LowLevelRetries = 1
	return ctx
}

// smbAddMu serializes AddSMBServer.
var smbAddMu sync.Mutex

// openSMBWith opens folder on the SMB remote name, with values over the
// remote's config if fromFile (or in place of it) and the password. The
// password goes into the options directly, never into the config or the
// remote's name: an Fs opened through rclone's connection strings would
// be named smb{hash}, and the string would reach the debug log.
func openSMBWith(ctx context.Context, name, folder string, values map[string]string, password string, fromFile bool) (fs.Fs, error) {
	ri, err := fs.Find(smbType)
	if err != nil {
		return nil, err
	}
	over := configmap.Simple{"pass": obscure.MustObscure(password)}
	for k, v := range values {
		over[k] = v
	}
	configName := name
	if !fromFile {
		configName = "" // the backend's defaults under values only
	}
	m := fs.ConfigMap(ri.Prefix, ri.Options, configName, over)
	return ri.NewFs(ctx, name, strings.Trim(folder, "/"), m)
}

// smbError says what went wrong reaching an SMB server in words, and
// when the server is on a tailnet, that Tailscale may not be connected.
func smbError(host string, err error) error {
	msg := err.Error()
	// go-smb2 gives the NT status's text.
	switch {
	case strings.Contains(msg, "logon is invalid") || strings.Contains(msg, "LOGON_FAILURE"):
		return smbLogonError{host, "didn't accept that user name and password"}
	case strings.Contains(msg, "account has been automatically locked") || strings.Contains(msg, "ACCOUNT_LOCKED_OUT"):
		return smbLogonError{host, "has locked this account after too many failed sign-ins. Unlock it on the server, then sign in again"}
	case strings.Contains(msg, "password has expired") || strings.Contains(msg, "password must be changed"):
		return smbLogonError{host, "says this user's password has expired. Change it on the server, then sign in again"}
	case strings.Contains(msg, "currently disabled") || strings.Contains(msg, "account restriction") ||
		strings.Contains(msg, "account has expired") || strings.Contains(msg, "time restrictions") ||
		strings.Contains(msg, "log on from the source workstation"):
		return smbLogonError{host, "won't let this account sign in: it is disabled or restricted"}
	case strings.Contains(msg, "not been granted those access rights") || strings.Contains(msg, "ACCESS_DENIED") ||
		strings.Contains(msg, "permission denied"):
		return fmt.Errorf("%s: this user isn't allowed in there: %w", host, err)
	case strings.Contains(msg, "share name cannot be found"):
		return fmt.Errorf("%s has no share by that name: %w", host, err)
	}
	var ne net.Error
	if errors.As(err, &ne) || strings.Contains(msg, "dial tcp") || strings.Contains(msg, "no such host") {
		if onTailnet(host) {
			return fmt.Errorf("can't reach %s over Tailscale. Is Tailscale connected on this device? (%w)", host, err)
		}
		return fmt.Errorf("can't reach %s: %w", host, err)
	}
	return err
}

// onTailnet reports whether host is a Tailscale name or address.
func onTailnet(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(strings.Trim(host, "[]"), "."))
	if strings.HasSuffix(h, ".ts.net") {
		return true
	}
	if a, err := netip.ParseAddr(h); err == nil {
		return netip.MustParsePrefix("100.64.0.0/10").Contains(a) || netip.MustParsePrefix("fd7a:115c:a1e0::/48").Contains(a)
	}
	return false
}
