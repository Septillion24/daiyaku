package console

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"daiyaku/internal/neutral"
)

func shellReq(seq int, withResult bool) *neutral.Request {
	r := &neutral.Request{
		Provider: "anthropic", Model: "m", Seq: seq,
		Tools: []neutral.ToolDef{
			{Name: "Bash", Kind: "function",
				Schema: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`)},
			{Name: "Read", Kind: "function",
				Schema: json.RawMessage(`{"type":"object","properties":{"file_path":{"type":"string"}}}`)},
		},
	}
	if withResult {
		r.Turns = []neutral.Turn{{Role: "user", Blocks: []neutral.Block{{Type: neutral.BlockToolResult,
			Result: &neutral.ToolResult{Content: "prev output"}}}}}
	}
	return r
}

// One reader shared across interact() calls, so cross-turn shell-mode persistence can be exercised.
func scriptedREPL(script string) *REPL {
	return &REPL{Provider: "anthropic", in: bufio.NewReader(strings.NewReader(script)), out: io.Discard}
}

func TestShellModeSendsCommand(t *testing.T) {
	r := scriptedREPL("shell\nwhoami\n")
	action := r.interact(shellReq(1, false))
	if action.Kind != neutral.ActionToolCall || action.ToolName != "Bash" {
		t.Fatalf("action = %+v", action)
	}
	if !strings.Contains(string(action.ToolInput), `"command":"whoami"`) {
		t.Errorf("input = %s", action.ToolInput)
	}
	if !r.shellMode {
		t.Errorf("expected to still be in shell mode")
	}
}

func TestShellModePersistsAndExit(t *testing.T) {
	r := scriptedREPL("shell\nid\n:exit\nend wrapping up\n")

	a1 := r.interact(shellReq(1, false))
	if a1.ToolName != "Bash" || !strings.Contains(string(a1.ToolInput), "id") {
		t.Fatalf("turn1 = %+v", a1)
	}
	if !r.shellMode {
		t.Fatal("should be in shell mode after turn 1")
	}

	a2 := r.interact(shellReq(2, true))
	if a2.Kind != neutral.ActionEnd || a2.Text != "wrapping up" {
		t.Fatalf("turn2 = %+v", a2)
	}
	if r.shellMode {
		t.Errorf(":exit should have left shell mode")
	}
}

func TestEnterShellNoToolFails(t *testing.T) {
	// With no shell/exec tool, 'shell' must refuse and a following 'end' still works.
	req := &neutral.Request{Provider: "anthropic", Seq: 1,
		Tools: []neutral.ToolDef{{Name: "Read", Kind: "function"}}}
	r := scriptedREPL("shell\nend nope\n")
	a := r.interact(req)
	if a.Kind != neutral.ActionEnd || a.Text != "nope" {
		t.Fatalf("action = %+v", a)
	}
	if r.shellMode {
		t.Errorf("should not have entered shell mode without a shell tool")
	}
}

func TestFindShellTool(t *testing.T) {
	name, field, ok := findShellTool(shellReq(1, false))
	if !ok || name != "Bash" || field != "command" {
		t.Fatalf("findShellTool = %q %q %v", name, field, ok)
	}
}

func TestCompleteToolNames(t *testing.T) {
	r := &REPL{}
	r.curReq = shellReq(1, false)
	got := r.completeToolNames()
	if len(got) != 2 || got[0] != "Bash" || got[1] != "Read" {
		t.Errorf("completeToolNames = %v", got)
	}
}

// A shell-named tool with no string field must not enter shell mode (would send {"":"<cmd>"}).
func TestEnterShellEmptyFieldRefused(t *testing.T) {
	req := &neutral.Request{Provider: "anthropic", Seq: 1,
		Tools: []neutral.ToolDef{{Name: "bash", Kind: "function",
			Schema: json.RawMessage(`{"type":"object","properties":{"n":{"type":"number"}}}`)}}}
	r := scriptedREPL("shell\nend nope\n")
	a := r.interact(req)
	if r.shellMode {
		t.Error("should not enter shell mode with no string field")
	}
	if a.Kind != neutral.ActionEnd || a.Text != "nope" {
		t.Fatalf("action = %+v", a)
	}
}
