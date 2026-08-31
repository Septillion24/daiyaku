package console

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"daiyaku/internal/neutral"
)

type Console interface {
	Run(ctx context.Context) error
}

func Summarize(req *neutral.Request) string {
	last := "none"
	if r := req.LastResult(); r != nil {
		last = truncate(oneLine(r.Content), 60)
		if r.IsError {
			last = "ERROR: " + last
		}
	}
	return fmt.Sprintf("#%d  model=%s  turns=%d  tools=%d  last-result=%q",
		req.Seq, req.Model, len(req.Turns), len(req.Tools), last)
}

func RenderTools(req *neutral.Request, prev []string) string {
	prevSet := map[string]bool{}
	for _, n := range prev {
		prevSet[n] = true
	}
	curSet := map[string]bool{}
	names := make([]string, 0, len(req.Tools))
	for _, t := range req.Tools {
		curSet[t.Label()] = true
		name := t.Label()
		if t.Kind != "" && t.Kind != "function" {
			name += " [" + t.Kind + "]"
		}
		names = append(names, name)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Offered tools (%d):\n", len(req.Tools))
	b.WriteString(wrapList(names, "  ", 72))

	if prev != nil {
		b.WriteString(toolDiff(req, prev, prevSet, curSet))
	}
	return b.String()
}

// toolDiff renders the added/removed lines between the previous turn's tool set
// and this one. prevSet/curSet are the label sets of prev and req respectively.
func toolDiff(req *neutral.Request, prev []string, prevSet, curSet map[string]bool) string {
	var added, removed []string
	for _, t := range req.Tools {
		if !prevSet[t.Label()] {
			added = append(added, t.Label())
		}
	}
	for _, n := range prev {
		if !curSet[n] {
			removed = append(removed, n)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	var b strings.Builder
	if len(added) > 0 {
		fmt.Fprintf(&b, "  + new this turn: %s\n", strings.Join(added, ", "))
	}
	if len(removed) > 0 {
		fmt.Fprintf(&b, "  - no longer offered: %s\n", strings.Join(removed, ", "))
	}
	return b.String()
}

func RenderToolHelp(req *neutral.Request, name string) string {
	t := req.FindTool(name)
	if t == nil {
		return fmt.Sprintf("no such tool offered: %s (see 'tools')", name)
	}
	head := t.Label()
	if t.Kind != "" && t.Kind != "function" {
		head += " [" + t.Kind + "]"
	}
	desc := strings.TrimSpace(t.Description)
	if desc == "" {
		desc = "(no description provided)"
	}
	return head + "\n" + desc + "\ninput fields:  schema " + t.Name
}

func wrapList(items []string, pad string, w int) string {
	if len(items) == 0 {
		return pad + "(none)\n"
	}
	var b strings.Builder
	line := pad
	for i, it := range items {
		piece := it
		if i < len(items)-1 {
			piece += ","
		}
		switch {
		case line == pad:
			line += piece
		case len([]rune(line))+1+len([]rune(piece)) > w:
			b.WriteString(line + "\n")
			line = pad + piece
		default:
			line += " " + piece
		}
	}
	b.WriteString(line + "\n")
	return b.String()
}

func RenderSchema(req *neutral.Request, name string) string {
	t := req.FindTool(name)
	if t == nil {
		return fmt.Sprintf("no such tool offered: %s", name)
	}
	return fmt.Sprintf("%s\n%s\n%s", t.Name, t.Description, prettyJSON(t.Schema))
}

func RenderContext(req *neutral.Request, showSystem bool) string {
	var b strings.Builder
	if showSystem && req.System != "" {
		b.WriteString("=== system ===\n")
		b.WriteString(req.System)
		b.WriteString("\n\n")
	}
	b.WriteString("=== conversation ===\n")
	for _, turn := range req.Turns {
		fmt.Fprintf(&b, "[%s]\n", turn.Role)
		for _, bl := range turn.Blocks {
			renderBlock(&b, bl)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// renderBlock writes a single conversation block (text, tool call, or tool result).
func renderBlock(b *strings.Builder, bl neutral.Block) {
	switch bl.Type {
	case neutral.BlockText:
		b.WriteString(indent(bl.Text, "  "))
	case neutral.BlockToolCall:
		if bl.Call != nil {
			fmt.Fprintf(b, "  ->call %s %s\n", bl.Call.Name, oneLine(string(bl.Call.Input)))
		}
	case neutral.BlockToolResult:
		if bl.Result != nil {
			tag := "result"
			if bl.Result.IsError {
				tag = "result(ERROR)"
			}
			fmt.Fprintf(b, "  <-%s %s", tag, indent(truncate(bl.Result.Content, 2000), "    "))
		}
	}
}

func Template(req *neutral.Request, name string) string {
	t := req.FindTool(name)
	if t == nil {
		return "{}"
	}
	b, err := json.MarshalIndent(templateValue(t.Schema), "", "  ")
	if err != nil {
		return "{}"
	}
	return string(b)
}

// templateValue is skeleton with an empty object as the floor. A tool with no
// schema at all (Anthropic server tools), a union type ("type":["object","null"])
// or a $ref-only schema all make skeleton give up, and rendering that as the
// literal "null" hands the operator a template they cannot edit into a call.
func templateValue(schema json.RawMessage) interface{} {
	if v := skeleton(schema); v != nil {
		return v
	}
	return map[string]interface{}{}
}

func TemplateCompact(req *neutral.Request, name string) string {
	t := req.FindTool(name)
	if t == nil {
		return "{}"
	}
	b, err := json.Marshal(templateValue(t.Schema))
	if err != nil {
		return "{}"
	}
	return string(b)
}

func skeleton(raw json.RawMessage) interface{} {
	var s struct {
		Type       string                     `json:"type"`
		Properties map[string]json.RawMessage `json:"properties"`
		Items      json.RawMessage            `json:"items"`
		Enum       []interface{}              `json:"enum"`
		Default    interface{}                `json:"default"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil {
		return nil
	}
	if s.Default != nil {
		return s.Default
	}
	if len(s.Enum) > 0 {
		return s.Enum[0]
	}
	switch s.Type {
	case "object":
		return skeletonObject(s.Properties)
	case "array":
		return []interface{}{skeleton(s.Items)}
	case "string":
		return ""
	case "number", "integer":
		return 0
	case "boolean":
		return false
	default:
		return nil
	}
}

// skeletonObject builds a placeholder object with one skeleton value per property,
// keys walked in sorted order so the emitted template is deterministic.
func skeletonObject(props map[string]json.RawMessage) map[string]interface{} {
	m := map[string]interface{}{}
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		m[k] = skeleton(props[k])
	}
	return m
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}

func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s) // rune-aware: never cut mid-rune
	if len(r) <= n {
		return s
	}
	if n == 1 {
		return string(r[:1])
	}
	return string(r[:n-1]) + "…"
}

func indent(s, pad string) string {
	if s == "" {
		return "\n"
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = pad + l
	}
	return strings.Join(lines, "\n") + "\n"
}

func prettyJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "(none)"
	}
	var v interface{}
	if json.Unmarshal(raw, &v) != nil {
		return string(raw)
	}
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}

// SideCallWarning describes a request that reached the operator offering no
// tools at all. A real agent turn always offers tools, so this is either a
// harness side-channel call daiyaku failed to recognize (the classifier's shape
// drifts every release) or an unusual turn worth a second look. Saying so is the
// difference between a visible warning and a harness that mysteriously reports
// the model as unavailable while a human types.
func SideCallWarning(req *neutral.Request) string {
	if len(req.Tools) > 0 {
		return ""
	}
	if req.MayBeSideCall() {
		return "this carries some marks of a harness side-channel call but not enough to answer it " +
			"automatically: its shape has probably drifted. Such calls expect a terse reply on a short " +
			"deadline, so answering by hand may make the harness report the model as unavailable. " +
			"Check 'raw', then see -classifier-severity."
	}
	return "this request offers no tools. If it is a harness side-channel call rather than a turn, " +
		"daiyaku did not recognize it and the harness may be waiting on a deadline. Check 'raw'."
}
