package intercept

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// marker tags every line daiyaku adds, so cleanup can find its own work without
// guessing and without touching anything the machine already had.
const marker = "# daiyaku"

// disabledPrefix parks a pre-existing entry for a hostname being intercepted.
// Overwriting it silently would break whatever it was there for, and leave no
// way to put it back.
const disabledPrefix = "# daiyaku-disabled "

// hostsPathOverride redirects every hosts-file operation, so the apply and
// revert cycle can be exercised in full without editing the real machine.
var hostsPathOverride string

// HostsPath is the machine's static name-to-address table.
func HostsPath() string {
	if hostsPathOverride != "" {
		return hostsPathOverride
	}
	if runtime.GOOS == "windows" {
		root := os.Getenv("SystemRoot")
		if root == "" {
			root = `C:\Windows`
		}
		return filepath.Join(root, "System32", "drivers", "etc", "hosts")
	}
	return "/etc/hosts"
}

// HostsWritable reports whether this process can modify the hosts file, by
// trying rather than by guessing at privilege: the answer depends on the OS, the
// account, and any managed policy in the way.
func HostsWritable() error {
	f, err := os.OpenFile(HostsPath(), os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	return f.Close()
}

// hostsEdit is the result of planning a change: the new file content plus what
// it would do, so callers can report before writing.
type hostsEdit struct {
	content  string
	added    []string
	disabled []string
	already  []string
}

// planHosts rewrites content so every name in hosts resolves to addr, leaving
// everything else byte-identical. Existing daiyaku lines are reused rather than
// duplicated, and a conflicting entry the machine already had is commented out
// with a tag so revert can restore it exactly.
func planHosts(content, addr string, hosts []string) hostsEdit {
	eol := "\n"
	if strings.Contains(content, "\r\n") {
		eol = "\r\n"
	}
	want := map[string]bool{}
	for _, h := range hosts {
		want[strings.ToLower(h)] = true
	}

	var out []string
	edit := hostsEdit{}
	have := map[string]bool{}

	for _, line := range splitLines(content) {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, disabledPrefix):
			// Left over from an earlier run; keep it parked until revert.
			out = append(out, line)
			continue
		case strings.HasSuffix(trimmed, marker):
			// One of ours. Keep it only if this run still wants that name.
			if h, ok := ourHost(trimmed); ok && want[h] {
				have[h] = true
				edit.already = append(edit.already, h)
				out = append(out, line)
				continue
			}
			continue // stale: drop it
		}
		if h, ok := conflictingHost(line, want); ok {
			edit.disabled = append(edit.disabled, h)
			out = append(out, disabledPrefix+line)
			continue
		}
		out = append(out, line)
	}

	for _, h := range hosts {
		if have[strings.ToLower(h)] {
			continue
		}
		edit.added = append(edit.added, h)
		out = append(out, fmt.Sprintf("%s\t%s\t%s", addr, h, marker))
	}

	body := strings.Join(out, eol)
	if !strings.HasSuffix(body, eol) {
		body += eol
	}
	edit.content = body
	return edit
}

// ourHost pulls the hostname out of a line daiyaku wrote.
func ourHost(line string) (string, bool) {
	fields := strings.Fields(strings.TrimSuffix(line, marker))
	if len(fields) < 2 {
		return "", false
	}
	return strings.ToLower(fields[1]), true
}

// conflictingHost reports an active (uncommented) entry that already maps one of
// the names this run wants to redirect.
func conflictingHost(line string, want map[string]bool) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return "", false
	}
	fields := strings.Fields(trimmed)
	for _, f := range fields[1:] {
		if strings.HasPrefix(f, "#") {
			break
		}
		if want[strings.ToLower(f)] {
			return strings.ToLower(f), true
		}
	}
	return "", false
}

// planRevert removes every daiyaku line and un-parks anything it disabled,
// returning the restored content and whether anything changed.
func planRevert(content string) (string, bool) {
	eol := "\n"
	if strings.Contains(content, "\r\n") {
		eol = "\r\n"
	}
	var out []string
	changed := false
	for _, line := range splitLines(content) {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasSuffix(trimmed, marker):
			changed = true
		case strings.HasPrefix(trimmed, disabledPrefix):
			out = append(out, strings.TrimPrefix(trimmed, disabledPrefix))
			changed = true
		default:
			out = append(out, line)
		}
	}
	if !changed {
		return content, false
	}
	body := strings.Join(out, eol)
	if !strings.HasSuffix(body, eol) {
		body += eol
	}
	return body, true
}

// splitLines splits on either line ending without leaving stray carriage
// returns, and drops the empty element a trailing newline produces.
func splitLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	return lines
}

// HostsContains reports whether the hosts file currently carries daiyaku lines,
// which is how a run left behind by a crash is detected at startup.
func HostsContains() (bool, error) {
	b, err := os.ReadFile(HostsPath())
	if err != nil {
		return false, err
	}
	for _, line := range splitLines(string(b)) {
		t := strings.TrimSpace(line)
		if strings.HasSuffix(t, marker) || strings.HasPrefix(t, disabledPrefix) {
			return true, nil
		}
	}
	return false, nil
}
