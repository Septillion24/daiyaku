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

	failures int   // entries that could not be persisted
	firstErr error // the first such failure
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
// only rather than world readable. The mode bits do that on Unix; Windows ignores
// them, so restrictToOwner sets the directory's ACL there.
func NewTranscript(sessionDir string) (*Transcript, error) {
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		return nil, err
	}
	if err := restrictToOwner(sessionDir); err != nil {
		fmt.Fprintf(os.Stderr, "! could not restrict %s to its owner: %v\n"+
			"  the transcript may be readable by other local users; protect the directory yourself.\n",
			sessionDir, err)
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
		// Leave a visible gap marker rather than a silent hole in the record. The
		// replacement carries only strings and ints, so it cannot fail to marshal.
		t.fail(fmt.Errorf("encode %s/%s entry: %w", e.Dir, e.Kind, err))
		b, _ = json.Marshal(Entry{TS: e.TS, Dir: "note", Seq: e.Seq, Kind: "encode-error",
			Payload: map[string]string{"error": err.Error(), "dropped_dir": e.Dir, "dropped_kind": e.Kind}})
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	// One Write per entry so a failure cannot leave a line without its newline.
	if _, err := t.f.Write(append(b, '\n')); err != nil {
		t.failLocked(fmt.Errorf("write transcript: %w", err))
		return
	}
	if err := t.f.Sync(); err != nil {
		t.failLocked(fmt.Errorf("sync transcript: %w", err))
	}
}

// fail records a failure to persist evidence. The first one is announced on
// stderr at once, because the operator's only defence is the transcript and a
// silently incomplete one is worse than a loud one; later ones are only counted
// so a full disk does not flood the console.
func (t *Transcript) fail(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.failLocked(err)
}

func (t *Transcript) failLocked(err error) {
	t.failures++
	if t.firstErr == nil {
		t.firstErr = err
		fmt.Fprintf(os.Stderr, "\n! TRANSCRIPT WRITE FAILED: %v\n  evidence for this run is incomplete (%s)\n",
			err, filepath.Join(t.dir, "transcript.jsonl"))
	}
}

// Failures reports how many entries could not be persisted and the first error,
// so the caller can say so again when the run ends.
func (t *Transcript) Failures() (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.failures, t.firstErr
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

// Wire records the exact bytes the mock sent back, alongside the status and
// response headers. The "response" entry says what the operator authored; this
// one is the proof of what the harness actually received, which is the claim a
// wire-shape finding rests on.
func (t *Transcript) Wire(seq, status int, headers map[string]string, body string) {
	t.write(Entry{Dir: "mock->harness", Seq: seq, Kind: "wire", Headers: headers,
		Payload: map[string]interface{}{
			"status":   status,
			"body_len": len(body),
			"body":     body,
		}})
}
