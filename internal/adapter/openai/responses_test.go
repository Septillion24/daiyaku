package openai

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"daiyaku/internal/neutral"
)

const codexRequest = `{
  "model": "gpt-5-codex",
  "instructions": "You are Codex.",
  "stream": true,
  "tools": [
    {"type":"function","name":"shell","description":"run a command","parameters":{"type":"object","properties":{"command":{"type":"array","items":{"type":"string"}}}}}
  ],
  "input": [
    {"type":"message","role":"user","content":[{"type":"input_text","text":"show env"}]},
    {"type":"function_call","name":"shell","call_id":"call_1","arguments":"{\"command\":[\"env\"]}"},
    {"type":"function_call_output","call_id":"call_1","output":"PATH=/usr/bin"}
  ]
}`

func TestNormalizeResponses(t *testing.T) {
	a := &Adapter{}
	req, err := a.Normalize(nil, []byte(codexRequest))
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if req.System != "You are Codex." {
		t.Errorf("system = %q", req.System)
	}
	if names := req.ToolNames(); len(names) != 1 || names[0] != "shell" {
		t.Errorf("tools = %v", names)
	}
	if len(req.Turns) != 3 {
		t.Fatalf("turns = %d want 3", len(req.Turns))
	}
	call := req.Turns[1].Blocks[0].Call
	if call == nil || call.Name != "shell" {
		t.Fatalf("expected shell call, got %+v", req.Turns[1].Blocks[0])
	}
	// arguments (a JSON string on the wire) must normalize to a JSON object.
	var obj map[string]interface{}
	if err := json.Unmarshal(call.Input, &obj); err != nil {
		t.Fatalf("call input not an object: %s (%v)", call.Input, err)
	}
	res := req.LastResult()
	if res == nil || !strings.Contains(res.Content, "PATH=/usr/bin") {
		t.Errorf("last result = %+v", res)
	}
}

func TestWriteBlockingResponses(t *testing.T) {
	a := &Adapter{}
	req := &neutral.Request{Model: "gpt-5-codex", Stream: false}
	action := neutral.Action{Kind: neutral.ActionToolCall, ToolName: "shell",
		ToolInput: json.RawMessage(`{"command":["whoami"]}`)}
	rec := httptest.NewRecorder()
	if err := a.WriteResponse(rec, req, action); err != nil {
		t.Fatalf("write: %v", err)
	}
	var resp struct {
		Status string `json:"status"`
		Output []struct {
			Type      string `json:"type"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"output"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v\n%s", err, rec.Body.String())
	}
	if resp.Status != "completed" {
		t.Errorf("status = %q", resp.Status)
	}
	if len(resp.Output) != 1 || resp.Output[0].Type != "function_call" {
		t.Fatalf("output = %+v", resp.Output)
	}
	// arguments must be serialized back as a JSON-encoded string.
	if !strings.Contains(resp.Output[0].Arguments, "whoami") {
		t.Errorf("arguments = %q", resp.Output[0].Arguments)
	}
}

func TestWriteSSEResponses(t *testing.T) {
	a := &Adapter{}
	req := &neutral.Request{Model: "gpt-5-codex", Stream: true}
	action := neutral.Action{Kind: neutral.ActionToolCall, ToolName: "shell",
		ToolInput: json.RawMessage(`{"command":["id"]}`)}
	rec := httptest.NewRecorder()
	if err := a.WriteResponse(rec, req, action); err != nil {
		t.Fatalf("write: %v", err)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"event: response.created", "event: response.output_item.added",
		"event: response.function_call_arguments.delta",
		"event: response.function_call_arguments.done",
		"event: response.output_item.done", "event: response.completed",
		"function_call", `"name":"shell"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("SSE missing %q\n---\n%s", want, body)
		}
	}
}

// TestNormalizeEmptyArgs: a function_call with empty arguments must normalize to
// an object {}, not the JSON string "".
func TestNormalizeEmptyArgs(t *testing.T) {
	body := `{"model":"m","input":[{"type":"function_call","name":"shell","call_id":"c","arguments":""}]}`
	req, err := (&Adapter{}).Normalize(nil, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	call := req.Turns[0].Blocks[0].Call
	if call == nil {
		t.Fatal("no call parsed")
	}
	var obj map[string]interface{}
	if err := json.Unmarshal(call.Input, &obj); err != nil {
		t.Fatalf("input is not an object: %s (%v)", call.Input, err)
	}
}
