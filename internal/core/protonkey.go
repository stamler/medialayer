package core

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/go-ctap/ctaphid/pkg/ctaphid"
	"github.com/go-ctap/ctaphid/pkg/ctaptypes"
	"github.com/go-ctap/ctaphid/pkg/device"
	"github.com/go-ctap/ctaphid/pkg/options"
	"github.com/go-ctap/ctaphid/pkg/sugar"
	"github.com/go-ctap/ctaphid/pkg/webauthntypes"
	"github.com/go-resty/resty/v2"
	"github.com/rclone/go-proton-api"
)

// A Proton account's two-step sign-in can be a security key (FIDO2), as
// Proton's own apps do it: Proton sends WebAuthn options with the first
// step, the key signs Proton's challenge when touched, and the signature
// goes back as the second step. MediaLayer talks to a key plugged in over
// USB itself, with its PIN if it has one (passed to the key, never
// kept); a passkey kept by the system (iCloud Keychain) needs the
// system's passkey prompt, which an app may only use for its own website.

// ErrProtonNoKey: no security key is plugged in.
var ErrProtonNoKey = errors.New("plug in your security key, then sign in again (MediaLayer can use a key plugged in over USB, not a passkey kept in iCloud Keychain)")

// ErrProtonNeedsPIN: the security key has a PIN, which it wants first.
var ErrProtonNeedsPIN = errors.New("your security key has a PIN: enter it, then touch the key when it blinks")

// needError is a message that still tells the app what to ask for, and
// what else it may ask for (also, if not nil).
type needError struct {
	msg        string
	need, also error
}

func (e needError) Error() string { return e.msg }
func (e needError) Unwrap() []error {
	if e.also == nil {
		return []error{e.need}
	}
	return []error{e.need, e.also}
}

// fidoRequest is what a security key signs: Proton's options, read.
type fidoRequest struct {
	rpID       string
	clientData []byte   // the WebAuthn client data, JSON
	allow      [][]byte // the account's registered keys
}

