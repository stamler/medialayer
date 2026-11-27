package core

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-ctap/ctaphid/pkg/ctaphid"
	"github.com/rclone/Proton-API-Bridge/common"
	"github.com/rclone/go-proton-api"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/hash"
)

// fakeProton puts a signed-in Proton remote in the core's config.
func fakeProton(t *testing.T, name, user string) {
	t.Helper()
	config.FileSetValue(name, "type", protonType)
	config.FileSetValue(name, "username", user)
	for _, k := range protonSessionKeys {
		config.FileSetValue(name, k, "x")
	}
}

func TestProtonAccounts(t *testing.T) {
	newTestCore(t, t.TempDir())
	fakeProton(t, protonRemote, "a@proton.me")
	got := protonAccounts()
	if len(got) != 1 || got[0] != (ProtonAccount{Remote: protonRemote, User: "a@proton.me", SignedIn: true}) {
		t.Fatalf("protonAccounts() = %+v", got)
	}
	if !remoteConnected(protonRemote) {
		t.Error("a Proton remote with a session isn't connected")
	}
	// Proton ending the session clears these keys.
	config.FileSetValue(protonRemote, "client_access_token", "")
	if remoteConnected(protonRemote) || protonAccounts()[0].SignedIn {
		t.Error("a Proton remote without a session counts as signed in")
	}
}

func TestConnectProtonRefusesSecondAccountInUse(t *testing.T) {
	c := newTestCore(t, t.TempDir())
	fakeProton(t, protonRemote, "a@proton.me")
	ctx := context.Background()
	if _, err := c.ConnectProton(ctx, " ", "pw", "", ""); err == nil {
		t.Error("signed in without a username")
	}
	if _, err := c.ConnectProton(ctx, "b@proton.me", "", "", ""); err == nil {
		t.Error("signed in without a password")
	}
	c.mu.Lock()
	c.settings.Secondaries = []source{{ID: "p", Remote: protonRemote, Folder: "Music"}}
	c.mu.Unlock()
	_, err := c.ConnectProton(ctx, "b@proton.me", "pw", "", "")
	if err == nil || !strings.Contains(err.Error(), "one Proton account") {
		t.Errorf("a second account replaced the one in use: %v", err)
	}
	if got := config.GetValue(protonRemote, "username"); got != "a@proton.me" {
		t.Errorf("the account in use became %q", got)
	}
}

func TestProtonSecondaryRules(t *testing.T) {
	c, _, _ := newMirrorCore(t)
	fakeProton(t, protonRemote, "a@proton.me")
	c.mu.Lock()
	c.settings.Secondaries = append(c.settings.Secondaries, source{ID: "p", Remote: protonRemote, Folder: "Music"})
	c.mu.Unlock()
	st := mirrorOf(t, c, "p")
	if st.Type != protonType || !st.CaseBlind {
		t.Errorf("Proton secondary status = %+v", st)
	}
	if st := mirrorOf(t, c, "sec1"); !st.CaseBlind {
		t.Errorf("local secondary status = %+v", st)
	}
}

func TestProtonErrorWhenSignedOut(t *testing.T) {
	for _, err := range []error{common.ErrUsernameAndPasswordRequired, common.Err2FACodeRequired} {
		wrapped := fmt.Errorf("couldn't initialize a new proton drive instance: %w", err)
		if got := protonError(wrapped); !strings.Contains(got.Error(), "saved sign-in") {
			t.Errorf("protonError(%v) = %v", wrapped, got)
		}
	}
	other := errors.New("network down")
	if protonError(other) != other {
		t.Error("protonError changed an unrelated error")
	}
}

// sha1Only shows a local object as on a cloud that keeps SHA-1 only, as
// Proton Drive does.
type sha1Only struct{ fs.Object }

func (o sha1Only) Fs() fs.Info { return sha1Info{o.Object.Fs()} }

type sha1Info struct{ fs.Info }

func (sha1Info) Hashes() hash.Set { return hash.Set(hash.SHA1) }

