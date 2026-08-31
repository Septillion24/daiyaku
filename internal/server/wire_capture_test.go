package server_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"daiyaku/internal/adapter/anthropic"
	"daiyaku/internal/engine"
	"daiyaku/internal/neutral"
	"daiyaku/internal/server"
)

// The transcript must hold the exact bytes the harness received, not only a
// summary of the action the operator authored: a wire-shape finding is a claim
// about those bytes.
func TestWireCaptureMatchesWhatTheHarnessReceived(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "blocking"
		if stream {
			name = "streaming"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			tx, err := server.NewTranscript(dir)
			if err != nil {
				t.Fatal(err)
			}
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
					ToolInput: json.RawMessage(`{"command":"whoami"}`)})
			}()

			body := `{"model":"m","max_tokens":10,"stream":` + boolStr(stream) +
				`,"tools":[{"name":"Bash","input_schema":{}}],"messages":[{"role":"user","content":"go"}]}`
			resp, err := http.Post(ts.URL+"/v1/messages", "application/json", strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			received, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			tx.Close()

			recorded, status, ok := readWireEntry(t, filepath.Join(dir, "transcript.jsonl"))
			if !ok {
				t.Fatal("no wire entry in the transcript")
			}
			if recorded != string(received) {
				t.Errorf("recorded bytes differ from what the client got:\nrecorded: %q\nreceived: %q",
					recorded, received)
			}
			if status != http.StatusOK {
				t.Errorf("recorded status = %d", status)
			}
			if !strings.Contains(recorded, "whoami") {
				t.Errorf("wire capture does not contain the tool input: %q", recorded)
			}
		})
	}
}

func readWireEntry(t *testing.T, path string) (body string, status int, ok bool) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		var e struct {
			Dir     string `json:"dir"`
			Kind    string `json:"kind"`
			Payload struct {
				Status  int    `json:"status"`
				Body    string `json:"body"`
				BodyLen int    `json:"body_len"`
			} `json:"payload"`
		}
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.Kind != "wire" {
			continue
		}
		if e.Payload.BodyLen != len(e.Payload.Body) {
			t.Errorf("body_len %d does not match the recorded body (%d bytes)",
				e.Payload.BodyLen, len(e.Payload.Body))
		}
		return e.Payload.Body, e.Payload.Status, true
	}
	return "", 0, false
}