// fidoOptions reads the options Proton sent with the first step. Proton
// sends the challenge and key ids as arrays of numbers.
func fidoOptions(opts any) (fidoRequest, error) {
	raw, err := json.Marshal(opts)
	if err != nil {
		return fidoRequest{}, err
	}
	var o struct {
		PublicKey struct {
			RPID             string `json:"rpId"`
			Challenge        []int  `json:"challenge"`
			AllowCredentials []struct {
				ID []int `json:"id"`
			} `json:"allowCredentials"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal(raw, &o); err != nil {
		return fidoRequest{}, fmt.Errorf("Proton's security key options: %w", err)
	}
	pk := o.PublicKey
	if pk.RPID == "" || len(pk.Challenge) == 0 {
		return fidoRequest{}, errors.New("Proton sent security key options without a challenge")
	}
	req := fidoRequest{rpID: pk.RPID}
	challenge, err := intBytes(pk.Challenge)
	if err != nil {
		return fidoRequest{}, err
	}
	for _, c := range pk.AllowCredentials {
		id, err := intBytes(c.ID)
		if err != nil {
			return fidoRequest{}, err
		}
		if len(id) > 0 {
			req.allow = append(req.allow, id)
		}
	}
	if len(req.allow) == 0 {
		return fidoRequest{}, errors.New("this Proton account has no security key registered for sign-in")
	}
	// As a browser makes it, and as Proton's Bridge does.
	req.clientData, err = json.Marshal(map[string]string{
		"type":      "webauthn.get",
		"challenge": base64.RawURLEncoding.EncodeToString(challenge),
		"origin":    "https://" + pk.RPID,
	})
	return req, err
}

func intBytes(ns []int) ([]byte, error) {
	b := make([]byte, len(ns))
	for i, n := range ns {
		if n < 0 || n > 255 {
			return nil, errors.New("Proton sent security key options with a bad byte")
		}
		b[i] = byte(n)
	}
	return b, nil
}

// fidoAssertion is a key's answer.
type fidoAssertion struct {
	credentialID, authData, signature []byte
}

// fido2Body is the second step's request body. go-proton-api's FIDO2Req
// has the credential id as a string; Proton wants an array of numbers.
type fido2Body struct {
	FIDO2 struct {
		AuthenticationOptions any
		ClientData            string
		AuthenticatorData     string
		Signature             string
		CredentialID          []int
	}
}

func newFido2Body(opts any, req fidoRequest, a fidoAssertion) fido2Body {
	var b fido2Body
	b.FIDO2.AuthenticationOptions = opts
	b.FIDO2.ClientData = base64.StdEncoding.EncodeToString(req.clientData)
	b.FIDO2.AuthenticatorData = base64.StdEncoding.EncodeToString(a.authData)
	b.FIDO2.Signature = base64.StdEncoding.EncodeToString(a.signature)
	b.FIDO2.CredentialID = make([]int, len(a.credentialID))
	for i, c := range a.credentialID {
		b.FIDO2.CredentialID[i] = int(c)
	}
	return b
}

// protonKeyStep signs Proton's challenge with a security key and sends
// the signature as the second step. waiting is told while the key waits
// for a touch.
func protonKeyStep(ctx context.Context, c *proton.Client, auth proton.Auth, pin string, waiting func(bool)) error {
	opts := auth.TwoFA.FIDO2.AuthenticationOptions
	req, err := fidoOptions(opts)
	if err != nil {
		return err
	}
	a, err := assertWithKey(ctx, req, pin, waiting)
	if err != nil {
		return err
	}
	body := newFido2Body(opts, req, a)
	// Auth2FA's own body can't carry the credential id Proton wants, so
	// the body is replaced on its way out (on a retry too). The hook is
	// on this sign-in's client only, which sends no other 2FA request.
	c.AddPreRequestHook(func(_ *resty.Client, r *resty.Request) error {
		if _, ok := r.Body.(proton.Auth2FAReq); ok {
			r.SetBody(body)
		}
		return nil
	})
	if err := c.Auth2FA(ctx, proton.Auth2FAReq{}); err != nil {
		return fmt.Errorf("Proton didn't accept the security key: %w", err)
	}
	return nil
}

// keyTouchTimeout is how long a key waits for its touch, at most. Keys
// give up on their own, most after 30 seconds.
const keyTouchTimeout = time.Minute

// keyBusy is held while a sign-in uses the key.
var keyBusy = make(chan struct{}, 1)

// assertWithKey asks the one security key plugged in to sign req. A key
// with a PIN is unlocked with pin first, as Proton's Bridge does; with
// no pin, the error is ErrProtonNeedsPIN. The PIN goes to the key only.
// A variable so tests can stand in for a key.
var assertWithKey = func(ctx context.Context, req fidoRequest, pin string, waiting func(bool)) (fidoAssertion, error) {
	// One sign-in at a time uses the key: macOS opens it for one user,
	// and closing it ends hidapi for the whole process. A cancelled
	// sign-in's key keeps it until the key gives up.
	select {
	case keyBusy <- struct{}{}:
	case <-ctx.Done():
		return fidoAssertion{}, ctx.Err()
	}
	held := true
	defer func() {
		if held {
			<-keyBusy
		}
	}()
	dev, err := openKey()
	if err != nil {
		return fidoAssertion{}, err
	}
	// ctaphid asks for a PIN protocol even without a PIN, and a key with
	// none (some CTAP 2.0 keys) would crash it.
	if len(dev.GetInfo().PinUvAuthProtocols) == 0 {
		_ = dev.Close()
		return fidoAssertion{}, errors.New("this security key is too old for MediaLayer to use (it has no PIN support): sign in with an authenticator app code")
	}
	var token []byte
	// A key that can't use its PIN for sign-ins (noMcGa...) signs in with
	// a touch, as before.
	if info := dev.GetInfo(); info.Options[ctaptypes.OptionClientPIN] && !info.Options[ctaptypes.OptionNoMcGaPermissionsWithClientPin] {
		if pin == "" {
			_ = dev.Close()
			return fidoAssertion{}, ErrProtonNeedsPIN
		}
		token, err = dev.GetPinUvAuthTokenUsingPIN(pin, ctaptypes.PermissionGetAssertion, req.rpID)
		if err != nil {
			err = pinError(dev, err)
			_ = dev.Close()
			return fidoAssertion{}, err
		}
	}
	allow := make([]webauthntypes.PublicKeyCredentialDescriptor, len(req.allow))
	for i, id := range req.allow {
		allow[i] = webauthntypes.PublicKeyCredentialDescriptor{Type: webauthntypes.PublicKeyCredentialTypePublicKey, ID: id}
	}
	type result struct {
		a   fidoAssertion
		err error
	}
	done := make(chan result, 1)
	waiting(true)
	defer waiting(false)
	// The key's answer can't be cancelled from here: on a cancel the
	// sign-in ends, and the key gives up by itself, then lets go.
	held = false
	go func() {
		defer func() { <-keyBusy }()
		defer dev.Close()
		defer func() {
			if p := recover(); p != nil {
				done <- result{err: fmt.Errorf("the security key couldn't sign in: %v", p)}
			}
		}()
		r := result{err: errors.New("the security key gave no answer")}
		// A touch, and the PIN's token when the key has one.
		for resp, err := range dev.GetAssertion(token, req.rpID, req.clientData, allow, nil,
			map[ctaptypes.Option]bool{ctaptypes.OptionUserPresence: true}) {
			if err != nil {
				r = result{err: keyError(err)}
				break
			}
			id := resp.Credential.ID
			if len(id) == 0 && len(req.allow) == 1 {
				id = req.allow[0] // a key may leave out the only one asked for
			}
			r = result{a: fidoAssertion{credentialID: id, authData: resp.AuthDataRaw, signature: resp.Signature}}
			break
		}
		done <- r
	}()
	t := time.NewTimer(keyTouchTimeout)
	defer t.Stop()
	select {
	case r := <-done:
		return r.a, r.err
	case <-ctx.Done():
		return fidoAssertion{}, ctx.Err()
	case <-t.C:
		return fidoAssertion{}, errors.New("the security key wasn't touched in time: sign in again, and touch it when it blinks")
	}
}

var hidLocaleOnce sync.Once

// openKey opens the one security key plugged in. The HID libraries
// panic on some devices' strings; that is an error here, never a crash
// of the app.
func openKey() (dev *device.Device, err error) {
	hidLocaleOnce.Do(setHIDLocale)
	defer func() {
		if p := recover(); p != nil {
			if dev != nil {
				_ = dev.Close()
			}
			dev, err = nil, fmt.Errorf("couldn't look for a security key: %v", p)
		}
	}()
	quiet := options.WithLogger(slog.New(slog.DiscardHandler))
	infos, err := sugar.EnumerateFIDODevices(quiet)
	if err != nil {
		return nil, fmt.Errorf("couldn't look for a security key: %w", err)
	}
	switch len(infos) {
	case 0:
		return nil, ErrProtonNoKey
	case 1:
	default:
		return nil, errors.New("more than one security key is plugged in: leave only the one for this Proton account, then sign in again")
	}
	dev, err = device.New(infos[0].Path, quiet)
	if err != nil {
		return nil, fmt.Errorf("couldn't open the security key: %w", err)
	}
	return dev, nil
}

// pinError explains a key's refusal of its PIN.
func pinError(dev *device.Device, err error) error {
	var ce *ctaphid.CTAPError
	if errors.As(err, &ce) {
		switch ce.StatusCode {
		case ctaphid.CTAP2_ERR_PIN_INVALID:
			msg := "that isn't the security key's PIN"
			if n, _, rerr := dev.GetPINRetries(); rerr == nil {
				msg += fmt.Sprintf(" (%d tries left before the key locks)", n)
			}
			return needError{msg: msg, need: ErrProtonNeedsPIN}
		case ctaphid.CTAP2_ERR_PIN_AUTH_BLOCKED:
			return errors.New("the security key stopped taking PINs after several wrong ones: unplug it, plug it in again, and sign in again")
		case ctaphid.CTAP2_ERR_PIN_BLOCKED:
			return errors.New("the security key is locked after too many wrong PINs: it needs a reset (in Yubico Authenticator), which removes its sign-ins")
		case ctaphid.CTAP2_ERR_PIN_POLICY_VIOLATION:
			return needError{msg: "that PIN is too short for the security key", need: ErrProtonNeedsPIN}
		}
	}
	return fmt.Errorf("the security key couldn't check its PIN: %w", err)
}

// keyError explains a security key's refusal.
func keyError(err error) error {
	var ce *ctaphid.CTAPError
	if errors.As(err, &ce) {
		switch ce.StatusCode {
		case ctaphid.CTAP2_ERR_NO_CREDENTIALS:
			return errors.New("this security key isn't one registered with this Proton account: plug in the right key and sign in again")
		case ctaphid.CTAP2_ERR_OPERATION_DENIED:
			return errors.New("the security key refused to sign in: sign in again, and touch it when it blinks")
		case ctaphid.CTAP2_ERR_USER_ACTION_TIMEOUT, ctaphid.CTAP2_ERR_ACTION_TIMEOUT, ctaphid.CTAP2_ERR_KEEPALIVE_CANCEL:
			return errors.New("the security key wasn't touched in time: sign in again, and touch it when it blinks")
		case ctaphid.CTAP2_ERR_PUAT_REQUIRED:
			return errors.New("your security key wants a PIN for every sign-in, but has none set: set one (in Yubico Authenticator), then sign in again")
		}
	}
	return fmt.Errorf("the security key couldn't sign in: %w", err)
}
