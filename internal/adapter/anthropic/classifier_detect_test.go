package anthropic

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Recognition must hold against real captured traffic, and must never fire on a
// real agent turn: a false positive answers the operator's turn with a severity
// number, a false negative stalls the harness.
func TestClassifierDetectionAgainstCapturedTraffic(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{"classifier as captured", `{"model":"m","max_tokens":64,
			"stop_sequences":["</severity>"],
			"system":[{"text":"You are a security monitor for autonomous AI coding agents.\n Reply <severity>N</severity> ONLY."}],
			"messages":[{"role":"user","content":"<transcript>...</transcript>"}]}`, true},
		{"prose rewritten, protocol intact", `{"model":"m","max_tokens":64,
			"stop_sequences":["</severity>"],
			"system":"Completely reworded guardrail prompt with no familiar sentence.",
			"messages":[{"role":"user","content":"x"}]}`, true},
		{"stop sequence dropped, tag and budget intact", `{"model":"m","max_tokens":64,
			"system":"Reworded. Answer <severity>N</severity> and nothing else.",
			"messages":[{"role":"user","content":"x"}]}`, true},
		{"real agent turn", `{"model":"m","max_tokens":32000,
			"tools":[{"name":"Bash","input_schema":{"type":"object"}}],
			"system":"You are Claude Code.",
			"messages":[{"role":"user","content":"list the files"}]}`, false},
		{"tool-less turn that is not a side-call", `{"model":"m","max_tokens":32000,
			"system":"Summarize the conversation so far.",
			"messages":[{"role":"user","content":"x"}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := (&Adapter{}).Normalize(nil, []byte(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			if got := req.IsSafetyClassifier(); got != tc.want {
				t.Errorf("IsSafetyClassifier() = %v, want %v", got, tc.want)
			}
		})
	}
}

// The real thing, replayed from a transcript captured against Claude Code
// 2.1.251, so a wire change shows up here rather than as a wedged harness.
func TestClassifierDetectionOnRecordedRun(t *testing.T) {
	matches, _ := filepath.Glob(filepath.Join("..", "..", "..", "runs", "*", "transcript.jsonl"))
	checked := 0
	for _, path := range matches {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if !strings.Contains(line, `"kind":"request"`) {
				continue
			}
			payload, ok := payloadOf(line)
			if !ok {
				continue
			}
			req, err := (&Adapter{}).Normalize(nil, payload)
			if err != nil {
				continue
			}
			isClassifier := strings.Contains(req.System, "security monitor for autonomous AI coding agents")
			if got := req.IsSafetyClassifier(); got != isClassifier {
				t.Errorf("%s: IsSafetyClassifier() = %v for a request whose prompt says classifier=%v",
					filepath.Base(filepath.Dir(path)), got, isClassifier)
			}
			checked++
		}
	}
	if checked == 0 {
		t.Skip("no captured runs available to replay")
	}
	t.Logf("replayed %d captured requests", checked)
}

// payloadOf pulls the recorded request body back out of a transcript line.
func payloadOf(line string) ([]byte, bool) {
	var e struct {
		Payload json.RawMessage `json:"payload"`
	}
	if json.Unmarshal([]byte(line), &e) != nil || len(e.Payload) == 0 {
		return nil, false
	}
	return e.Payload, true
}
