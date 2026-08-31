package console

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"daiyaku/internal/neutral"
)

func testRequest() *neutral.Request {
	return &neutral.Request{
		Provider: "anthropic", Model: "m", Seq: 1,
		Tools: []neutral.ToolDef{
			{Name: "Bash", Kind: "function", Description: "run a command",
				Schema: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`)},
		},
		Turns: []neutral.Turn{{Role: "user", Blocks: []neutral.Block{{Type: neutral.BlockText, Text: "hi"}}}},
	}
}

func replWithScript(script string) *REPL {
	return &REPL{
		Provider: "anthropic",
		in:       bufio.NewReader(strings.NewReader(script)),
		out:      io.Discard,
	}
}

func TestREPLAuthorsToolCall(t *testing.T) {
	r := replWithScript("call Bash {\"command\":\"id\"}\n")
	action := r.interact(testRequest())
	if action.Kind != neutral.ActionToolCall || action.ToolName != "Bash" {
		t.Fatalf("action = %+v", action)
	}
	if !strings.Contains(string(action.ToolInput), "id") {
		t.Errorf("input = %s", action.ToolInput)
	}
}

func TestREPLInspectThenEnd(t *testing.T) {
	// Inspection commands must not terminate the turn; only call/text/end do.
	r := replWithScript("tools\nschema Bash\ntemplate Bash\nsys\nlast\nend all done\n")
	action := r.interact(testRequest())
	if action.Kind != neutral.ActionEnd || action.Text != "all done" {
		t.Fatalf("action = %+v", action)
	}
}

func TestREPLInvalidJSONRetries(t *testing.T) {
	// A bad JSON call is rejected (loop continues); the next good call is returned.
	r := replWithScript("call Bash {bad}\ncall Bash {\"command\":\"whoami\"}\n")
	action := r.interact(testRequest())
	if action.Kind != neutral.ActionToolCall || !strings.Contains(string(action.ToolInput), "whoami") {
		t.Fatalf("action = %+v", action)
	}
}

func TestREPLEOFEndsTurn(t *testing.T) {
	// Closed stdin must end the turn gracefully rather than hang the harness.
	r := replWithScript("")
	action := r.interact(testRequest())
	if action.Kind != neutral.ActionEnd {
		t.Fatalf("action = %+v", action)
	}
}

func TestTemplateFromSchema(t *testing.T) {
	req := testRequest()
	tpl := Template(req, "Bash")
	var got map[string]interface{}
	if err := json.Unmarshal([]byte(tpl), &got); err != nil {
		t.Fatalf("template not valid JSON: %v (%s)", err, tpl)
	}
	if _, ok := got["command"]; !ok {
		t.Errorf("template missing 'command': %s", tpl)
	}
}
