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

type entry struct {
	Dir     string          `json:"dir"`
	Seq     int             `json:"seq"`
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload"`
}

func readEntries(t *testing.T, dir string) []entry {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, "transcript.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		var e entry
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			out = append(out, e)
		}
	}
	return out
}

// A refused action was never sent, so it must not appear as a "response" (which
// `daiyaku report` counts as a tool call and replays). The attempt is kept in a
// note, and the 400 the harness actually received is on the wire record.
func TestRefusedActionIsNotRecordedAsResponse(t *testing.T) {
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
			ToolInput: json.RawMessage(`{"command": "x"`)})
	}()

	body := `{"model":"m","max_tokens":10,"tools":[{"name":"Bash","input_schema":{}}],"messages":[{"role":"user","content":"go"}]}`
	resp, err := http.Post(ts.URL+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	tx.Close()

	var sawNote, sawWire400 bool
	for _, e := range readEntries(t, dir) {
		switch {
		case e.Dir == "mock->harness" && e.Kind == "response":
			t.Errorf("refused action recorded as a response: %s", e.Payload)
		case e.Dir == "note" && e.Kind == "invalid-action":
			sawNote = strings.Contains(string(e.Payload), "Bash")
		case e.Kind == "wire":
			sawWire400 = strings.Contains(string(e.Payload), `"status":400`)
		}
	}
	if !sawNote {
		t.Error("no invalid-action note carrying the rejected action")
	}
	if !sawWire400 {
		t.Error("no wire entry recording the 400 the harness received")
	}
}

// Passthrough has to forward every path. Answering count_tokens or /v1/models
// from the mock's stubs, or 404ing the rest, would capture a harness behaving
// differently from how it does against the real API.
func TestPassthroughForwardsEveryPath(t *testing.T) {
	var seen []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path)
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, `{"from":"upstream"}`)
	}))
	defer upstream.Close()

	dir := t.TempDir()
	tx, err := server.NewTranscript(dir)
	if err != nil {
		t.Fatal(err)
	}
	srv := server.New(&anthropic.Adapter{}, engine.New(0), tx, "127.0.0.1:0")
	srv.SetProxy(server.NewProxy(upstream.URL + "/")) // trailing slash must not double up
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	for _, c := range []struct{ method, path string }{
		{"POST", "/v1/messages"},
		{"POST", "/v1/messages/count_tokens"},
		{"GET", "/v1/models"},
		{"GET", "/v1/something/unmodelled"},
	} {
		req, _ := http.NewRequest(c.method, ts.URL+c.path, strings.NewReader(`{}`))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(b), "upstream") {
			t.Errorf("%s %s: status %d body %q, want the upstream's answer", c.method, c.path, resp.StatusCode, b)
		}
	}
	tx.Close()

	want := []string{"POST /v1/messages", "POST /v1/messages/count_tokens", "GET /v1/models", "GET /v1/something/unmodelled"}
	if strings.Join(seen, "|") != strings.Join(want, "|") {
		t.Errorf("upstream saw %v, want %v", seen, want)
	}
	var inbound int
	for _, e := range readEntries(t, dir) {
		if e.Dir == "harness->mock" && e.Kind == "request" {
			inbound++
		}
	}
	if inbound != len(want) {
		t.Errorf("recorded %d inbound entries, want %d", inbound, len(want))
	}
}

// Losing evidence silently is the worst failure for an evidence tool.
func TestTranscriptWriteFailureIsCounted(t *testing.T) {
	tx, err := server.NewTranscript(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := tx.Failures(); n != 0 {
		t.Fatalf("fresh transcript reports %d failures", n)
	}
	tx.Close() // later writes now fail the way a full or vanished disk would
	tx.Note("after-close", "x")
	tx.Note("after-close", "y")
	n, ferr := tx.Failures()
	if n != 2 || ferr == nil {
		t.Errorf("Failures() = (%d, %v), want (2, first error)", n, ferr)
	}
}

// A shutdown must release handlers blocked on the operator, and Wait must
// return once the server has drained rather than racing process exit.
func TestShutdownReleasesBlockedHandlers(t *testing.T) {
	tx, err := server.NewTranscript(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Close()
	srv := server.New(&anthropic.Adapter{}, engine.New(0), tx, "127.0.0.1:18799")

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- srv.ListenAndServe(ctx) }()

	// A request nobody answers: its handler blocks in engine.Submit.
	got := make(chan struct{})
	go func() {
		defer close(got)
		for i := 0; i < 50; i++ {
			resp, err := http.Post("http://127.0.0.1:18799/v1/messages", "application/json",
				strings.NewReader(`{"model":"m","max_tokens":10,"tools":[{"name":"Bash","input_schema":{}}],"messages":[{"role":"user","content":"go"}]}`))
			if err == nil {
				resp.Body.Close()
				return
			}
			time.Sleep(20 * time.Millisecond) // listener not up yet
		}
	}()
	time.Sleep(300 * time.Millisecond)

	start := time.Now()
	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("ListenAndServe: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ListenAndServe did not return after cancel")
	}
	srv.Wait(time.Second)
	if d := time.Since(start); d > 1500*time.Millisecond {
		t.Errorf("shutdown took %v; blocked handlers were not released", d)
	}
	<-got
}
