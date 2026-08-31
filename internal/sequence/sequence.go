// Package sequence handles canned tool-call sequences: ordered lists of operator
// actions that can be replayed automatically. Shipping the sequence file with a
// report is what makes each finding reproducible after a config change.
package sequence

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"daiyaku/internal/neutral"
)

// Exactly one of Tool or Text is set. A "text" step always ends the harness's
// turn: neither wire format has a way for the model to speak and keep going, so
// there is no flag to control it. Older files carrying "end": true still load;
// the field is simply ignored.
type Step struct {
	Note  string          `json:"note,omitempty"` // ignored on replay
	Tool  string          `json:"tool,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	Text  string          `json:"text,omitempty"`
}

func (s Step) Action() neutral.Action {
	if s.Tool != "" {
		return neutral.Action{Kind: neutral.ActionToolCall, ToolName: s.Tool, ToolInput: s.Input}
	}
	return neutral.Action{Kind: neutral.ActionEnd, Text: s.Text}
}

func FromAction(a neutral.Action, note string) Step {
	s := Step{Note: note}
	switch a.Kind {
	case neutral.ActionToolCall:
		s.Tool = a.ToolName
		s.Input = a.ToolInput
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
		f := &File{Steps: steps}
		if err := f.Validate(); err != nil {
			return nil, err
		}
		return f, nil
	}
	var f File
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("parse sequence: %w", err)
	}
	if err := f.Validate(); err != nil {
		return nil, err
	}
	return &f, nil
}

// Validate rejects steps that would silently misbehave at replay time: a step
// with neither a tool nor text quietly ends the harness turn, and a tool input
// that is not a JSON object cannot be serialized to either wire format. Both are
// far cheaper to catch when the file is loaded than three steps into a run.
func (f *File) Validate() error {
	for i, s := range f.Steps {
		if s.Tool == "" && s.Text == "" {
			return fmt.Errorf("step %d has neither \"tool\" nor \"text\" (note: %q)", i+1, s.Note)
		}
		if s.Tool != "" && s.Text != "" {
			return fmt.Errorf("step %d sets both \"tool\" and \"text\"; use one per step", i+1)
		}
		if err := s.Action().Validate(); err != nil {
			return fmt.Errorf("step %d: %w", i+1, err)
		}
	}
	return nil
}

// Save writes via a temp file and a rename so an interrupted write cannot leave
// a half-written (or empty) recording where a complete one used to be: the file
// is rewritten in full after every recorded step.
func Save(path string, f *File) error {
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".daiyaku-seq-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// Windows will not rename onto an existing file.
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.Rename(tmpName, path)
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
