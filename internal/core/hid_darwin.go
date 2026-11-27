//go:build darwin

package core

/*
#include <locale.h>

// Device names from macOS's HID manager are wide strings, which go-hid
// turns into Go strings with wcstombs: in the C locale a process starts
// with, any non-ASCII character in any device's name (a Bluetooth
// keyboard's curly apostrophe) fails, and go-hid panics.
static void mlHIDLocale(void) { setlocale(LC_CTYPE, "UTF-8"); }
*/
import "C"

import "github.com/sstallion/go-hid"

func setHIDLocale() { C.mlHIDLocale() }

// setHIDShared has hidapi open devices without seizing them, as other
// FIDO clients (libfido2, browsers) do.
func setHIDShared() { hid.SetOpenExclusive(false) }
