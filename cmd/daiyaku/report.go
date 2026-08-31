package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"daiyaku/internal/sequence"
)

// reportEntry is one line of a session transcript.
type reportEntry struct {
	Dir     string          `json:"dir"`
	Kind    string          `json:"kind"`
	Seq     int             `json:"seq"`
	TS      string          `json:"ts"`
	Payload json.RawMessage `json:"payload"`
}

// reportAgg accumulates the running totals and recovered steps while scanning a
// transcript.
type reportAgg struct {
	steps                   []sequence.Step
	reqs, executed, proxied int
	wire                    int
	notes                   map[string]int
	noteLines               []string
}

func runReport(args []string) error {
	fs := newFlagSet("report")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dir := fs.Arg(0)
	if dir == "" {
		return fmt.Errorf("usage: daiyaku report <run-dir>")
	}
	path := filepath.Join(dir, "transcript.jsonl")
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open transcript: %w", err)
	}
	defer f.Close()

	var agg reportAgg
	fmt.Printf("\ndaiyaku session report: %s\n", dir)
	fmt.Println("────────────────────────────────────────────────────────")

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	for sc.Scan() {
		var e reportEntry
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		agg.handle(e)
	}

	if err := sc.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "\n! transcript scan stopped early (%v); the report below is incomplete.\n", err)
	}

	fmt.Println("────────────────────────────────────────────────────────")
	fmt.Printf("  requests seen : %d\n", agg.reqs)
	fmt.Printf("  tool calls    : %d\n", agg.executed)
	if agg.proxied > 0 {
		fmt.Printf("  proxied       : %d\n", agg.proxied)
	}
	if agg.wire > 0 {
		fmt.Printf("  wire captures : %d (exact bytes returned to the harness)\n", agg.wire)
	}

	agg.printNotes()

	if err := writeReconstructed(dir, path, agg.steps); err != nil {
		return err
	}
	fmt.Println()
	return nil
}

// handle folds one transcript entry into the running totals.
func (a *reportAgg) handle(e reportEntry) {
	switch {
	case e.Dir == "harness->mock" && e.Kind == "request":
		a.reqs++
	case e.Dir == "mock->harness" && e.Kind == "wire":
		a.wire++ // the raw bytes entry; the "response" entry above is the action
	case e.Dir == "mock->harness":
		a.handleAction(e)
	case e.Dir == "note":
		a.handleNote(e)
	}
}

// handleNote keeps the out-of-band entries: unrouted paths are the harness
// probing for features the mock does not serve (a finding in its own right), and
// the error notes explain any gap between requests seen and actions sent.
func (a *reportAgg) handleNote(e reportEntry) {
	if e.Kind == "" || e.Kind == "session-start" {
		return
	}
	if a.notes == nil {
		a.notes = map[string]int{}
	}
	a.notes[e.Kind]++
	if a.notes[e.Kind] <= 5 {
		a.noteLines = append(a.noteLines, fmt.Sprintf("  note  %-16s %s", e.Kind, compactPayload(e.Payload)))
	}
}

// compactPayload renders a note payload on one bounded line.
func compactPayload(p json.RawMessage) string {
	s := strings.Join(strings.Fields(string(p)), " ")
	if len(s) > 160 {
		s = s[:160] + "..."
	}
	return s
}

// handleAction records an operator action entry (a tool call, a text/end reply,
// or a proxied upstream response), printing a report line for it.
func (a *reportAgg) handleAction(e reportEntry) {
	var act struct {
		Kind      string          `json:"kind"`
		ToolName  string          `json:"tool_name"`
		ToolInput json.RawMessage `json:"tool_input"`
		Text      string          `json:"text"`
		Proxied   bool            `json:"proxied"`
		Status    int             `json:"status"`
	}
	json.Unmarshal(e.Payload, &act)
	if act.Proxied {
		a.proxied++
		fmt.Printf("  #%-3d  [proxied] upstream status %d\n", e.Seq, act.Status)
		return
	}
	switch act.Kind {
	case "tool_call":
		a.executed++
		fmt.Printf("  #%-3d  tool_call  %s %s\n", e.Seq, act.ToolName, string(act.ToolInput))
		a.steps = append(a.steps, sequence.Step{Tool: act.ToolName, Input: act.ToolInput})
	case "text", "end":
		fmt.Printf("  #%-3d  %-9s %q\n", e.Seq, act.Kind, act.Text)
		a.steps = append(a.steps, sequence.Step{Text: act.Text})
	}
}

// writeReconstructed emits a replayable sequence file from the recovered steps,
// or does nothing when no operator actions were found.
func writeReconstructed(dir, path string, steps []sequence.Step) error {
	if len(steps) == 0 {
		return nil
	}
	out := filepath.Join(dir, "reconstructed-sequence.json")
	if err := sequence.Save(out, &sequence.File{
		Name:        "reconstructed",
		Description: "Operator actions recovered from " + path + "; replay with -mode canned.",
		Steps:       steps,
	}); err != nil {
		return err
	}
	fmt.Printf("\n  replayable sequence written: %s\n", out)
	fmt.Printf("  re-run with: daiyaku serve --mode canned --sequence %q\n", out)
	return nil
}

// printNotes reports the transcript's out-of-band entries. They are evidence:
// an unrouted path is the harness probing for an endpoint the mock does not
// serve, and an error note is a turn the operator may believe went through.
func (a *reportAgg) printNotes() {
	if len(a.notes) == 0 {
		return
	}
	kinds := make([]string, 0, len(a.notes))
	for k := range a.notes {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	fmt.Println()
	for _, k := range kinds {
		fmt.Printf("  %-16s : %d\n", k, a.notes[k])
	}
	for _, line := range a.noteLines {
		fmt.Println(line)
	}
}
