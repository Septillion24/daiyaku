package server_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"daiyaku/internal/adapter/anthropic"
	"daiyaku/internal/engine"
	"daiyaku/internal/neutral"
	"daiyaku/internal/server"
)

// A malformed tool input used to be serialized anyway: the blocking encoder
// failed after the 200 header was already sent (empty body, no error the harness
// could report) and the streaming path shipped the fragment as partial_json.
func TestMalformedToolInputIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		stream      bool
	}{
		{"truncated json blocking", `{"command": "x"`, false},
		{"truncated json streaming", `{"command": "x"`, true},
		{"bare string blocking", `"whoami"`, false},
		{"array streaming", `["whoami"]`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tx, err := server.NewTranscript(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Close()

			eng := engine.New(1)
			srv := server.New(&anthropic.Adapter{}, eng, tx, "127.0.0.1:0")
			ts := httptest.NewServer(srv.Handler())
			defer ts.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			go func() {
				ex, err := eng.Next(ctx)
				if err != nil {
					return
				}
				ex.Respond(neutral.Action{Kind: neutral.ActionToolCall, ToolName: "Bash",
					ToolInput: json.RawMessage(tc.input)})
			}()

			body := `{"model":"m","max_tokens":10,"stream":` + boolStr(tc.stream) +
				`,"tools":[{"name":"Bash","input_schema":{}}],"messages":[{"role":"user","content":"go"}]}`
			resp, err := http.Post(ts.URL+"/v1/messages", "application/json", strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			raw, _ := io.ReadAll(resp.Body)

			if resp.StatusCode == http.StatusOK {
				t.Fatalf("malformed input answered with 200 and body %q", raw)
			}
			var env struct {
				Error struct{ Message string } `json:"error"`
			}
			if json.Unmarshal(raw, &env) != nil || env.Error.Message == "" {
				t.Fatalf("error body is not a message the harness can surface: %q", raw)
			}
		})
	}
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
