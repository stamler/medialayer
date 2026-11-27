//go:build !darwin

package core

// setHIDLocale: only macOS's HID backend converts device names with the C
// locale (see hid_darwin.go).
func setHIDLocale() {}

// setHIDShared: only macOS's hidapi seizes devices by default.
func setHIDShared() {}

func hidInitShared() {}
