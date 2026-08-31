// Package anthropic implements the adapter for the Anthropic Messages API,
// which is the wire protocol Claude Code speaks to an LLM gateway. The shapes
// here match the documented Messages API; validate against a Step-0 capture
// before an engagement (see the methodology doc).
package anthropic

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"

	"daiyaku/internal/adapter"
	"daiyaku/internal/neutral"
)

func init() { adapter.Register("anthropic", func() adapter.Adapter { return &Adapter{} }) }

type Adapter struct{}

func (a *Adapter) Provider() string { return "anthropic" }

func (a *Adapter) Routes() adapter.Routes {
	return adapter.Routes{
		Primary: "POST /v1/messages",
		Aux: map[string]http.HandlerFunc{
			"POST /v1/messages/count_tokens": countTokens,
			"GET /v1/models":                 listModels,
		},
	}
}

type wireRequest struct {
	Model         string          `json:"model"`
	MaxTokens     int             `json:"max_tokens"`
	Stream        bool            `json:"stream"`
	StopSequences []string        `json:"stop_sequences"`
	System        json.RawMessage `json:"system"` // string or []textBlock
	Messages      []wireMessage   `json:"messages"`
	Tools         []wireTool      `json:"tools"`
}

type wireMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string or []block
}

type wireTool struct {
	Type        string          `json:"type"` // absent or "custom" for a normal function; set for server tools
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type wireBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"` // tool_result: string or []block
	IsError   bool            `json:"is_error"`
}

func (a *Adapter) Normalize(_ http.Header, body []byte) (*neutral.Request, error) {
	var wr wireRequest
	if err := json.Unmarshal(body, &wr); err != nil {
		return nil, fmt.Errorf("decode request: %w", err)
	}
	req := &neutral.Request{
		Provider:      "anthropic",
		Model:         wr.Model,
		Stream:        wr.Stream,
		MaxTokens:     wr.MaxTokens,
		StopSequences: wr.StopSequences,
		System:        decodeSystem(wr.System),
	}
	for _, t := range wr.Tools {
		req.Tools = append(req.Tools, neutral.ToolDef{
			Name:        t.Name,
			Kind:        toolKind(t),
			Description: t.Description,
			Schema:      t.InputSchema,
		})
	}
	for _, m := range wr.Messages {
		turn := neutral.Turn{Role: m.Role}
		blocks, err := decodeContent(m.Content)
		if err != nil {
			return nil, fmt.Errorf("decode message content: %w", err)
		}
		turn.Blocks = blocks
		req.Turns = append(req.Turns, turn)
	}
	return req, nil
}

func decodeSystem(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []wireBlock
	if json.Unmarshal(raw, &blocks) == nil {
		var b bytes.Buffer
		first := true
		for _, bl := range blocks {
			if bl.Type != "" && bl.Type != "text" {
				continue // skip non-text system blocks (e.g. cache_control markers)
			}
			if !first {
				b.WriteString("\n")
			}
			b.WriteString(bl.Text)
			first = false
		}
		return b.String()
	}
	return string(raw)
}

func decodeContent(raw json.RawMessage) ([]neutral.Block, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []neutral.Block{{Type: neutral.BlockText, Text: s}}, nil
	}
	var wblocks []wireBlock
	if err := json.Unmarshal(raw, &wblocks); err != nil {
		return nil, err
	}
	var out []neutral.Block
	for _, wb := range wblocks {
		switch wb.Type {
		case "text":
			out = append(out, neutral.Block{Type: neutral.BlockText, Text: wb.Text})
		case "tool_use":
			out = append(out, neutral.Block{Type: neutral.BlockToolCall, Call: &neutral.ToolCall{
				ID: wb.ID, Name: wb.Name, Input: wb.Input,
			}})
		case "tool_result":
			out = append(out, neutral.Block{Type: neutral.BlockToolResult, Result: &neutral.ToolResult{
				CallID:  wb.ToolUseID,
				Content: stringifyResult(wb.Content),
				IsError: wb.IsError,
			}})
		default:
			// Unknown block types (thinking, image, ...) surface as text so the operator sees something was present.
			out = append(out, neutral.Block{Type: neutral.BlockText,
				Text: fmt.Sprintf("[%s block]", wb.Type)})
		}
	}
	return out, nil
}

func stringifyResult(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []wireBlock
	if json.Unmarshal(raw, &blocks) == nil {
		var b bytes.Buffer
		for i, bl := range blocks {
			if i > 0 {
				b.WriteString("\n")
			}
			if bl.Type == "text" {
				b.WriteString(bl.Text)
			} else {
				b.WriteString(fmt.Sprintf("[%s]", bl.Type))
			}
		}
		return b.String()
	}
	return string(raw)
}

func countTokens(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]int{"input_tokens": 1000})
}

func listModels(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"data": []map[string]string{
			{"type": "model", "id": "claude-sonnet-4-6", "display_name": "Daiyaku Mock"},
		},
	})
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// toolKind classifies an offered tool. Server-side tools (web_search, the text
// editor, bash_20250124, ...) arrive with a versioned "type" and usually without
// an input_schema; reporting them as ordinary functions understates the offered
// surface, which is the thing an enumeration finding is about. "custom" is the
// explicit spelling of a normal client tool, so it normalizes to "function".
func toolKind(t wireTool) string {
	if t.Type == "" || t.Type == "custom" || t.Type == "function" {
		return "function"
	}
	return t.Type
}
