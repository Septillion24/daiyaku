//go:build !windows

package server

// restrictToOwner is a no-op off Windows: the 0o700 directory and 0o600 file
// modes NewTranscript already uses are enforced by the OS.
func restrictToOwner(string) error { return nil }
