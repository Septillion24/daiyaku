package server_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"daiyaku/internal/adapter/anthropic"
	"daiyaku/internal/engine"
	"daiyaku/internal/neutral"
	"daiyaku/internal/server"
)

func TestFullLoop(t *testing.T) {
	dir := t.TempDir()
	tx, err := server.NewTranscript(dir)
	if err != nil {
		t.Fatalf("transcript: %v", err)
	}
	defer tx.Close()

	eng := engine.New(0)
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
		if got := ex.Req.ToolNames(); len(got) == 0 || got[0] != "Bash" {
			t.Errorf("operator saw tools %v", got)
		}
		ex.Respond(neutral.Action{
			Kind: neutral.ActionToolCall, ToolName: "Bash",
			ToolInput: json.RawMessage(`{"command":"whoami"}`),
		})
	}()

	ctResp, err := http.Post(ts.URL+"/v1/messages/count_tokens", "application/json",
		strings.NewReader(`{"messages":[]}`))
	if err != nil {
		t.Fatalf("count_tokens: %v", err)
	}
	var ct map[string]int
	json.NewDecoder(ctResp.Body).Decode(&ct)
	ctResp.Body.Close()
	if ct["input_tokens"] <= 0 {
		t.Errorf("count_tokens = %v", ct)
	}

	reqBody := `{"model":"claude-sonnet-4-6","max_tokens":100,"stream":true,
		"tools":[{"name":"Bash","description":"run","input_schema":{"type":"object"}}],
		"messages":[{"role":"user","content":"who am i"}]}`
	resp, err := http.Post(ts.URL+"/v1/messages", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("messages: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("content-type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}

	var sawToolUse, sawWhoami, sawStop bool
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if strings.Contains(line, `"type":"tool_use"`) {
			sawToolUse = true
		}
		if strings.Contains(line, "whoami") {
			sawWhoami = true
		}
		if strings.Contains(line, `"stop_reason":"tool_use"`) {
			sawStop = true
		}
	}
	if !sawToolUse || !sawWhoami || !sawStop {
		t.Errorf("stream incomplete: toolUse=%v whoami=%v stop=%v", sawToolUse, sawWhoami, sawStop)
	}
}

func TestConcurrentSeqCorrelation(t *testing.T) {
	dir := t.TempDir()
	tx, err := server.NewTranscript(dir)
	if err != nil {
		t.Fatalf("transcript: %v", err)
	}
	eng := engine.New(0)
	srv := server.New(&anthropic.Adapter{}, eng, tx, "127.0.0.1:0")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	go func() {
		for {
			ex, err := eng.Next(ctx)
			if err != nil {
				return
			}
			ex.Respond(neutral.Action{Kind: neutral.ActionToolCall, ToolName: "Bash",
				ToolInput: json.RawMessage(`{"command":"id"}`)})
		}
	}()

	const n = 6
	body := `{"model":"m","max_tokens":10,"stream":false,"tools":[{"name":"Bash","input_schema":{}}],"messages":[{"role":"user","content":"go"}]}`
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Post(ts.URL+"/v1/messages", "application/json", strings.NewReader(body))
			if err == nil {
				resp.Body.Close()
			}
		}()
	}
	wg.Wait()
	tx.Close()

	f, err := os.Open(filepath.Join(dir, "transcript.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	inbound := map[int]int{}
	outbound := map[int]int{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		var e struct {
			Dir  string `json:"dir"`
			Kind string `json:"kind"`
			Seq  int    `json:"seq"`
		}
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		switch {
		case e.Dir == "harness->mock" && e.Kind == "request":
			inbound[e.Seq]++
		case e.Dir == "mock->harness" && e.Kind == "response":
			outbound[e.Seq]++
		}
	}
	if len(inbound) != n {
		t.Fatalf("expected %d distinct inbound seqs, got %d: %v", n, len(inbound), inbound)
	}
	for seq, count := range inbound {
		if seq == 0 {
			t.Errorf("inbound entry logged with seq 0 (correlation broken)")
		}
		if count != 1 {
			t.Errorf("seq %d appears %d times inbound (duplicate/lost update)", seq, count)
		}
		if outbound[seq] != 1 {
			t.Errorf("inbound seq %d has %d matching outbound entries, want 1", seq, outbound[seq])
		}
	}
}
