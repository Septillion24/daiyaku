package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Transcript is the append-only JSONL evidence record; every entry is fsync'd
// immediately (see write) because it's the operator's only defence if something
// goes wrong.
type Transcript struct {
	mu  sync.Mutex
	f   *os.File
	dir string
}

type Entry struct {
	TS      string      `json:"ts"`
	Dir     string      `json:"dir"` // "harness->mock" | "mock->harness" | "note"
	Seq     int         `json:"seq,omitempty"`
	Kind    string      `json:"kind,omitempty"`
	Headers interface{} `json:"headers,omitempty"`
	Payload interface{} `json:"payload,omitempty"`
}

// NewSessionDir creates a fresh evidence directory under base, stamped with the
// start time. The stamp only has second resolution, so a same-second restart
// would otherwise reopen the previous run in append mode and interleave two
// sessions into one transcript; os.Mkdir fails on an existing directory, which
// makes the suffix search a safe claim rather than a check-then-use race.
func NewSessionDir(base string, start time.Time) (string, error) {
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", err
	}
	stamp := start.Format("20060102-150405")
	for n := 1; n < 100; n++ {
		dir := filepath.Join(base, stamp)
		if n > 1 {
			dir = fmt.Sprintf("%s-%d", dir, n)
		}
		err := os.Mkdir(dir, 0o700)
		if err == nil {
			return dir, nil
		}
		if !os.IsExist(err) {
			return "", err
		}
	}
	return "", fmt.Errorf("no free session directory under %s for %s", base, stamp)
}

// Evidence holds whatever the agent read during the run (file contents, secrets
// it surfaced, the full system prompt), so the directory and the file are owner
// only rather than world readable.
func NewTranscript(sessionDir string) (*Transcript, error) {
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(sessionDir, "transcript.jsonl"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return &Transcript{f: f, dir: sessionDir}, nil
}

func (t *Transcript) Dir() string { return t.dir }

func (t *Transcript) write(e Entry) {
	e.TS = time.Now().UTC().Format(time.RFC3339Nano)
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.f.Write(b)
	t.f.Write([]byte("\n"))
	t.f.Sync()
}

func (t *Transcript) Inbound(seq int, headers, payload interface{}) {
	t.write(Entry{Dir: "harness->mock", Seq: seq, Kind: "request", Headers: headers, Payload: payload})
}

func (t *Transcript) Outbound(seq int, payload interface{}) {
	t.write(Entry{Dir: "mock->harness", Seq: seq, Kind: "response", Payload: payload})
}

func (t *Transcript) Note(kind string, payload interface{}) {
	t.write(Entry{Dir: "note", Kind: kind, Payload: payload})
}

func (t *Transcript) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.f.Close()
}
