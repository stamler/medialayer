//go:build !darwin

package core

// setHIDLocale: only macOS's HID backend converts device names with the C
// locale (see hidlocale_darwin.go).
func setHIDLocale() {}
