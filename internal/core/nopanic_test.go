package core

import (
	"net/http"
	"testing"
)

func TestNoPanic(t *testing.T) {
	var resp *http.Response
	err := noPanic("copy a.flac", func() error {
		_ = resp.StatusCode // as go-proton-api's CreateFile does with no answer
		return nil
	})
	if err == nil {
		t.Fatal("noPanic returned nil after a panic")
	}
	if err := noPanic("copy b.flac", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
}
