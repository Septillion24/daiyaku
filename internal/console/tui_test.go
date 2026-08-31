package console

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"daiyaku/internal/engine"
	"daiyaku/internal/neutral"
)

// Ctrl+S must deliver the composed tool call back to the blocked Submit caller.
func TestTUISendsToolCall(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	eng := engine.New(1)

	req := &neutral.Request{
		Provider: "anthropic", Model: "m",
		Tools: []neutral.ToolDef{{Name: "Bash", Kind: "function",
			Schema: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}}}`)}},
		Turns: []neutral.Turn{{Role: "user", Blocks: []neutral.Block{{Type: neutral.BlockText, Text: "hi"}}}},
	}

	result := make(chan neutral.Action, 1)
	go func() {
		a, err := eng.Submit(ctx, req)
		if err == nil {
			result <- a
		}
	}()

	ex, err := eng.Next(ctx)
	if err != nil {
		t.Fatalf("next: %v", err)
	}

	var m tea.Model = newModel(&TUI{eng: eng, provider: "anthropic"})
	m, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m, _ = m.Update(exchangeMsg{ex})

	mm := m.(model)
	mm.composer.SetValue(`{"command":"whoami"}`)
	m, _ = mm.Update(tea.KeyMsg{Type: tea.KeyCtrlS})

	select {
	case a := <-result:
		if a.Kind != neutral.ActionToolCall || a.ToolName != "Bash" {
			t.Fatalf("action = %+v", a)
		}
		if !strings.Contains(string(a.ToolInput), "whoami") {
			t.Errorf("input = %s", a.ToolInput)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("operator action never reached the Submit caller")
	}
}

// Plain Enter (not only Ctrl+S) sends the composed tool call.
func TestTUIEnterSends(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	eng := engine.New(1)
	req := &neutral.Request{Provider: "anthropic", Model: "m",
		Tools: []neutral.ToolDef{{Name: "Bash", Kind: "function",
			Schema: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}}}`)}}}
	result := make(chan neutral.Action, 1)
	go func() {
		if a, err := eng.Submit(ctx, req); err == nil {
			result <- a
		}
	}()
	ex, _ := eng.Next(ctx)
	var m tea.Model = newModel(&TUI{eng: eng, provider: "anthropic"})
	m, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m, _ = m.Update(exchangeMsg{ex})
	mm := m.(model)
	mm.composer.SetValue(`{"command":"id"}`)
	m, _ = mm.Update(tea.KeyMsg{Type: tea.KeyEnter})
	select {
	case a := <-result:
		if a.Kind != neutral.ActionToolCall || !strings.Contains(string(a.ToolInput), "id") {
			t.Fatalf("action = %+v", a)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Enter did not send")
	}
}

// System prompt is hidden by default; 's' reveals it.
func TestTUISystemToggle(t *testing.T) {
	eng := engine.New(1)
	req := &neutral.Request{Provider: "anthropic", Model: "m", System: "SECRET-SYSTEM-PROMPT",
		Turns: []neutral.Turn{{Role: "user", Blocks: []neutral.Block{{Type: neutral.BlockText, Text: "hi"}}}}}
	ex := &engine.Exchange{Req: req}
	var m tea.Model = newModel(&TUI{eng: eng, provider: "anthropic"})
	m, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m, _ = m.Update(exchangeMsg{ex})
	mm := m.(model)
	if mm.showSystem {
		t.Fatal("system should be hidden by default")
	}
	if !strings.Contains(mm.View(), "system prompt hidden") {
		t.Error("expected hidden-system hint in view")
	}
	if strings.Contains(mm.View(), "SECRET-SYSTEM-PROMPT") {
		t.Error("system prompt should not be shown by default")
	}
	mm.focus = focusContext
	m2, _ := mm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	if !m2.(model).showSystem {
		t.Fatal("'s' should reveal the system prompt")
	}
	if !strings.Contains(m2.(model).View(), "SECRET-SYSTEM-PROMPT") {
		t.Error("system prompt should be visible after toggle")
	}
}

