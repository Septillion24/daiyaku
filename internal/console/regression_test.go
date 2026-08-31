package console

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"daiyaku/internal/engine"
	"daiyaku/internal/neutral"
	"daiyaku/internal/sequence"
)

func bashReq(seq int) *neutral.Request {
	return &neutral.Request{Provider: "anthropic", Model: "m", Seq: seq,
		Tools: []neutral.ToolDef{{Name: "Bash", Kind: "function",
			Schema: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`)}}}
}

func newSizedModel(t *TUI) tea.Model {
	var m tea.Model = newModel(t)
	m, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	return m
}

// Ctrl+E means "answer in words and end the turn". It must not fall through to
// the tool path and run the operator's sentence as a shell command.
func TestCtrlESendsTextNotAToolCall(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	eng := engine.New(1)
	result := make(chan neutral.Action, 1)
	go func() {
		if a, err := eng.Submit(ctx, bashReq(1)); err == nil {
			result <- a
		}
	}()
	ex, err := eng.Next(ctx)
	if err != nil {
		t.Fatalf("next: %v", err)
	}

	m := newSizedModel(&TUI{eng: eng, provider: "anthropic"})
	m, _ = m.Update(exchangeMsg{ex})
	mm := m.(model)
	mm.composer.SetValue("stopping here")
	mm.Update(tea.KeyMsg{Type: tea.KeyCtrlE})

	select {
	case a := <-result:
		if a.Kind != neutral.ActionEnd {
			t.Fatalf("ctrl+e produced %s (%s %s), want an end-turn text reply",
				a.Kind, a.ToolName, a.ToolInput)
		}
		if a.Text != "stopping here" {
			t.Errorf("text = %q", a.Text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no action reached the harness")
	}
}

// A second request arriving while one is unanswered must queue, not overwrite:
// the overwritten one would block its HTTP handler until the harness timed out.
func TestConcurrentRequestsAreQueuedNotDropped(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	eng := engine.New(4)
	answered := make(chan int, 2)
	for i := 1; i <= 2; i++ {
		i := i
		go func() {
			if _, err := eng.Submit(ctx, bashReq(i)); err == nil {
				answered <- i
			}
		}()
	}
	ex1, _ := eng.Next(ctx)
	ex2, _ := eng.Next(ctx)

	m := newSizedModel(&TUI{eng: eng, provider: "anthropic"})
	m, _ = m.Update(exchangeMsg{ex1})
	m, _ = m.Update(exchangeMsg{ex2})
	if q := len(m.(model).queue); q != 1 {
		t.Fatalf("queue depth = %d, want 1", q)
	}
	for i := 0; i < 2; i++ {
		mm := m.(model)
		mm.composer.SetValue(`{"command":"id"}`)
		m, _ = mm.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	}
	for i := 0; i < 2; i++ {
		select {
		case <-answered:
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d of 2 requests were answered", i)
		}
	}
}

// An action the harness never received is not evidence, so it must not be
// written to the --record chain.
func TestUndeliveredActionIsNotRecorded(t *testing.T) {
	rec := filepath.Join(t.TempDir(), "chain.json")
	eng := engine.New(1)
	reqCtx, reqCancel := context.WithCancel(context.Background())
	go func() { eng.Submit(reqCtx, bashReq(1)) }()

	ex, err := eng.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	m := newSizedModel(&TUI{eng: eng, provider: "anthropic", recordPath: rec})
	m, _ = m.Update(exchangeMsg{ex})

	reqCancel() // harness goes away while the operator is composing
	time.Sleep(50 * time.Millisecond)

	mm := m.(model)
	mm.composer.SetValue(`{"command":"id"}`)
	m, _ = mm.Update(tea.KeyMsg{Type: tea.KeyCtrlS})

	if _, err := os.Stat(rec); !os.IsNotExist(err) {
		b, _ := os.ReadFile(rec)
		t.Fatalf("undelivered action was recorded as evidence: %s", b)
	}
	if got := m.(model).status; !strings.Contains(got, "NOT DELIVERED") {
		t.Errorf("status = %q, want it to say the action was not delivered", got)
	}
	if m.(model).sent != 0 {
		t.Errorf("sent counter advanced for an undelivered action")
	}
}

// The bare-text shorthand must always fill the same field for the same schema.
func TestPrimaryFieldIsDeterministic(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{
		"alpha":{"type":"string"},"beta":{"type":"string"},"gamma":{"type":"string"},
		"delta":{"type":"string"},"epsilon":{"type":"string"}}}`)
	first := primaryField(schema)
	for i := 0; i < 200; i++ {
		if got := primaryField(schema); got != first {
			t.Fatalf("primaryField returned %q then %q for the same schema", first, got)
		}
	}
	if first != "alpha" {
		t.Errorf("primaryField = %q, want the first string property in sorted order", first)
	}
}

// Ctrl+T on a tool whose schema is absent or not a plain object used to load the
// literal "null", which is not an editable starting point for a call.
func TestTemplateNeverEmitsNull(t *testing.T) {
	req := &neutral.Request{Tools: []neutral.ToolDef{
		{Name: "web_search", Kind: "web_search"},
		{Name: "Union", Schema: json.RawMessage(`{"type":["object","null"]}`)},
		{Name: "Ref", Schema: json.RawMessage(`{"$ref":"#/defs/x"}`)},
	}}
	for _, n := range []string{"web_search", "Union", "Ref", "Absent"} {
		if got := Template(req, n); got == "null" {
			t.Errorf("Template(%s) = null", n)
		}
		if got := TemplateCompact(req, n); got != "{}" {
			t.Errorf("TemplateCompact(%s) = %s, want {}", n, got)
		}
	}
}

// Replaying a recorded chain must round-trip through the loader's validation.
func TestRecordedChainReloads(t *testing.T) {
	rec := filepath.Join(t.TempDir(), "chain.json")
	for _, a := range []neutral.Action{
		{Kind: neutral.ActionToolCall, ToolName: "Bash", ToolInput: json.RawMessage(`{"command":"id"}`)},
		{Kind: neutral.ActionEnd, Text: "done"},
	} {
		if err := sequence.AppendStep(rec, sequence.FromAction(a, "")); err != nil {
			t.Fatal(err)
		}
	}
	f, err := sequence.Load(rec)
	if err != nil {
		t.Fatalf("a chain daiyaku recorded does not reload: %v", err)
	}
	if len(f.Steps) != 2 {
		t.Errorf("steps = %d, want 2", len(f.Steps))
	}
}
