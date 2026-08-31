package console

import (
	"encoding/json"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"daiyaku/internal/engine"
	"daiyaku/internal/neutral"
)

func lipglossWidth(s string) int { return lipgloss.Width(s) }

func TestRenderRealistic(t *testing.T) {
	longsys := "You are Claude Code. " + strings.Repeat("This is a very long system prompt line that would previously run off the right edge of the pane without any wrapping and could not be scrolled. ", 6)
	req := &neutral.Request{Provider: "anthropic", Model: "claude-sonnet-4-6", Seq: 1,
		System: longsys,
		Tools: []neutral.ToolDef{
			{Name: "Agent", Kind: "function", Description: "Launch a subagent", Schema: json.RawMessage(`{"type":"object","properties":{"prompt":{"type":"string"},"subagent_type":{"type":"string"}},"required":["prompt"]}`)},
			{Name: "Bash", Kind: "function", Description: "Run a shell command in a persistent session with a very long description that keeps going and going to test wrapping in the tools description area too.", Schema: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`)},
			{Name: "Read", Kind: "function", Description: "Read a file"},
		},
		Turns: []neutral.Turn{{Role: "user", Blocks: []neutral.Block{{Type: neutral.BlockText, Text: strings.Repeat("Please migrate the schema safely. ", 20)}}}},
	}
	ex := &engine.Exchange{Req: req}
	var m tea.Model = newModel(&TUI{provider: "anthropic", recordPath: "chain.json"})
	m, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m, _ = m.Update(exchangeMsg{ex})
	mm := m.(model)
	out := mm.View()
	lines := strings.Split(out, "\n")
	// Every visible line must fit the width (no horizontal overflow).
	over := 0
	for _, l := range lines {
		if lipglossWidth(l) > 120 { over++ }
	}
	t.Logf("total lines=%d width=120 height=40 overflow_lines=%d selected_tool=%s", len(lines), over, mm.selectedTool().Name)
	t.Logf("\n%s", out)
	if over > 0 { t.Errorf("%d lines exceed terminal width", over) }
	if len(lines) > 40 { t.Errorf("view is %d lines, exceeds height 40 (would push header off-screen)", len(lines)) }
	if mm.selectedTool().Name != "Bash" { t.Errorf("expected Bash auto-selected, got %s", mm.selectedTool().Name) }
}
