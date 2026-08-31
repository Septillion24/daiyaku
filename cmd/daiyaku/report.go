package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"daiyaku/internal/sequence"
)

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

	type entry struct {
		Dir     string          `json:"dir"`
		Kind    string          `json:"kind"`
		Seq     int             `json:"seq"`
		TS      string          `json:"ts"`
		Payload json.RawMessage `json:"payload"`
	}

	var steps []sequence.Step
	var reqs, executed, proxied int
	fmt.Printf("\ndaiyaku session report: %s\n", dir)
	fmt.Println("────────────────────────────────────────────────────────")

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	for sc.Scan() {
		var e entry
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		switch {
		case e.Dir == "harness->mock" && e.Kind == "request":
			reqs++
		case e.Dir == "mock->harness":
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
				proxied++
				fmt.Printf("  #%-3d  [proxied] upstream status %d\n", e.Seq, act.Status)
				continue
			}
			switch act.Kind {
			case "tool_call":
				executed++
				fmt.Printf("  #%-3d  tool_call  %s %s\n", e.Seq, act.ToolName, string(act.ToolInput))
				steps = append(steps, sequence.Step{Tool: act.ToolName, Input: act.ToolInput})
			case "text", "end":
				fmt.Printf("  #%-3d  %-9s %q\n", e.Seq, act.Kind, act.Text)
				steps = append(steps, sequence.Step{Text: act.Text, End: act.Kind == "end"})
			}
		}
	}

	if err := sc.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "\n! transcript scan stopped early (%v); the report below is incomplete.\n", err)
	}

	fmt.Println("────────────────────────────────────────────────────────")
	fmt.Printf("  requests seen : %d\n", reqs)
	fmt.Printf("  tool calls    : %d\n", executed)
	if proxied > 0 {
		fmt.Printf("  proxied       : %d\n", proxied)
	}

	if len(steps) > 0 {
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
	}
	fmt.Println()
	return nil
}
