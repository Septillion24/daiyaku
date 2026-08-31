package intercept

import (
	"strings"
	"testing"
)

const original = "127.0.0.1\tlocalhost\r\n::1\tlocalhost\r\n# a comment\r\n10.0.0.5\tinternal.example\r\n"

func TestPlanHostsAddsAndRevertRestoresExactly(t *testing.T) {
	edit := planHosts(original, "127.0.0.1", []string{"api.anthropic.com"})
	if len(edit.added) != 1 || edit.added[0] != "api.anthropic.com" {
		t.Fatalf("added = %v", edit.added)
	}
	if !strings.Contains(edit.content, "api.anthropic.com\t# daiyaku") {
		t.Errorf("entry not written:\n%s", edit.content)
	}
	for _, keep := range []string{"localhost", "# a comment", "internal.example"} {
		if !strings.Contains(edit.content, keep) {
			t.Errorf("existing line %q was lost", keep)
		}
	}
	if !strings.Contains(edit.content, "\r\n") {
		t.Error("windows line endings were not preserved")
	}
	restored, changed := planRevert(edit.content)
	if !changed {
		t.Fatal("revert reported no change")
	}
	if restored != original {
		t.Errorf("revert did not restore the file byte for byte:\nwant %q\ngot  %q", original, restored)
	}
}

// Running twice must not stack up duplicate lines.
func TestPlanHostsIsIdempotent(t *testing.T) {
	once := planHosts(original, "127.0.0.1", []string{"api.anthropic.com"})
	twice := planHosts(once.content, "127.0.0.1", []string{"api.anthropic.com"})
	if len(twice.added) != 0 || len(twice.already) != 1 {
		t.Errorf("second run: added=%v already=%v", twice.added, twice.already)
	}
	if strings.Count(twice.content, "api.anthropic.com") != 1 {
		t.Errorf("duplicate entry:\n%s", twice.content)
	}
}

// An entry the machine already had must be parked, not overwritten, and must
// come back on revert.
func TestPlanHostsParksAnExistingEntry(t *testing.T) {
	pre := original + "192.168.1.9\tapi.anthropic.com\tapi-alias\r\n"
	edit := planHosts(pre, "127.0.0.1", []string{"api.anthropic.com"})
	if len(edit.disabled) != 1 {
		t.Fatalf("disabled = %v, want the pre-existing entry parked", edit.disabled)
	}
	if !strings.Contains(edit.content, disabledPrefix+"192.168.1.9") {
		t.Errorf("pre-existing entry not parked:\n%s", edit.content)
	}
	restored, _ := planRevert(edit.content)
	if restored != pre {
		t.Errorf("revert lost the pre-existing entry:\nwant %q\ngot  %q", pre, restored)
	}
}

// A stale line from a previous run for a host this run does not want is cleaned
// up rather than left to redirect traffic forever.
func TestPlanHostsDropsStaleEntries(t *testing.T) {
	stale := original + "127.0.0.1\tapi.openai.com\t# daiyaku\r\n"
	edit := planHosts(stale, "127.0.0.1", []string{"api.anthropic.com"})
	if strings.Contains(edit.content, "api.openai.com") {
		t.Errorf("stale entry survived:\n%s", edit.content)
	}
}

func TestPlanRevertOnUntouchedFile(t *testing.T) {
	if _, changed := planRevert(original); changed {
		t.Error("revert reported a change on a file daiyaku never touched")
	}
}

// A commented-out mention must not be mistaken for an active entry.
func TestConflictIgnoresComments(t *testing.T) {
	want := map[string]bool{"api.anthropic.com": true}
	if _, ok := conflictingHost("# 1.2.3.4 api.anthropic.com", want); ok {
		t.Error("a commented line was treated as a conflict")
	}
	if _, ok := conflictingHost("1.2.3.4\tapi.anthropic.com", want); !ok {
		t.Error("an active entry was not detected as a conflict")
	}
	if _, ok := conflictingHost("1.2.3.4\tother.example # api.anthropic.com", want); ok {
		t.Error("a trailing comment was treated as a conflict")
	}
}