func TestSameFileBySHA1(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	write(t, filepath.Join(dir, "remote.bin"), []byte("same bytes"))
	f, err := fs.NewFs(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	o, err := f.NewObject(ctx, "remote.bin")
	if err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(t.TempDir(), "local.bin")
	write(t, local, []byte("same bytes"))
	if !sameFile(ctx, sha1Only{o}, local) {
		t.Error("files with one SHA-1 differ")
	}
	if err := os.WriteFile(local, []byte("other byte"), 0o644); err != nil {
		t.Fatal(err)
	}
	if sameFile(ctx, sha1Only{o}, local) {
		t.Error("files with different SHA-1s are the same")
	}
}

func TestSameProtonUser(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		same bool
	}{
		{"alice@proton.me", "Alice@Proton.me", true},
		{"alice@proton.me", "alice", true},
		{"alice", "alice@pm.me", true},
		{"alice", "alice@gmail.com", false},
		{"alice@proton.me", "alice@pm.me", false},
		{"alice", "bob", false},
	} {
		if got := sameProtonUser(tc.a, tc.b); got != tc.same {
			t.Errorf("sameProtonUser(%q, %q) = %v", tc.a, tc.b, got)
		}
	}
}

func TestOpenProtonKeepsSession(t *testing.T) {
	newTestCore(t, t.TempDir())
	fakeProton(t, protonRemote, "a@proton.me")
	k := keepSession(protonRemote)
	k.Set("client_access_token", "")
	if !protonSignedIn(protonRemote) {
		t.Error("a cleared session was saved")
	}
	k.Set("client_access_token", "renewed")
	if got := config.GetValue(protonRemote, "client_access_token"); got != "renewed" {
		t.Errorf("a renewed session wasn't saved: %q", got)
	}
}

// coarseFs reports times kept to the second, as Proton Drive does.
type coarseFs struct{ fs.Fs }

func (coarseFs) Precision() time.Duration { return time.Second }

// A primary that keeps times to the second (a Proton folder made the
// primary) keeps every file's id and doesn't read it again: each row
// takes the time the cloud reports.
func TestIndexKeepsFilesOnCoarseTimes(t *testing.T) {
	ctx := context.Background()
	lib := writeLibrary(t, nil)
	base := time.Date(2026, 1, 2, 3, 4, 5, 678_000_000, time.UTC)
	for _, p := range []string{"A/Album/01.flac", "A/Album/02.flac", "B/Other/01.flac"} {
		if err := os.Chtimes(filepath.Join(lib, p), base, base); err != nil {
			t.Fatal(err)
		}
	}
	c := newTestCore(t, lib)
	f, err := fs.NewFs(ctx, lib)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.index(ctx, f); err != nil {
		t.Fatal(err)
	}
	before := fileIDs(t, c)
	whole := base.Truncate(time.Second)
	for p := range before {
		if err := os.Chtimes(filepath.Join(lib, p), whole, whole); err != nil {
			t.Fatal(err)
		}
	}
	// Unreadable now: indexing it again would fail.
	if err := os.WriteFile(filepath.Join(lib, "B/Other/01.flac"), make([]byte, fileSize(filepath.Join(lib, "B/Other/01.flac"))), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(lib, "B/Other/01.flac"), whole, whole); err != nil {
		t.Fatal(err)
	}
	if err := c.index(ctx, coarseFs{f}); err != nil {
		t.Fatal(err)
	}
	known, err := c.currentDB().KnownFiles()
	if err != nil {
		t.Fatal(err)
	}
	for p, id := range before {
		k := known[p]
		if k.ID != id || k.MTime != whole.UnixMilli() {
			t.Errorf("%s: id %s -> %s, mtime %d (want %d)", p, id, k.ID, k.MTime, whole.UnixMilli())
		}
	}
	// On a cloud with fine times, a time off by under a second is a change.
	if withinPrecision(f, whole.UnixMilli(), base.UnixMilli()) {
		t.Error("a local folder's times count as the same within a second")
	}
}

func TestProtonAppVersionIsStable(t *testing.T) {
	if !strings.HasPrefix(protonAppVersion, "external-drive-rclone@") || !strings.HasSuffix(protonAppVersion, "-stable") {
		t.Errorf("protonAppVersion = %q", protonAppVersion)
	}
}

func TestProtonScopeError(t *testing.T) {
	err := protonScopeError(fmt.Errorf("wrapped: %w", &proton.APIError{Code: 9100, Status: 403, Message: "Access token does not have sufficient scope"}),
		proton.Auth{Scope: "self", TwoFA: proton.TwoFAInfo{Enabled: proton.HasFIDO2}})
	if !strings.Contains(err.Error(), "limited access") || !strings.Contains(err.Error(), `"self"`) {
		t.Errorf("protonScopeError = %v", err)
	}
	if err := protonScopeError(errors.New("offline"), proton.Auth{}); strings.Contains(err.Error(), "limited access") {
		t.Errorf("an unrelated error explained as scope: %v", err)
	}
}

