package server_test

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"daiyaku/internal/adapter/anthropic"
	"daiyaku/internal/engine"
	"daiyaku/internal/server"
)

func TestUnroutedLogged(t *testing.T) {
	dir := t.TempDir()
	tx, err := server.NewTranscript(dir)
	if err != nil {
		t.Fatalf("transcript: %v", err)
	}
	defer tx.Close()

	srv := server.New(&anthropic.Adapter{}, engine.New(0), tx, "127.0.0.1:0")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/some/unexpected/probe")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}

	f, err := os.Open(filepath.Join(dir, "transcript.jsonl"))
	if err != nil {
		t.Fatalf("open transcript: %v", err)
	}
	defer f.Close()

	var found bool
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e struct {
			Kind    string            `json:"kind"`
			Payload map[string]string `json:"payload"`
		}
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		if e.Kind == "unrouted" && strings.Contains(e.Payload["path"], "/v1/some/unexpected/probe") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected an 'unrouted' note for the probe path; none found")
	}
}
