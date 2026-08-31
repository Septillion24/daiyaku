// Package sequence handles canned tool-call sequences: ordered lists of operator
// actions that can be replayed automatically. Shipping the sequence file with a
// report is what makes each finding reproducible after a config change.
package sequence

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"

	"daiyaku/internal/neutral"
)

// Exactly one of Tool or Text is set.
type Step struct {
	Note  string          `json:"note,omitempty"` // ignored on replay
	Tool  string          `json:"tool,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	Text  string          `json:"text,omitempty"`
	End   bool            `json:"end,omitempty"`
}

func (s Step) Action() neutral.Action {
	if s.Tool != "" {
		return neutral.Action{Kind: neutral.ActionToolCall, ToolName: s.Tool, ToolInput: s.Input}
	}
	kind := neutral.ActionText
	if s.End {
		kind = neutral.ActionEnd
	}
	return neutral.Action{Kind: kind, Text: s.Text}
}

func FromAction(a neutral.Action, note string) Step {
	s := Step{Note: note}
	switch a.Kind {
	case neutral.ActionToolCall:
		s.Tool = a.ToolName
		s.Input = a.ToolInput
	case neutral.ActionEnd:
		s.Text = a.Text
		s.End = true
	default:
		s.Text = a.Text
	}
	return s
}

type File struct {
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Provider    string `json:"provider,omitempty"`
	Steps       []Step `json:"steps"`
}

// Load accepts either the wrapped {steps:[...]} form or a bare top-level array.
func Load(path string) (*File, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// First non-space byte picks the form: '[' bare array, '{' wrapped object
	// (valid even if its "steps" is absent/null).
	trimmed := bytes.TrimSpace(b)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var steps []Step
		if err := json.Unmarshal(b, &steps); err != nil {
			return nil, fmt.Errorf("parse sequence: %w", err)
		}
		return &File{Steps: steps}, nil
	}
	var f File
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("parse sequence: %w", err)
	}
	return &f, nil
}

func Save(path string, f *File) error {
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// If the file exists but cannot be parsed, AppendStep refuses rather than
// overwriting (which would silently destroy an existing recording).
func AppendStep(path string, step Step) error {
	f := &File{}
	if _, statErr := os.Stat(path); statErr == nil {
		existing, err := Load(path)
		if err != nil {
			return fmt.Errorf("refusing to overwrite unparseable recording %s: %w", path, err)
		}
		f = existing
	}
	f.Steps = append(f.Steps, step)
	return Save(path, f)
}