// Ctrl+E in text mode ends the turn.
func TestTUITextEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	eng := engine.New(1)
	req := &neutral.Request{Provider: "anthropic", Model: "m"}
	result := make(chan neutral.Action, 1)
	go func() {
		if a, err := eng.Submit(ctx, req); err == nil {
			result <- a
		}
	}()
	ex, _ := eng.Next(ctx)

	var m tea.Model = newModel(&TUI{eng: eng, provider: "anthropic"})
	m, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m, _ = m.Update(exchangeMsg{ex})
	mm := m.(model)
	mm.compose = modeText
	mm.composer.SetValue("all done")
	m, _ = mm.Update(tea.KeyMsg{Type: tea.KeyCtrlE})

	select {
	case a := <-result:
		if a.Kind != neutral.ActionEnd || a.Text != "all done" {
			t.Fatalf("action = %+v", a)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("text-end action never delivered")
	}
}

// Composer starts at two lines, grows with Alt+Enter without exceeding the height, then collapses after send.
func TestTUIComposerGrows(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	eng := engine.New(1)
	req := &neutral.Request{Provider: "anthropic", Model: "m",
		Tools: []neutral.ToolDef{{Name: "Bash", Kind: "function",
			Schema: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}}}`)}}}
	result := make(chan neutral.Action, 1)
	go func() {
		if a, err := eng.Submit(ctx, req); err == nil {
			result <- a
		}
	}()
	ex, _ := eng.Next(ctx)

	var m tea.Model = newModel(&TUI{eng: eng, provider: "anthropic"})
	m, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m, _ = m.Update(exchangeMsg{ex})

	if got := m.(model).composerRows; got != composerMinRows {
		t.Fatalf("default composerRows = %d, want %d", got, composerMinRows)
	}

	// Alt+Enter (KeyEnter+Alt) stringifies to "alt+enter" and inserts a newline.
	for i := 0; i < 3; i++ {
		m, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter, Alt: true})
	}
	if got := m.(model).composerRows; got <= composerMinRows {
		t.Fatalf("after 3 Alt+Enter composerRows = %d, want > %d", got, composerMinRows)
	}
	if lines := strings.Split(m.(model).View(), "\n"); len(lines) > 30 {
		t.Errorf("grown view has %d lines > height 30", len(lines))
	}

	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	select {
	case <-result:
	case <-time.After(2 * time.Second):
		t.Fatal("Enter did not send the multi-line buffer")
	}
	if got := m.(model).composerRows; got != composerMinRows {
		t.Errorf("after send composerRows = %d, want %d", got, composerMinRows)
	}
}

// Small/narrow terminals must not panic or overflow (no line exceeds width or height).
func TestTUISmallTerminal(t *testing.T) {
	for _, sz := range [][2]int{{20, 8}, {40, 10}, {30, 12}, {50, 16}, {80, 24}} {
		var m tea.Model = newModel(&TUI{provider: "anthropic"})
		m, _ = m.Update(tea.WindowSizeMsg{Width: sz[0], Height: sz[1]})
		out := m.(model).View() // must not panic
		lines := strings.Split(out, "\n")
		if len(lines) > sz[1] {
			t.Errorf("size %v: %d lines > height", sz, len(lines))
		}
		for _, l := range lines {
			if lipglossWidth(l) > sz[0] {
				t.Errorf("size %v: line width %d > %d: %q", sz, lipglossWidth(l), sz[0], l)
			}
		}
	}
}

// A tool that vanishes between turns is a finding, so the pane has to name it.
// The '+' marker can only report arrivals; departures have no row to sit on.
func TestTUIToolsPaneReportsRemovals(t *testing.T) {
	tool := func(name string) neutral.ToolDef {
		return neutral.ToolDef{Name: name, Kind: "function",
			Schema: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}}}`)}
	}

	var m tea.Model = newModel(&TUI{provider: "anthropic"})
	m, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	mm := m.(model)
	mm.ex = &engine.Exchange{Req: &neutral.Request{Provider: "anthropic"}}
	mm.tools = []neutral.ToolDef{tool("Bash"), tool("Read")}
	mm.prevTools = []string{"Read", "WebFetch", "Write"}

	pane := mm.toolsPane()
	if !strings.Contains(pane, "gone:") {
		t.Fatalf("no removal line in pane:\n%s", pane)
	}
	for _, want := range []string{"WebFetch", "Write"} {
		if !strings.Contains(pane, want) {
			t.Errorf("removed tool %q not named:\n%s", want, pane)
		}
	}

	// A turn that removes nothing must not spend a row on an empty line.
	mm.prevTools = []string{"Bash", "Read"}
	if got := mm.toolsPane(); strings.Contains(got, "gone:") {
		t.Errorf("removal line drawn with nothing removed:\n%s", got)
	}
}
