//go:build hwkey

package core

// Run on a Mac with a security key plugged in:
//
//	go test -tags hwkey -count=1 -v -run TestKeyFromManyThreads ./internal/core
//
// It talks to the key only (no Proton sign-in, no touch needed): each
// try opens the key from a new OS thread that then ends, as calls from
// the app's Swift threads do, and stops at the first touch wait (the
// touch that picks a key with a PIN), cut short after 3 seconds: don't
// touch the key. No PIN is ever sent.

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"
)

func TestKeyFromManyThreads(t *testing.T) {
	req := fidoRequest{rpID: "proton.me", clientData: []byte(`{}`), allow: [][]byte{{1, 2, 3}}}
	for i := 0; i < 4; i++ {
		done := make(chan error, 1)
		go func() {
			runtime.LockOSThread() // not unlocked: the thread ends with the goroutine
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_, err := assertWithKey(ctx, req, "", func(KeyTouch) {})
			done <- err
		}()
		var err error
		select {
		case err = <-done:
		case <-time.After(time.Minute):
			t.Fatalf("try %d hung", i)
		}
		t.Logf("try %d: %v", i, err)
		if errors.Is(err, errKeyNoAnswer) {
			t.Fatal("the key didn't answer")
		}
		if errors.Is(err, ErrProtonNoKey) {
			t.Fatal("plug in a security key")
		}
	}
}
