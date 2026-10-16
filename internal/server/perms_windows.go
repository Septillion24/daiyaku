//go:build windows

package server

import (
	"fmt"
	"os/exec"
	"os/user"
	"strings"
)

// restrictToOwner replaces dir's ACL with one granting only the current user
// full control, and stops it inheriting from the parent. Go's 0o700/0o600 modes
// do nothing on Windows, so without this a run directory created under a drive
// root or a shared folder inherits grants such as BUILTIN\Users:(RX) and
// Authenticated Users:(M), and any local user could read the transcript.
//
// The grant is by SID (the "*S-1-..." form) so it does not depend on account
// name resolution. (OI)(CI) makes files created later in the directory, such as
// the transcript and the reconstructed sequence, inherit the same ACL.
func restrictToOwner(dir string) error {
	u, err := user.Current()
	if err != nil {
		return fmt.Errorf("look up current user: %w", err)
	}
	// Three steps because /grant:r only replaces the named user's own entries: a
	// grant already set explicitly on the directory (to Users, say) would survive
	// it. /reset drops explicit entries, /inheritance:r drops inherited ones, and
	// the grant then leaves the user as the only entry.
	for _, args := range [][]string{
		{dir, "/reset"},
		{dir, "/inheritance:r"},
		{dir, "/grant:r", "*" + u.Uid + ":(OI)(CI)F"},
	} {
		if out, err := exec.Command("icacls", args...).CombinedOutput(); err != nil {
			return fmt.Errorf("icacls %s: %w: %s", args[1], err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}
