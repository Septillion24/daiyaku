package anthropic

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"

	"daiyaku/internal/neutral"
)

func (a *Adapter) WriteResponse(w http.ResponseWriter, req *neutral.Request, action neutral.Action) error {
	if req.Stream {
		return writeSSE(w, req.Model, action)
	}
	return writeBlocking(w, req.Model, action)
}

func stopReason(action neutral.Action) string {
	if action.Kind == neutral.ActionToolCall {
		return "tool_use"
	}
	return "end_turn"
}

func contentBlock(action neutral.Action, toolID string) map[string]interface{} {
	if action.Kind == neutral.ActionToolCall {
		input := json.RawMessage(action.ToolInput)
		if len(input) == 0 {
			input = json.RawMessage("{}")
		}
		return map[string]interface{}{
			"type": "tool_use", "id": toolID, "name": action.ToolName, "input": input,
		}
	}
	return map[string]interface{}{"type": "text", "text": action.Text}
}

func writeBlocking(w http.ResponseWriter, model string, action neutral.Action) error {
	msg := map[string]interface{}{
		"id":            "msg_" + randID(),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       []interface{}{contentBlock(action, "toolu_"+randID())},
		"stop_reason":   stopReason(action),
		"stop_sequence": nil,
		"usage":         map[string]int{"input_tokens": 1000, "output_tokens": 50},
	}
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(http.StatusOK)
	return json.NewEncoder(w).Encode(msg)
}

// Confirm the event ordering against a Step-0 capture before relying on it in an engagement.
func writeSSE(w http.ResponseWriter, model string, action neutral.Action) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return fmt.Errorf("streaming unsupported by ResponseWriter")
	}
	w.Header().Set("content-type", "text/event-stream")
	w.Header().Set("cache-control", "no-cache")
	w.Header().Set("connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	msgID := "msg_" + randID()
	toolID := "toolu_" + randID()

	ev := func(name string, data map[string]interface{}) error {
		b, _ := json.Marshal(data)
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	send := []func() error{
		func() error {
			return ev("message_start", map[string]interface{}{
				"type": "message_start",
				"message": map[string]interface{}{
					"id": msgID, "type": "message", "role": "assistant", "model": model,
					"content": []interface{}{}, "stop_reason": nil, "stop_sequence": nil,
					"usage": map[string]int{"input_tokens": 1000, "output_tokens": 1},
				},
			})
		},
		func() error { return ev("ping", map[string]interface{}{"type": "ping"}) },
	}

	if action.Kind == neutral.ActionToolCall {
		input := string(action.ToolInput)
		if input == "" {
			input = "{}"
		}
		send = append(send,
			func() error {
				return ev("content_block_start", map[string]interface{}{
					"type": "content_block_start", "index": 0,
					"content_block": map[string]interface{}{
						"type": "tool_use", "id": toolID, "name": action.ToolName,
						"input": map[string]interface{}{},
					},
				})
			},
			func() error {
				return ev("content_block_delta", map[string]interface{}{
					"type": "content_block_delta", "index": 0,
					"delta": map[string]interface{}{"type": "input_json_delta", "partial_json": input},
				})
			},
		)
	} else {
		text := action.Text
		send = append(send,
			func() error {
				return ev("content_block_start", map[string]interface{}{
					"type": "content_block_start", "index": 0,
					"content_block": map[string]interface{}{"type": "text", "text": ""},
				})
			},
			func() error {
				return ev("content_block_delta", map[string]interface{}{
					"type": "content_block_delta", "index": 0,
					"delta": map[string]interface{}{"type": "text_delta", "text": text},
				})
			},
		)
	}

	send = append(send,
		func() error {
			return ev("content_block_stop", map[string]interface{}{
				"type": "content_block_stop", "index": 0,
			})
		},
		func() error {
			return ev("message_delta", map[string]interface{}{
				"type":  "message_delta",
				"delta": map[string]interface{}{"stop_reason": stopReason(action), "stop_sequence": nil},
				"usage": map[string]int{"output_tokens": 50},
			})
		},
		func() error { return ev("message_stop", map[string]interface{}{"type": "message_stop"}) },
	)

	for _, f := range send {
		if err := f(); err != nil {
			return err
		}
	}
	return nil
}

func randID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}
