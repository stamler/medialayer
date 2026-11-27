//go:build !linux && !darwin

package core

// fileChange is unknown here, so every scan reads every file again.
func fileChange(string) string { return "" }
