// Package neutral defines the provider-neutral internal representation of an
// inference request and the operator's authored response. Adapters translate
// between a provider's wire format and these types; nothing else in the
// codebase needs to know which provider is in play.
package neutral

import "encoding/json"

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
	Provider  string            `json:"provider"`
	Model     string            `json:"model"`
	System    string            `json:"system,omitempty"`
	Turns     []Turn            `json:"turns"`
	Tools     []ToolDef         `json:"tools"`
	Stream    bool              `json:"stream"`
	MaxTokens int               `json:"max_tokens,omitempty"`
	Seq       int               `json:"seq"`
	Headers   map[string]string `json:"headers"` // selected subset, kept for evidence
	Raw       []byte            `json:"-"`
}

type ActionKind string

const (
	ActionToolCall ActionKind = "tool_call"
	ActionText     ActionKind = "text"
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