// protonFidoOptions is the shape Proton sends: numbers for bytes.
func protonFidoOptions(t *testing.T) any {
	t.Helper()
	var opts any
	if err := json.Unmarshal([]byte(`{"publicKey":{"rpId":"proton.me","timeout":60000,"userVerification":"discouraged",
		"challenge":[1,2,3,250],"allowCredentials":[{"id":[9,8,7],"type":"public-key"},{"id":[6],"type":"public-key"}]}}`), &opts); err != nil {
		t.Fatal(err)
	}
	return opts
}

func TestFidoOptions(t *testing.T) {
	req, err := fidoOptions(protonFidoOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	if req.rpID != "proton.me" || len(req.allow) != 2 || !bytes.Equal(req.allow[0], []byte{9, 8, 7}) {
		t.Errorf("fidoOptions = %+v", req)
	}
	var cd map[string]string
	if err := json.Unmarshal(req.clientData, &cd); err != nil {
		t.Fatal(err)
	}
	if cd["type"] != "webauthn.get" || cd["origin"] != "https://proton.me" || cd["challenge"] != "AQID-g" {
		t.Errorf("client data = %v", cd)
	}
	for _, bad := range []string{`{}`, `{"publicKey":{"rpId":"proton.me","challenge":[1],"allowCredentials":[]}}`,
		`{"publicKey":{"rpId":"proton.me","challenge":[1,300],"allowCredentials":[{"id":[1]}]}}`} {
		var o any
		_ = json.Unmarshal([]byte(bad), &o)
		if _, err := fidoOptions(o); err == nil {
			t.Errorf("fidoOptions(%s) succeeded", bad)
		}
	}
}

// The second step goes to Proton with the key's answer in the shape
// Proton's own apps send, though go-proton-api's request type can't hold
// it.
func TestProtonKeyStepSendsAssertion(t *testing.T) {
	var got map[string]map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/v4/2fa" {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"Code":1000,"Scopes":["full"]}`)
	}))
	defer srv.Close()
	m := proton.New(proton.WithHostURL(srv.URL))
	defer m.Close()
	c := m.NewClient("uid", "acc", "ref")
	defer c.Close()

	old := assertWithKey
	defer func() { assertWithKey = old }()
	var waited []bool
	assertWithKey = func(_ context.Context, req fidoRequest, waiting func(bool)) (fidoAssertion, error) {
		waiting(true)
		waiting(false)
		return fidoAssertion{credentialID: []byte{9, 8, 7}, authData: []byte("auth"), signature: []byte("sig")}, nil
	}
	opts := protonFidoOptions(t)
	auth := proton.Auth{TwoFA: proton.TwoFAInfo{Enabled: proton.HasFIDO2, FIDO2: proton.FIDO2Info{AuthenticationOptions: opts}}}
	if err := protonKeyStep(context.Background(), c, auth, func(w bool) { waited = append(waited, w) }); err != nil {
		t.Fatal(err)
	}
	f := got["FIDO2"]
	if f == nil {
		t.Fatalf("body = %v", got)
	}
	if fmt.Sprint(f["CredentialID"]) != "[9 8 7]" || f["AuthenticatorData"] != "YXV0aA==" || f["Signature"] != "c2ln" {
		t.Errorf("FIDO2 = %v", f)
	}
	cd, _ := base64.StdEncoding.DecodeString(f["ClientData"].(string))
	if !strings.Contains(string(cd), `"origin":"https://proton.me"`) {
		t.Errorf("client data = %s", cd)
	}
	if fmt.Sprint(f["AuthenticationOptions"]) != fmt.Sprint(opts) {
		t.Errorf("options sent back = %v", f["AuthenticationOptions"])
	}
	if fmt.Sprint(waited) != "[true false]" {
		t.Errorf("waiting = %v", waited)
	}
}

func TestKeyError(t *testing.T) {
	for code, want := range map[ctaphid.StatusCode]string{
		ctaphid.CTAP2_ERR_NO_CREDENTIALS:      "isn't one registered",
		ctaphid.CTAP2_ERR_USER_ACTION_TIMEOUT: "touched in time",
		ctaphid.CTAP2_ERR_PUAT_REQUIRED:       "PIN",
	} {
		err := keyError(fmt.Errorf("x: %w", &ctaphid.CTAPError{StatusCode: code}))
		if !strings.Contains(err.Error(), want) {
			t.Errorf("keyError(%v) = %v", code, err)
		}
	}
}

func TestNoKeyAsksForCode(t *testing.T) {
	err := needError{"plug in your key", ErrProtonNeeds2FA}
	if !errors.Is(err, ErrProtonNeeds2FA) || err.Error() != "plug in your key" {
		t.Errorf("needError = %v", err)
	}
}
