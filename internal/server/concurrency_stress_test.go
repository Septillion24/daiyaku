package server_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
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

// The engine hands each request from an HTTP goroutine to the single console
// goroutine and the answer back again, over an unbuffered channel that also has
// to lose cleanly to a cancelled request. Run this with -race once a C toolchain
// is available; even without it, at -count and varied -cpu it catches deadlocks,
// crossed answers, and lost or duplicated transcript entries.
func TestConcurrentExchangesStayCorrelated(t *testing.T) {
	const n = 40

	dir := t.TempDir()
	tx, err := server.NewTranscript(dir)
	if err != nil {
		t.Fatal(err)
	}
	eng := engine.New(0) // unbuffered: the real hand-off, no slack
	srv := server.New(&anthropic.Adapter{}, eng, tx, "127.0.0.1:0")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// One console goroutine, as in every real mode: it answers each request with
	// a command naming that request, so a crossed answer is detectable.
	go func() {
		for {
			ex, err := eng.Next(ctx)
			if err != nil {
				return
			}
			marker := ex.Req.Turns[0].Blocks[0].Text
			ex.Respond(neutral.Action{Kind: neutral.ActionToolCall, ToolName: "Bash",
				ToolInput: json.RawMessage(fmt.Sprintf(`{"command":%q}`, marker))})
		}
	}()

	var wg sync.WaitGroup
	errs := make(chan string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			marker := fmt.Sprintf("marker-%d", i)
			body := fmt.Sprintf(`{"model":"m","max_tokens":10,"stream":%v,
				"tools":[{"name":"Bash","input_schema":{}}],
				"messages":[{"role":"user","content":%q}]}`, i%2 == 0, marker)
			resp, err := http.Post(ts.URL+"/v1/messages", "application/json", strings.NewReader(body))
			if err != nil {
				errs <- fmt.Sprintf("%s: %v", marker, err)
				return
			}
			defer resp.Body.Close()
			raw, _ := io.ReadAll(resp.Body)
			if !strings.Contains(string(raw), marker) {
				errs <- fmt.Sprintf("%s: answer went to the wrong request: %s", marker, truncateBody(raw))
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	tx.Close()

	responses, wires := countEntries(t, filepath.Join(dir, "transcript.jsonl"))
	for seq := 1; seq <= n; seq++ {
		if responses[seq] != 1 {
			t.Errorf("seq %d has %d response entries, want 1", seq, responses[seq])
		}
		if wires[seq] != 1 {
			t.Errorf("seq %d has %d wire entries, want 1", seq, wires[seq])
		}
	}
}

// Respond racing a cancelled request must never block or panic: the reply
// channel is unbuffered, so both sides have to lose that race cleanly, and the
// caller has to learn the action was not delivered.
func TestRespondRacingCancellation(t *testing.T) {
	for i := 0; i < 200; i++ {
		eng := engine.New(1)
		reqCtx, reqCancel := context.WithCancel(context.Background())
		go func() { eng.Submit(reqCtx, &neutral.Request{Model: "m", Seq: 1}) }()

		ex, err := eng.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}

		delivered := make(chan bool, 1)
		go func() { delivered <- ex.Respond(neutral.Action{Kind: neutral.ActionEnd, Text: "x"}) }()
		reqCancel() // races the Respond above

		select {
		case ok := <-delivered:
			if ok {
				// Delivered: the handler took it before the cancel landed. Fine.
				continue
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("iteration %d: Respond blocked forever against a cancelled request", i)
		}
	}
}

func truncateBody(b []byte) string {
	if len(b) > 200 {
		return string(b[:200]) + "..."
	}
	return string(b)
}

func countEntries(t *testing.T, path string) (responses, wires map[int]int) {
	t.Helper()
	responses, wires = map[int]int{}, map[int]int{}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		var e struct {
			Dir  string `json:"dir"`
			Kind string `json:"kind"`
			Seq  int    `json:"seq"`
		}
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.Dir != "mock->harness" {
			continue
		}
		switch e.Kind {
		case "response":
			responses[e.Seq]++
		case "wire":
			wires[e.Seq]++
		}
	}
	return responses, wires
}
