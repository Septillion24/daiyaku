//go:build windows

package server_test

import (
	"os/exec"
	"strings"
	"testing"

	"daiyaku/internal/server"
)

// The run directory must not stay readable to other local users just because
// the parent grants them access (as a folder under a drive root does).
func TestTranscriptDirIsOwnerOnlyOnWindows(t *testing.T) {
	dir := t.TempDir()
	// Simulate the hostile parent: grant BUILTIN\Users read before daiyaku runs.
	if out, err := exec.Command("icacls", dir, "/grant", "*S-1-5-32-545:(OI)(CI)R").CombinedOutput(); err != nil {
		t.Fatalf("setup grant: %v: %s", err, out)
	}

	tx, err := server.NewTranscript(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Close()
	tx.Note("probe", "x")

	out, err := exec.Command("icacls", dir).CombinedOutput()
	if err != nil {
		t.Fatalf("icacls: %v: %s", err, out)
	}
	acl := string(out)
	for _, broad := range []string{`BUILTIN\Users`, "Authenticated Users", "Everyone"} {
		if strings.Contains(acl, broad) {
			t.Errorf("ACL still grants %s:\n%s", broad, acl)
		}
	}
}
