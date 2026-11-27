package core

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/fshttp"
	"github.com/rclone/rclone/lib/oauthutil"
	"golang.org/x/oauth2/google"
)

// The app names a Google account by its email address, not by the rclone
// remote it is ("gdrive2"). Drive tells the address once, and the core
// keeps it in its settings.

// GoogleAccount is a connected Google account's address.
type GoogleAccount struct {
	Remote string `json:"remote"`
	Email  string `json:"email"`
}

// googleEmail asks Drive which account the remote name is signed in to.
func (c *Core) googleEmail(ctx context.Context, name string) (string, error) {
	g, err := c.googleClient()
	if err != nil {
		return "", err
	}
	ri, err := fs.Find("drive")
	if err != nil {
		return "", err
	}
	m := fs.ConfigMap(ri.Prefix, ri.Options, name, nil)
	cfg := &oauthutil.Config{
		ClientID: g.ID, ClientSecret: g.Secret,
		AuthURL: google.Endpoint.AuthURL, TokenURL: google.Endpoint.TokenURL,
		Scopes: []string{"https://www.googleapis.com/auth/drive"},
	}
	client, _, err := oauthutil.NewClientWithBaseClient(ctx, name, m, cfg, fshttp.NewClient(ctx))
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://www.googleapis.com/drive/v3/about?fields=user(emailAddress)", nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Drive answered %s", resp.Status)
	}
	var about struct {
		User struct {
			EmailAddress string `json:"emailAddress"`
		} `json:"user"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&about); err != nil {
		return "", err
	}
	if about.User.EmailAddress == "" {
		return "", fmt.Errorf("Drive didn't say which account %s is", name)
	}
	return about.User.EmailAddress, nil
}

// learnGoogleEmail asks for and keeps the address of the remote name,
// replacing what was kept (a sign-in may be to another account, so one
// that fails forgets the old address).
func (c *Core) learnGoogleEmail(ctx context.Context, name string) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	email, err := c.googleEmail(ctx, name)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	if err != nil {
		fs.Errorf(nil, "find the Google account of %s: %v", name, err)
		if _, ok := c.settings.GoogleEmails[name]; ok {
			delete(c.settings.GoogleEmails, name)
			if err := c.saveSettings(); err != nil {
				fs.Errorf(nil, "save settings: %v", err)
			}
		}
		return
	}
	if c.settings.GoogleEmails == nil {
		c.settings.GoogleEmails = map[string]string{}
	}
	if c.settings.GoogleEmails[name] == email {
		return
	}
	c.settings.GoogleEmails[name] = email
	if err := c.saveSettings(); err != nil {
		fs.Errorf(nil, "save the Google account of %s: %v", name, err)
	}
}

// unknownGoogleEmails are the connected Google accounts whose address
// isn't known yet, as for accounts connected before the app kept them.
// The caller holds c.mu, or is Open.
func (c *Core) unknownGoogleEmailsLocked() []string {
	var todo []string
	for _, name := range driveAccounts(c.primaryRemote()) {
		if c.settings.GoogleEmails[name] == "" {
			todo = append(todo, name)
		}
	}
	return todo
}

// learnGoogleEmails finds the addresses of the accounts named.
func (c *Core) learnGoogleEmails(names []string) {
	for _, name := range names {
		c.learnGoogleEmail(context.Background(), name)
	}
}
