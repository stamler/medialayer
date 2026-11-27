// Command mlcore builds MediaLayer's Go core as a C library for the Mac
// app:
//
//	go build -buildmode=c-archive -o libmlcore.a ./cmd/mlcore
//
// The functions below are the whole interface. Strings cross as UTF-8 C
// strings; every returned string is owned by the caller, who frees it
// with MLFree. A NULL return means success where a function returns an
// error message. The core runs inside the app; Swift calls the blocking
// functions (MLConnectGoogleDrive, MLAddGoogleAccount, MLConnectProton,
// MLListFolders) off the main thread.
//
// gomobile will replace this once full Xcode is part of the build.
package main

/*
#include <stdlib.h>
*/
import "C"

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"unsafe"

	"github.com/stamler/medialayer/internal/core"
)

var (
	mu  sync.Mutex
	lib *core.Core
)

func get() (*core.Core, error) {
	mu.Lock()
	defer mu.Unlock()
	if lib == nil {
		return nil, errors.New("MLInit has not been called")
	}
	return lib, nil
}

func cstr(s string) *C.char { return C.CString(s) }

// errStr returns NULL for nil, else the message.
func errStr(err error) *C.char {
	if err == nil {
		return nil
	}
	return cstr(err.Error())
}

func jsonStr(v any) *C.char {
	b, err := json.Marshal(v)
	if err != nil {
		b, _ = json.Marshal(map[string]string{"error": err.Error()})
	}
	return cstr(string(b))
}

// MLInit opens the core with its files in dataDir, its saved sign-ins
// encrypted with configKey (which the app keeps in the keychain). Call
// it once.
//
//export MLInit
func MLInit(dataDir, configKey *C.char) *C.char {
	mu.Lock()
	defer mu.Unlock()
	if lib != nil {
		return nil
	}
	c, err := core.Open(C.GoString(dataDir), C.GoString(configKey))
	if err != nil {
		return errStr(err)
	}
	lib = c
	return nil
}

// MLStatus returns the core's status as JSON (see core.Status).
//
//export MLStatus
func MLStatus() *C.char {
	c, err := get()
	if err != nil {
		return jsonStr(map[string]string{"error": err.Error()})
	}
	return jsonStr(c.Status())
}

// MLConnectGoogleDrive signs in to Google Drive in the browser. It blocks
// until the user finishes.
//
//export MLConnectGoogleDrive
func MLConnectGoogleDrive() *C.char {
	c, err := get()
	if err != nil {
		return errStr(err)
	}
	return errStr(c.ConnectGoogleDrive(context.Background()))
}

// MLAddGoogleAccount signs in to another Google account, for a
// secondary, in the browser. It blocks until the user finishes and
// returns {"remote": "..."} or {"error": "..."}.
//
//export MLAddGoogleAccount
func MLAddGoogleAccount() *C.char {
	c, err := get()
	if err == nil {
		var remote string
		if remote, err = c.AddGoogleAccount(context.Background()); err == nil {
			return jsonStr(map[string]string{"remote": remote})
		}
	}
	return jsonStr(map[string]string{"error": err.Error()})
}

// MLConnectProton signs in to Proton Drive with a username, password,
// mailbox password and 2FA code (the last two may be empty). It blocks
// until Proton answers and returns {"remote": "..."} or {"error": "..."},
// with "need": "2fa" or "mailbox" when the account needs that too. An
// account with a security key signs in with the key plugged in when no
// code is given (see MLProtonKeyWaiting).
//
//export MLConnectProton
func MLConnectProton(user, password, mailbox, code *C.char) *C.char {
	c, err := get()
	if err == nil {
		var remote string
		remote, err = c.ConnectProton(context.Background(), C.GoString(user), C.GoString(password), C.GoString(mailbox), C.GoString(code))
		if err == nil {
			return jsonStr(map[string]string{"remote": remote})
		}
	}
	out := map[string]string{"error": err.Error()}
	switch {
	case errors.Is(err, core.ErrProtonNeeds2FA):
		out["need"] = "2fa"
	case errors.Is(err, core.ErrProtonNeedsMailbox):
		out["need"] = "mailbox"
	}
	return jsonStr(out)
}

// MLProtonKeyWaiting returns 1 while MLConnectProton waits for a security
// key to be touched, else 0.
//
//export MLProtonKeyWaiting
func MLProtonKeyWaiting() C.int {
	if c, err := get(); err == nil && c.ProtonKeyWaiting() {
		return 1
	}
	return 0
}

// MLCancelProtonSignIn gives up the MLConnectProton call under way,
// which then returns an error.
//
//export MLCancelProtonSignIn
func MLCancelProtonSignIn() {
	if c, err := get(); err == nil {
		c.CancelProtonSignIn()
	}
}

// MLListFolders returns {"folders": [...]} or {"error": "..."} for the
// folders inside dir ("" for the top of the drive) on a connected
// account ("" for the primary's).
//
//export MLListFolders
func MLListFolders(remote, dir *C.char) *C.char {
	c, err := get()
	if err == nil {
		var folders []string
		folders, err = c.ListFolders(context.Background(), C.GoString(remote), C.GoString(dir))
		if err == nil {
			if folders == nil {
				folders = []string{}
			}
			return jsonStr(map[string][]string{"folders": folders})
		}
	}
	return jsonStr(map[string]string{"error": err.Error()})
}

