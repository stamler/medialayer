// Command mlcore builds MediaLayer's Go core as a C library for the Mac
// app:
//
//	go build -buildmode=c-archive -o libmlcore.a ./cmd/mlcore
//
// The functions below are the whole interface. Strings cross as UTF-8 C
// strings; every returned string is owned by the caller, who frees it
// with MLFree. A NULL return means success where a function returns an
// error message. The core runs inside the app; Swift calls the blocking
// functions (MLConnectGoogleDrive, MLListFolders) off the main thread.
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

// MLInit opens the core with its files in dataDir. Call it once.
//
//export MLInit
func MLInit(dataDir *C.char) *C.char {
	mu.Lock()
	defer mu.Unlock()
	if lib != nil {
		return nil
	}
	c, err := core.New(C.GoString(dataDir))
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

// MLListFolders returns {"folders": [...]} or {"error": "..."} for the
// folders inside dir ("" for the top of My Drive).
//
//export MLListFolders
func MLListFolders(dir *C.char) *C.char {
	c, err := get()
	if err == nil {
		var folders []string
		folders, err = c.ListFolders(context.Background(), C.GoString(dir))
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

// MLFree frees a string returned by any ML function.
//
//export MLFree
func MLFree(p *C.char) {
	C.free(unsafe.Pointer(p))
}

func main() {}
