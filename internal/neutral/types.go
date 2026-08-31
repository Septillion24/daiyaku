// Package neutral defines the provider-neutral internal representation of an
// inference request and the operator's authored response. Adapters translate
// between a provider's wire format and these types; nothing else in the
// codebase needs to know which provider is in play.
package neutral

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type BlockType string

const (
	BlockText       BlockType = "text"
	BlockToolCall   BlockType = "tool_call"
	BlockToolResult BlockType = "tool_result"
)

// Input is normalized to a JSON object: OpenAI sends it as a JSON-encoded string
// on the wire, and adapters decode it here so the console sees a real object.
type ToolCall struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

type ToolResult struct {
	CallID  string `json:"call_id"`
	Name    string `json:"name,omitempty"`
	Content string `json:"content"`
	IsError bool   `json:"is_error,omitempty"`
}

type Block struct {
	Type   BlockType   `json:"type"`
	Text   string      `json:"text,omitempty"`
	Call   *ToolCall   `json:"call,omitempty"`
	Result *ToolResult `json:"result,omitempty"`
}

type Turn struct {
	Role   string  `json:"role"`
	Blocks []Block `json:"blocks"`
}

// Schema is the raw JSON schema exactly as the harness sent it; the diff between
// what is offered and what you expected is itself a finding.
type ToolDef struct {
	Name        string          `json:"name"`
	Kind        string          `json:"kind,omitempty"` // function, namespace, web_search, ...
	Namespace   string          `json:"namespace,omitempty"`
	Description string          `json:"description,omitempty"`
	Schema      json.RawMessage `json:"schema,omitempty"`
}

func (t ToolDef) Label() string {
	if t.Namespace != "" {
		return t.Namespace + "." + t.Name
	}
	return t.Name
}

type Request struct {
	Provider  string    `json:"provider"`
	Model     string    `json:"model"`
	System    string    `json:"system,omitempty"`
	Turns     []Turn    `json:"turns"`
	Tools     []ToolDef `json:"tools"`
	Stream    bool      `json:"stream"`
	MaxTokens int       `json:"max_tokens,omitempty"`
	// StopSequences is part of how a harness side-call identifies itself; see
	// IsSafetyClassifier.
	StopSequences []string          `json:"stop_sequences,omitempty"`
	Seq           int               `json:"seq"`
	Headers       map[string]string `json:"headers"` // selected subset, kept for evidence
	Raw           []byte            `json:"-"`
}

type ActionKind string

// There is no "speak and continue" action: neither wire format lets an assistant
// message keep the turn open, so any words the operator sends end it.
const (
	ActionToolCall ActionKind = "tool_call"
	ActionEnd      ActionKind = "end" // assistant text with end_turn stop reason
)

type Action struct {
	Kind      ActionKind      `json:"kind"`
	ToolName  string          `json:"tool_name,omitempty"`
	ToolInput json.RawMessage `json:"tool_input,omitempty"`
	Text      string          `json:"text,omitempty"`
}

func (r *Request) ToolNames() []string {
	out := make([]string, len(r.Tools))
	for i, t := range r.Tools {
		out[i] = t.Label()
	}
	return out
}

func (r *Request) FindTool(name string) *ToolDef {
	for i := range r.Tools {
		if r.Tools[i].Name == name || r.Tools[i].Label() == name {
			return &r.Tools[i]
		}
	}
	return nil
}

func (r *Request) LastResult() *ToolResult {
	for i := len(r.Turns) - 1; i >= 0; i-- {
		blocks := r.Turns[i].Blocks
		for j := len(blocks) - 1; j >= 0; j-- {
			if blocks[j].Type == BlockToolResult {
				return blocks[j].Result
			}
		}
	}
	return nil
}

// The harness fires side-channel calls that are not turns in the agent
// conversation: Claude Code's auto-approval safety classifier grades a pending
// tool action and expects "<severity>N</severity>" back on a tight deadline.
// Left for a human operator it stalls, and the harness reports the model as
// unavailable, so daiyaku answers it automatically (see engine.Engine.Auto).
//
// Recognition deliberately does not hang on one sentence of prose. That prose is
// rewritten every harness release, and when the match silently stops the call
// lands in the operator's queue and the harness wedges, with nothing to say why.
// Three independent marks are each sufficient on their own, so the call is still
// recognized when any one of them survives a rewrite. severityTag and
// severityStopSequence are protocol rather than wording: the harness parses its
// own reply for them, so changing them would break its own parser.
const (
	classifierSentinel   = "You are a security monitor for autonomous AI coding agents"
	severityTag          = "<severity>"
	severityStopSequence = "</severity>"
	// A budget this small cannot hold a turn's worth of assistant output, so it
	// marks a side-call. On its own it means little (any short call qualifies),
	// so it only ever raises the operator's warning, never auto-answers.
	sideCallMaxTokens = 256 // the classifier is observed at 64; the grade is one digit
)

// IsSafetyClassifier reports whether req is a harness side-call to answer
// automatically rather than a turn to put in front of the operator. Offering no
// tools at all is required: a real agent turn always offers tools, so that gate
// alone excludes the conversation and the marks below only have to tell one kind
// of side-call from another.
func (r *Request) IsSafetyClassifier() bool {
	return len(r.Tools) == 0 && r.hasClassifierMark()
}

// hasClassifierMark reports any of the self-sufficient marks. Each is specific
// enough that a genuine turn will not carry it by accident, so any single
// survivor keeps the harness moving after a wording change.
func (r *Request) hasClassifierMark() bool {
	for _, s := range r.StopSequences {
		if strings.Contains(s, severityStopSequence) {
			return true
		}
	}
	return strings.Contains(r.System, severityTag) ||
		strings.Contains(r.System, classifierSentinel)
}

// MayBeSideCall reports a tool-less request that looks like a side-call by
// budget alone: too weak a signal to answer automatically, but enough that the
// operator should be told the shape may have drifted. Without this, a match that
// stops working shows up only as a harness that mysteriously stalls.
func (r *Request) MayBeSideCall() bool {
	return len(r.Tools) == 0 && !r.hasClassifierMark() &&
		r.MaxTokens > 0 && r.MaxTokens <= sideCallMaxTokens
}

// Validate reports whether the action can be serialized to a provider's wire
// format at all. Both wire formats require a tool call's input to be a JSON
// object (Anthropic sends it inline, OpenAI as a JSON-encoded string), so an
// operator typo or a hand-written sequence file carrying a fragment, a bare
// string, or an array would otherwise be emitted verbatim: the blocking encoder
// fails after the 200 header is already out, and the streaming path ships the
// broken fragment as partial_json. Both leave the harness with an unexplainable
// parse error, so the mock refuses the action instead.
func (a Action) Validate() error {
	if a.Kind != ActionToolCall {
		return nil
	}
	if a.ToolName == "" {
		return errors.New("tool call has no tool name")
	}
	if len(a.ToolInput) == 0 {
		return nil // adapters substitute {}
	}
	if !json.Valid(a.ToolInput) {
		return fmt.Errorf("tool input for %s is not valid JSON: %s", a.ToolName, truncateForError(a.ToolInput))
	}
	trimmed := bytes.TrimSpace(a.ToolInput)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return fmt.Errorf("tool input for %s must be a JSON object, got %s", a.ToolName, truncateForError(a.ToolInput))
	}
	return nil
}

func truncateForError(b []byte) string {
	const n = 120
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}
