package openai

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

// The Responses API expects function-call arguments as a JSON-encoded string.
func argString(action neutral.Action) string {
	if len(action.ToolInput) == 0 {
		return "{}"
	}
	return string(action.ToolInput)
}

func outputItem(action neutral.Action, fcID, callID, msgID string) map[string]interface{} {
	if action.Kind == neutral.ActionToolCall {
		return map[string]interface{}{
			"type": "function_call", "id": fcID, "call_id": callID,
			"name": action.ToolName, "arguments": argString(action), "status": "completed",
		}
	}
	return map[string]interface{}{
		"type": "message", "id": msgID, "role": "assistant", "status": "completed",
		"content": []interface{}{
			map[string]interface{}{"type": "output_text", "text": action.Text, "annotations": []interface{}{}},
		},
	}
}

func responseEnvelope(id, model, status string, output []interface{}) map[string]interface{} {
	return map[string]interface{}{
		"id": id, "object": "response", "created_at": 0, "status": status,
		"model": model, "output": output, "parallel_tool_calls": false,
		"usage": map[string]int{"input_tokens": 1000, "output_tokens": 50, "total_tokens": 1050},
	}
}

func writeBlocking(w http.ResponseWriter, model string, action neutral.Action) error {
	respID := "resp_" + randID()
	item := outputItem(action, "fc_"+randID(), "call_"+randID(), "msg_"+randID())
	env := responseEnvelope(respID, model, "completed", []interface{}{item})
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(http.StatusOK)
	return json.NewEncoder(w).Encode(env)
}

// Confirm the event ordering against a Step-0 capture / the Codex source before relying on it.
func writeSSE(w http.ResponseWriter, model string, action neutral.Action) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return fmt.Errorf("streaming unsupported by ResponseWriter")
	}
	w.Header().Set("content-type", "text/event-stream")
	w.Header().Set("cache-control", "no-cache")
	w.WriteHeader(http.StatusOK)

	respID := "resp_" + randID()
	fcID := "fc_" + randID()
	callID := "call_" + randID()
	msgID := "msg_" + randID()

	seq := 0
	ev := func(name string, data map[string]interface{}) error {
		data["type"] = name
		data["sequence_number"] = seq
		seq++
		b, _ := json.Marshal(data)
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	inProgress := responseEnvelope(respID, model, "in_progress", []interface{}{})
	inProgress["usage"] = nil // real API reports usage only on response.completed
	completedItem := outputItem(action, fcID, callID, msgID)
	completed := responseEnvelope(respID, model, "completed", []interface{}{completedItem})

	var steps []func() error
	steps = append(steps,
		func() error { return ev("response.created", map[string]interface{}{"response": inProgress}) },
		func() error { return ev("response.in_progress", map[string]interface{}{"response": inProgress}) },
	)

	if action.Kind == neutral.ActionToolCall {
		args := argString(action)
		steps = append(steps,
			func() error {
				return ev("response.output_item.added", map[string]interface{}{
					"output_index": 0,
					"item": map[string]interface{}{
						"type": "function_call", "id": fcID, "call_id": callID,
						"name": action.ToolName, "arguments": "", "status": "in_progress",
					},
				})
			},
			func() error {
				return ev("response.function_call_arguments.delta", map[string]interface{}{
					"item_id": fcID, "output_index": 0, "delta": args,
				})
			},
			func() error {
				return ev("response.function_call_arguments.done", map[string]interface{}{
					"item_id": fcID, "output_index": 0, "arguments": args,
				})
			},
			func() error {
				return ev("response.output_item.done", map[string]interface{}{
					"output_index": 0, "item": completedItem,
				})
			},
		)
	} else {
		text := action.Text
		steps = append(steps,
			func() error {
				return ev("response.output_item.added", map[string]interface{}{
					"output_index": 0,
					"item": map[string]interface{}{
						"type": "message", "id": msgID, "role": "assistant",
						"status": "in_progress", "content": []interface{}{},
					},
				})
			},
			func() error {
				return ev("response.content_part.added", map[string]interface{}{
					"item_id": msgID, "output_index": 0, "content_index": 0,
					"part": map[string]interface{}{"type": "output_text", "text": "", "annotations": []interface{}{}},
				})
			},
			func() error {
				return ev("response.output_text.delta", map[string]interface{}{
					"item_id": msgID, "output_index": 0, "content_index": 0, "delta": text,
				})
			},
			func() error {
				return ev("response.output_text.done", map[string]interface{}{
					"item_id": msgID, "output_index": 0, "content_index": 0, "text": text,
				})
			},
			func() error {
				return ev("response.content_part.done", map[string]interface{}{
					"item_id": msgID, "output_index": 0, "content_index": 0,
					"part": map[string]interface{}{"type": "output_text", "text": text, "annotations": []interface{}{}},
				})
			},
			func() error {
				return ev("response.output_item.done", map[string]interface{}{
					"output_index": 0, "item": completedItem,
				})
			},
		)
	}

	steps = append(steps, func() error {
		return ev("response.completed", map[string]interface{}{"response": completed})
	})

	for _, f := range steps {
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