// MLSetFolder picks the library folder and starts syncing it.
//
//export MLSetFolder
func MLSetFolder(folder *C.char) *C.char {
	c, err := get()
	if err != nil {
		return errStr(err)
	}
	return errStr(c.SetFolder(C.GoString(folder)))
}

// MLAddSecondary adds a folder on a connected account (or, with
// an empty remote, a local folder) as a secondary and starts syncing.
//
//export MLAddSecondary
func MLAddSecondary(remote, folder *C.char) *C.char {
	c, err := get()
	if err != nil {
		return errStr(err)
	}
	return errStr(c.AddSecondary(C.GoString(remote), C.GoString(folder)))
}

// MLRemoveSecondary stops syncing to a secondary; its files stay.
//
//export MLRemoveSecondary
func MLRemoveSecondary(id *C.char) *C.char {
	c, err := get()
	if err != nil {
		return errStr(err)
	}
	return errStr(c.RemoveSecondary(C.GoString(id)))
}

// MLApproveChanges lets a secondary that paused on a mass change go
// ahead, and starts a sync.
//
//export MLApproveChanges
func MLApproveChanges(id *C.char) *C.char {
	c, err := get()
	if err != nil {
		return errStr(err)
	}
	return errStr(c.ApproveChanges(C.GoString(id)))
}

// MLPromote makes a synced secondary the library's primary.
//
//export MLPromote
func MLPromote(id *C.char) *C.char {
	c, err := get()
	if err != nil {
		return errStr(err)
	}
	return errStr(c.Promote(C.GoString(id)))
}

// MLStartSync re-reads the library folder: restore, index new and changed
// files, publish. It returns at once; MLStatus shows progress.
//
//export MLStartSync
func MLStartSync() *C.char {
	c, err := get()
	if err != nil {
		return errStr(err)
	}
	return errStr(c.StartSync())
}

// MLPrepare gets ready to stream a file (the selected or next track)
// by looking up its cloud object in the background.
//
//export MLPrepare
func MLPrepare(fileID *C.char) {
	if c, err := get(); err == nil {
		c.Prepare(C.GoString(fileID))
	}
}

// MLCreatePlaylist makes an empty playlist and returns {"id": "..."} or
// {"error": "..."}.
//
//export MLCreatePlaylist
func MLCreatePlaylist(name *C.char) *C.char {
	c, err := get()
	if err == nil {
		var id string
		if id, err = c.CreatePlaylist(C.GoString(name)); err == nil {
			return jsonStr(map[string]string{"id": id})
		}
	}
	return jsonStr(map[string]string{"error": err.Error()})
}

// MLRenamePlaylist renames a playlist.
//
//export MLRenamePlaylist
func MLRenamePlaylist(id, name *C.char) *C.char {
	c, err := get()
	if err != nil {
		return errStr(err)
	}
	return errStr(c.RenamePlaylist(C.GoString(id), C.GoString(name)))
}

// MLDeletePlaylist deletes a playlist; its tracks stay in the library.
//
//export MLDeletePlaylist
func MLDeletePlaylist(id *C.char) *C.char {
	c, err := get()
	if err != nil {
		return errStr(err)
	}
	return errStr(c.DeletePlaylist(C.GoString(id)))
}

// MLAddToPlaylist inserts files, given as a JSON array of file ids,
// before the item at position before, or at the end when before is
// negative.
//
//export MLAddToPlaylist
func MLAddToPlaylist(id, fileIDsJSON *C.char, before C.int) *C.char {
	c, err := get()
	if err != nil {
		return errStr(err)
	}
	var ids []string
	if err := json.Unmarshal([]byte(C.GoString(fileIDsJSON)), &ids); err != nil {
		return errStr(err)
	}
	return errStr(c.AddToPlaylist(C.GoString(id), ids, int(before)))
}

// MLRemoveFromPlaylist removes the items at the positions given as a
// JSON array of numbers.
//
//export MLRemoveFromPlaylist
func MLRemoveFromPlaylist(id, positionsJSON *C.char) *C.char {
	c, err := get()
	if err != nil {
		return errStr(err)
	}
	var positions []int
	if err := json.Unmarshal([]byte(C.GoString(positionsJSON)), &positions); err != nil {
		return errStr(err)
	}
	return errStr(c.RemoveFromPlaylist(C.GoString(id), positions))
}

// MLMovePlaylistItems moves the items at the positions given as a JSON
// array of numbers to just before the item now at position before, or
// to the end when before is negative.
//
//export MLMovePlaylistItems
func MLMovePlaylistItems(id, positionsJSON *C.char, before C.int) *C.char {
	c, err := get()
	if err != nil {
		return errStr(err)
	}
	var positions []int
	if err := json.Unmarshal([]byte(C.GoString(positionsJSON)), &positions); err != nil {
		return errStr(err)
	}
	return errStr(c.MovePlaylistItems(C.GoString(id), positions, int(before)))
}

// MLRemoveMissingFromPlaylist removes the items whose file is missing
// from the library, which the app hides.
//
//export MLRemoveMissingFromPlaylist
func MLRemoveMissingFromPlaylist(id *C.char) *C.char {
	c, err := get()
	if err != nil {
		return errStr(err)
	}
	return errStr(c.RemoveMissingFromPlaylist(C.GoString(id)))
}

// MLFree frees a string returned by any ML function.
//
//export MLFree
func MLFree(p *C.char) {
	C.free(unsafe.Pointer(p))
}

func main() {}
