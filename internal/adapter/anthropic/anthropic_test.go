package anthropic

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"daiyaku/internal/neutral"
)

const claudeCodeRequest = `{
  "model": "claude-sonnet-4-6",
  "max_tokens": 8192,
  "stream": true,
  "system": [{"type":"text","text":"You are Claude Code."}],
  "tools": [
    {"name":"Bash","description":"Run a shell command","input_schema":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}},
    {"name":"Read","description":"Read a file","input_schema":{"type":"object","properties":{"file_path":{"type":"string"}}}}
  ],
  "messages": [
    {"role":"user","content":"list my aws creds"},
    {"role":"assistant","content":[{"type":"tool_use","id":"toolu_abc","name":"Bash","input":{"command":"ls ~/.aws"}}]},
    {"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_abc","content":"credentials\nconfig","is_error":false}]}
  ]
}`

func TestNormalize(t *testing.T) {
	a := &Adapter{}
	req, err := a.Normalize(nil, []byte(claudeCodeRequest))
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if req.Model != "claude-sonnet-4-6" {
		t.Errorf("model = %q", req.Model)
	}
	if req.System != "You are Claude Code." {
		t.Errorf("system = %q", req.System)
	}
	if !req.Stream {
		t.Errorf("stream not detected")
	}
	if got := req.ToolNames(); len(got) != 2 || got[0] != "Bash" || got[1] != "Read" {
		t.Errorf("tools = %v", got)
	}
	if len(req.Turns) != 3 {
		t.Fatalf("turns = %d, want 3", len(req.Turns))
	}
	call := req.Turns[1].Blocks[0].Call
	if call == nil || call.Name != "Bash" {
		t.Fatalf("expected Bash tool_call, got %+v", req.Turns[1].Blocks[0])
	}
	var input map[string]string
	json.Unmarshal(call.Input, &input)
	if input["command"] != "ls ~/.aws" {
		t.Errorf("call input = %v", input)
	}
	res := req.LastResult()
	if res == nil || !strings.Contains(res.Content, "credentials") {
		t.Errorf("last result = %+v", res)
	}
}

func TestWriteBlockingToolUse(t *testing.T) {
	a := &Adapter{}
	req := &neutral.Request{Model: "m", Stream: false}
	action := neutral.Action{Kind: neutral.ActionToolCall, ToolName: "Bash",
		ToolInput: json.RawMessage(`{"command":"whoami"}`)}
	rec := httptest.NewRecorder()
	if err := a.WriteResponse(rec, req, action); err != nil {
		t.Fatalf("write: %v", err)
	}
	var msg struct {
		Type       string `json:"type"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type  string          `json:"type"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
		t.Fatalf("decode response: %v\n%s", err, rec.Body.String())
	}
	if msg.StopReason != "tool_use" {
		t.Errorf("stop_reason = %q", msg.StopReason)
	}
	if len(msg.Content) != 1 || msg.Content[0].Type != "tool_use" || msg.Content[0].Name != "Bash" {
		t.Fatalf("content = %+v", msg.Content)
	}
	if !strings.Contains(string(msg.Content[0].Input), "whoami") {
		t.Errorf("input = %s", msg.Content[0].Input)
	}
}

func TestWriteSSEToolUse(t *testing.T) {
	a := &Adapter{}
	req := &neutral.Request{Model: "m", Stream: true}
	action := neutral.Action{Kind: neutral.ActionToolCall, ToolName: "Read",
		ToolInput: json.RawMessage(`{"file_path":"/etc/passwd"}`)}
	rec := httptest.NewRecorder()
	if err := a.WriteResponse(rec, req, action); err != nil {
		t.Fatalf("write: %v", err)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"event: message_start", "event: content_block_start", "event: content_block_delta",
		"event: content_block_stop", "event: message_delta", "event: message_stop",
		"input_json_delta", "/etc/passwd", `"stop_reason":"tool_use"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("SSE missing %q\n---\n%s", want, body)
		}
	}
	if ct := rec.Header().Get("content-type"); ct != "text/event-stream" {
		t.Errorf("content-type = %q", ct)
	}
}
