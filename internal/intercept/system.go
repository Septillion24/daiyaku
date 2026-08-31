package intercept

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// TrustCommand is a platform's way to add or remove a CA in the machine trust
// store. Both are surfaced to the operator verbatim: this modifies what the
// whole machine will believe, so it should never happen invisibly, and the
// removal has to be printable even if daiyaku is gone.
type TrustCommand struct {
	Install []string
	Remove  []string
	Note    string
}

// TrustCommands returns the platform commands for the CA at certPath. Linux is
// the awkward one: the file has to be copied into the anchor directory first,
// so Install is the copy plus the refresh, expressed as a shell line.
func TrustCommands(certPath string) TrustCommand {
	switch runtime.GOOS {
	case "windows":
		return TrustCommand{
			Install: []string{"certutil", "-addstore", "-f", "Root", certPath},
			Remove:  []string{"certutil", "-delstore", "Root", CACommonName},
			Note:    "adds the CA to the machine Root store; needs an elevated shell",
		}
	case "darwin":
		return TrustCommand{
			Install: []string{"security", "add-trusted-cert", "-d", "-r", "trustRoot",
				"-k", "/Library/Keychains/System.keychain", certPath},
			Remove: []string{"security", "delete-certificate", "-c", CACommonName,
				"/Library/Keychains/System.keychain"},
			Note: "adds the CA to the System keychain; prompts for an admin password",
		}
	default:
		dest := "/usr/local/share/ca-certificates/daiyaku.crt"
		return TrustCommand{
			Install: []string{"sh", "-c", fmt.Sprintf("cp %q %q && update-ca-certificates", certPath, dest)},
			Remove:  []string{"sh", "-c", fmt.Sprintf("rm -f %q && update-ca-certificates", dest)},
			Note:    "Debian/Ubuntu layout; RHEL uses /etc/pki/ca-trust and update-ca-trust",
		}
	}
}

// runCommand executes a platform command, returning its combined output so a
// failure can be reported with what the OS actually said.
func runCommand(argv []string) (string, error) {
	if len(argv) == 0 {
		return "", fmt.Errorf("empty command")
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// FlushDNS clears the resolver cache. Without it the machine keeps answering
// with the address it looked up before the hosts file changed, and the first
// minute of a run mysteriously does not intercept. Best effort: a machine with
// no cache daemon is not an error.
func FlushDNS() string {
	var argv []string
	switch runtime.GOOS {
	case "windows":
		argv = []string{"ipconfig", "/flushdns"}
	case "darwin":
		argv = []string{"sh", "-c", "dscacheutil -flushcache; killall -HUP mDNSResponder"}
	default:
		argv = []string{"sh", "-c", "resolvectl flush-caches 2>/dev/null || systemd-resolve --flush-caches 2>/dev/null || true"}
	}
	if _, err := runCommand(argv); err != nil {
		return "not flushed (no cache service found); if the harness does not reach daiyaku, wait a minute or reboot the resolver"
	}
	return "flushed"
}

// backupHosts keeps a copy of the untouched file before the first edit. Revert
// works by un-editing, not by restoring this, but if anything ever goes wrong
// with that the operator still has the original.
func backupHosts(dir string) (string, error) {
	b, err := os.ReadFile(HostsPath())
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, fmt.Sprintf("hosts.backup.%s", time.Now().Format("20060102-150405")))
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// writeHosts replaces the hosts file in place. A rename cannot be used: on
// Windows the file is a protected system path where a replace loses the ACL, and
// on every platform other resolvers may hold it open.
func writeHosts(content string) error {
	return os.WriteFile(HostsPath(), []byte(content), 0o644)
}

// PermissionHint turns the OS's refusal into the thing the operator should do,
// since "access is denied" on the hosts file always means the same thing.
func PermissionHint() string {
	switch runtime.GOOS {
	case "windows":
		return "run this from an elevated terminal (right-click > Run as administrator)"
	default:
		return "run this with sudo"
	}
}
