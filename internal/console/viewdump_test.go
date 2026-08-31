package console

import (
	"encoding/json"
	"strings"
	"testing"

	"daiyaku/internal/engine"
	"daiyaku/internal/neutral"
	tea "github.com/charmbracelet/bubbletea"
)

func TestTUIViewDump(t *testing.T) {
	eng := engine.New(1)
	req := &neutral.Request{Provider: "openai", Model: "gpt-5-codex", Seq: 3,
		System: "You are a coding agent running in Codex CLI.",
		Tools: []neutral.ToolDef{
			{Name: "exec_command", Kind: "function", Description: "Runs a command in a PTY, returning output or a session ID.",
				Schema: json.RawMessage(`{"type":"object","properties":{"cmd":{"type":"string"}},"required":["cmd"]}`)},
			{Name: "write_stdin", Kind: "function", Description: "Write to a running session's stdin."},
			{Name: "multi_agent_v1", Kind: "namespace", Description: "Tools for spawning sub-agents."},
			{Name: "close_agent", Kind: "function", Namespace: "multi_agent_v1", Description: "Close an agent."},
			{Name: "web_search", Kind: "web_search", Description: ""},
		},
		Turns: []neutral.Turn{
			{Role: "user", Blocks: []neutral.Block{{Type: neutral.BlockText, Text: "Identify the current user."}}},
			{Role: "assistant", Blocks: []neutral.Block{{Type: neutral.BlockToolCall, Call: &neutral.ToolCall{Name: "exec_command", Input: json.RawMessage(`{"cmd":"whoami"}`)}}}},
			{Role: "tool", Blocks: []neutral.Block{{Type: neutral.BlockToolResult, Result: &neutral.ToolResult{Content: "DESKTOP\\user"}}}},
		},
	}
	ex := &engine.Exchange{Req: req}
	var m tea.Model = newModel(&TUI{eng: eng, provider: "openai", recordPath: "chain.json"})
	m, _ = m.Update(tea.WindowSizeMsg{Width: 110, Height: 32})
	m, _ = m.Update(exchangeMsg{ex})
	mm := m.(model)
	mm.composer.SetValue(`{"cmd":"whoami"}`)
	out := mm.View()
	for _, want := range []string{
		"offered tools", "exec_command", "[namespace]", "multi_agent_v1.close_agent",
		"ACT - call a tool", "AWAITING OPERATOR", "REC",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("view missing %q", want)
		}
	}
}
