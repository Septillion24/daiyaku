package sequence

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"daiyaku/internal/neutral"
)

func writeFile(path, content string) error { return os.WriteFile(path, []byte(content), 0o644) }

func TestStepActionRoundTrip(t *testing.T) {
	call := neutral.Action{Kind: neutral.ActionToolCall, ToolName: "Bash",
		ToolInput: json.RawMessage(`{"command":"id"}`)}
	got := FromAction(call, "recon").Action()
	if got.Kind != call.Kind || got.ToolName != call.ToolName {
		t.Errorf("call round-trip = %+v", got)
	}
	end := neutral.Action{Kind: neutral.ActionEnd, Text: "done"}
	if FromAction(end, "").Action().Kind != neutral.ActionEnd {
		t.Errorf("end round-trip lost kind")
	}
}

func TestLoadBareArrayAndWrapped(t *testing.T) {
	dir := t.TempDir()
	bare := filepath.Join(dir, "bare.json")
	if err := writeFile(bare, `[{"tool":"Bash","input":{"command":"id"}}]`); err != nil {
		t.Fatal(err)
	}
	f, err := Load(bare)
	if err != nil || len(f.Steps) != 1 || f.Steps[0].Tool != "Bash" {
		t.Fatalf("bare load: %v %+v", err, f)
	}

	wrapped := filepath.Join(dir, "w.json")
	if err := Save(wrapped, &File{Name: "x", Steps: []Step{{Text: "hi"}}}); err != nil {
		t.Fatal(err)
	}
	f2, err := Load(wrapped)
	if err != nil || f2.Name != "x" || f2.Steps[0].Text != "hi" {
		t.Fatalf("wrapped load: %v %+v", err, f2)
	}
	if got := f2.Steps[0].Action().Kind; got != neutral.ActionEnd {
		t.Errorf("a text step produced %q, want an end-turn action", got)
	}
}

func TestAppendStep(t *testing.T) {
	p := filepath.Join(t.TempDir(), "rec.json")
	for i := 0; i < 3; i++ {
		if err := AppendStep(p, Step{Tool: "Bash", Input: json.RawMessage(`{}`)}); err != nil {
			t.Fatal(err)
		}
	}
	f, err := Load(p)
	if err != nil || len(f.Steps) != 3 {
		t.Fatalf("append: %v steps=%d", err, len(f.Steps))
	}
}

func TestAppendStepRefusesClobber(t *testing.T) {
	p := filepath.Join(t.TempDir(), "rec.json")
	if err := writeFile(p, "this is not json at all"); err != nil {
		t.Fatal(err)
	}
	if err := AppendStep(p, Step{Tool: "Bash"}); err == nil {
		t.Error("AppendStep should refuse to overwrite an unparseable recording")
	}
	b, _ := os.ReadFile(p)
	if string(b) != "this is not json at all" {
		t.Errorf("existing recording was clobbered: %q", b)
	}
}

func TestLoadWrappedNoSteps(t *testing.T) {
	p := filepath.Join(t.TempDir(), "meta.json")
	if err := writeFile(p, `{"name":"x","description":"d"}`); err != nil {
		t.Fatal(err)
	}
	f, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if f.Name != "x" || len(f.Steps) != 0 {
		t.Errorf("got %+v", f)
	}
}
