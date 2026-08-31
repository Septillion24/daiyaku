// Package openai implements the adapter for the OpenAI Responses API, the wire
// protocol Codex CLI uses (wire_api = "responses"). Codex is open source, so the
// exact request construction can be read from its repository; validate the
// shapes here against a Step-0 capture before an engagement.
package openai

import (
	"encoding/json"
	"fmt"
	"net/http"

	"daiyaku/internal/adapter"
	"daiyaku/internal/neutral"
)

func init() { adapter.Register("openai", func() adapter.Adapter { return &Adapter{} }) }

type Adapter struct{}

func (a *Adapter) Provider() string { return "openai" }

func (a *Adapter) Routes() adapter.Routes {
	return adapter.Routes{
		Primary: "POST /v1/responses",
		Aux: map[string]http.HandlerFunc{
			"GET /v1/models": listModels,
		},
	}
}

type wireRequest struct {
	Model        string          `json:"model"`
	Instructions string          `json:"instructions"`
	Input        json.RawMessage `json:"input"` // string or []item
	Tools        []wireTool      `json:"tools"`
	Stream       bool            `json:"stream"`
}

type wireTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Tools       []wireTool      `json:"tools"` // present on "namespace" tools
	// Chat-style nesting fallback: {"type":"function","function":{...}}
	Function *struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type wireItem struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"` // string or []contentPart
	Name      string          `json:"name"`
	CallID    string          `json:"call_id"`
	Arguments string          `json:"arguments"`
	Output    json.RawMessage `json:"output"` // string or structured
}

type contentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func (a *Adapter) Normalize(_ http.Header, body []byte) (*neutral.Request, error) {
	var wr wireRequest
	if err := json.Unmarshal(body, &wr); err != nil {
		return nil, fmt.Errorf("decode request: %w", err)
	}
	req := &neutral.Request{
		Provider: "openai",
		Model:    wr.Model,
		Stream:   wr.Stream,
		System:   wr.Instructions,
	}
	for _, t := range wr.Tools {
		req.Tools = append(req.Tools, flattenTool(t, "")...)
	}
	if err := decodeInput(wr.Input, req); err != nil {
		return nil, err
	}
	return req, nil
}

// Namespace tools are expanded so sub-tools are individually visible/callable; a
// header entry for the namespace itself is also emitted so the offered surface is
// fully recorded (matters for the enumeration finding).
func flattenTool(t wireTool, namespace string) []neutral.ToolDef {
	switch t.Type {
	case "namespace":
		out := []neutral.ToolDef{{Name: t.Name, Kind: "namespace", Description: t.Description}}
		for _, sub := range t.Tools {
			out = append(out, flattenTool(sub, t.Name)...)
		}
		return out
	case "function", "":
		td := neutral.ToolDef{Name: t.Name, Kind: "function", Namespace: namespace,
			Description: t.Description, Schema: t.Parameters}
		if t.Function != nil {
			td.Name = t.Function.Name
			td.Description = t.Function.Description
			td.Schema = t.Function.Parameters
		}
		return []neutral.ToolDef{td}
	default:
		name := t.Name
		if name == "" {
			name = t.Type
		}
		return []neutral.ToolDef{{Name: name, Kind: t.Type, Namespace: namespace, Description: t.Description}}
	}
}

func decodeInput(raw json.RawMessage, req *neutral.Request) error {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		req.Turns = append(req.Turns, neutral.Turn{Role: "user",
			Blocks: []neutral.Block{{Type: neutral.BlockText, Text: s}}})
		return nil
	}
	var items []wireItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return fmt.Errorf("decode input items: %w", err)
	}
	for _, it := range items {
		switch it.Type {
		case "message", "":
			blocks := decodeParts(it.Content)
			req.Turns = append(req.Turns, neutral.Turn{Role: orDefault(it.Role, "user"), Blocks: blocks})
		case "function_call":
			// arguments is a JSON-encoded string on the wire; normalize to an
			// object. Empty or invalid arguments become {} (not the string "").
			input := json.RawMessage(it.Arguments)
			if it.Arguments == "" || !json.Valid(input) {
				input = json.RawMessage("{}")
			}
			req.Turns = append(req.Turns, neutral.Turn{Role: "assistant",
				Blocks: []neutral.Block{{Type: neutral.BlockToolCall, Call: &neutral.ToolCall{
					ID: it.CallID, Name: it.Name, Input: input,
				}}}})
		case "function_call_output":
			req.Turns = append(req.Turns, neutral.Turn{Role: "tool",
				Blocks: []neutral.Block{{Type: neutral.BlockToolResult, Result: &neutral.ToolResult{
					CallID: it.CallID, Content: stringifyOutput(it.Output),
				}}}})
		default:
			req.Turns = append(req.Turns, neutral.Turn{Role: "system",
				Blocks: []neutral.Block{{Type: neutral.BlockText, Text: fmt.Sprintf("[%s item]", it.Type)}}})
		}
	}
	return nil
}

func decodeParts(raw json.RawMessage) []neutral.Block {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []neutral.Block{{Type: neutral.BlockText, Text: s}}
	}
	var parts []contentPart
	if json.Unmarshal(raw, &parts) == nil {
		var out []neutral.Block
		for _, p := range parts {
			out = append(out, neutral.Block{Type: neutral.BlockText, Text: p.Text})
		}
		return out
	}
	return []neutral.Block{{Type: neutral.BlockText, Text: string(raw)}}
}

func stringifyOutput(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []contentPart
	if json.Unmarshal(raw, &parts) == nil && len(parts) > 0 {
		var b string
		for i, p := range parts {
			if i > 0 {
				b += "\n"
			}
			b += p.Text
		}
		return b
	}
	return string(raw)
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func listModels(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("content-type", "application/json")
	// Model comes from config (-c model=...), so this endpoint only quiets clients
	// that probe it. Empty arrays satisfy both OpenAI SDKs ("data") and Codex's
	// models manager ("models") without per-object schema validation (varies by client).
	json.NewEncoder(w).Encode(map[string]interface{}{
		"object": "list",
		"data":   []interface{}{},
		"models": []interface{}{},
	})
}
