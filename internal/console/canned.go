package console

import (
	"context"
	"fmt"
	"strings"
	"time"

	"daiyaku/internal/engine"
	"daiyaku/internal/neutral"
	"daiyaku/internal/sequence"
)

// Canned replays a fixed sequence of operator actions, then hands off to Fallback (or ends turns if nil).
type Canned struct {
	Engine     *engine.Engine
	Provider   string
	Steps      []sequence.Step
	Delay      time.Duration
	RecordPath string
	Fallback   *REPL // optional; if nil, exhausted sequence ends each turn

	idx       int
	prevTools []string
}

func NewCanned(e *engine.Engine, provider string, f *sequence.File, delay time.Duration, recordPath string, fallback *REPL) *Canned {
	return &Canned{Engine: e, Provider: provider, Steps: f.Steps, Delay: delay,
		RecordPath: recordPath, Fallback: fallback}
}

func (c *Canned) Run(ctx context.Context) error {
	fmt.Printf("\n  daiyaku operator console (canned replay)  provider=%s  steps=%d\n",
		c.Provider, len(c.Steps))
	if c.Fallback != nil {
		fmt.Printf("  interactive fallback: on (sequence tail is hand-driven)\n")
	}
	fmt.Printf("  waiting for the harness to connect...\n\n")
	if c.Fallback != nil {
		// The hand-driven tail borrows the fallback REPL's line editing (Tab completion + history).
		defer c.Fallback.closeReadline()
	}
	for {
		ex, err := c.Engine.Next(ctx)
		if err != nil {
			return nil
		}
		if c.idx < len(c.Steps) {
			if c.replayStep(ctx, ex) {
				return nil
			}
			continue
		}
		if c.Fallback != nil {
			if c.fallbackStep(ex) {
				return nil
			}
			continue
		}
		fmt.Printf("── sequence exhausted at request #%d; ending turn ──\n", ex.Req.Seq)
		ex.Respond(neutral.Action{Kind: neutral.ActionEnd, Text: ""})
		c.prevTools = ex.Req.ToolNames()
	}
}

// replayStep replays the next canned step for ex. It returns true if the context
// was cancelled during the inter-step delay (the caller should stop).
func (c *Canned) replayStep(ctx context.Context, ex *engine.Exchange) bool {
	step := c.Steps[c.idx]
	c.idx++
	action := step.Action()
	fmt.Printf("── request #%d ─ %s\n", ex.Req.Seq, Summarize(ex.Req))
	if step.Note != "" {
		fmt.Printf("   note: %s\n", step.Note)
	}
	c.warnUnoffered(ex.Req, action)
	c.echo(c.idx, action)
	if c.delayElapsed(ctx) {
		return true
	}
	if !ex.Respond(action) {
		fmt.Printf("   ! NOT DELIVERED: the harness stopped waiting before this step was sent; nothing ran.\n")
		return false
	}
	c.record(action, step.Note)
	c.prevTools = ex.Req.ToolNames()
	return false
}

// warnUnoffered flags a step that names a tool this turn does not offer. The
// sequence libraries are written per harness, so replaying a Claude Code file
// against Codex (or against a turn where the tool was withdrawn) otherwise fails
// deep inside the harness with an error that looks like a daiyaku bug.
func (c *Canned) warnUnoffered(req *neutral.Request, a neutral.Action) {
	if a.Kind != neutral.ActionToolCall || req.FindTool(a.ToolName) != nil {
		return
	}
	fmt.Printf("   ! %q is not offered this turn; sending anyway. Offered: %s\n",
		a.ToolName, truncate(strings.Join(req.ToolNames(), ", "), 120))
}

// record appends a replayed step to the --record chain, so a recording made
// during a canned run is the whole chain and not just the hand-driven tail.
func (c *Canned) record(a neutral.Action, note string) {
	if c.RecordPath == "" {
		return
	}
	if err := sequence.AppendStep(c.RecordPath, sequence.FromAction(a, note)); err != nil {
		fmt.Printf("   ! failed to record step: %v\n", err)
	}
}

// delayElapsed waits out the configured inter-step delay, returning true if the
// context was cancelled while waiting (false when there is no delay).
func (c *Canned) delayElapsed(ctx context.Context) bool {
	if c.Delay <= 0 {
		return false
	}
	select {
	case <-time.After(c.Delay):
		return false
	case <-ctx.Done():
		return true
	}
}

// fallbackStep hands the exhausted-sequence tail to the interactive operator. It
// returns true when the operator has asked to quit.
func (c *Canned) fallbackStep(ex *engine.Exchange) bool {
	fmt.Printf("\n── sequence exhausted at request #%d; handing to interactive operator ──\n",
		ex.Req.Seq)
	c.Fallback.ensureReadline()
	c.Fallback.prevTools = c.prevTools
	action := c.Fallback.interact(ex.Req)
	c.Fallback.report(action, ex.Respond(action))
	c.prevTools = ex.Req.ToolNames()
	return c.Fallback.quit
}

func (c *Canned) echo(n int, a neutral.Action) {
	switch a.Kind {
	case neutral.ActionToolCall:
		fmt.Printf("   [%d/%d] → tool_call %s %s\n", n, len(c.Steps), a.ToolName, string(a.ToolInput))
	default:
		fmt.Printf("   [%d/%d] → %s %q\n", n, len(c.Steps), a.Kind, a.Text)
	}
}
