package console

import (
	"context"
	"fmt"
	"time"

	"daiyaku/internal/engine"
	"daiyaku/internal/neutral"
	"daiyaku/internal/sequence"
)

// Canned replays a fixed sequence of operator actions, then hands off to Fallback (or ends turns if nil).
type Canned struct {
	Engine   *engine.Engine
	Provider string
	Steps    []sequence.Step
	Delay    time.Duration
	Fallback *REPL // optional; if nil, exhausted sequence ends each turn

	idx       int
	prevTools []string
}

func NewCanned(e *engine.Engine, provider string, f *sequence.File, delay time.Duration, fallback *REPL) *Canned {
	return &Canned{Engine: e, Provider: provider, Steps: f.Steps, Delay: delay, Fallback: fallback}
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
			step := c.Steps[c.idx]
			c.idx++
			action := step.Action()
			fmt.Printf("── request #%d ─ %s\n", ex.Req.Seq, Summarize(ex.Req))
			if step.Note != "" {
				fmt.Printf("   note: %s\n", step.Note)
			}
			c.echo(c.idx, action)
			if c.Delay > 0 {
				select {
				case <-time.After(c.Delay):
				case <-ctx.Done():
					return nil
				}
			}
			ex.Respond(action)
			c.prevTools = ex.Req.ToolNames()
			continue
		}
		if c.Fallback != nil {
			fmt.Printf("\n── sequence exhausted at request #%d; handing to interactive operator ──\n",
				ex.Req.Seq)
			c.Fallback.ensureReadline()
			c.Fallback.prevTools = c.prevTools
			action := c.Fallback.interact(ex.Req)
			ex.Respond(action)
			c.prevTools = ex.Req.ToolNames()
			if c.Fallback.quit {
				return nil
			}
			continue
		}
		fmt.Printf("── sequence exhausted at request #%d; ending turn ──\n", ex.Req.Seq)
		ex.Respond(neutral.Action{Kind: neutral.ActionEnd, Text: ""})
		c.prevTools = ex.Req.ToolNames()
	}
}

func (c *Canned) echo(n int, a neutral.Action) {
	switch a.Kind {
	case neutral.ActionToolCall:
		fmt.Printf("   [%d/%d] → tool_call %s %s\n", n, len(c.Steps), a.ToolName, string(a.ToolInput))
	default:
		fmt.Printf("   [%d/%d] → %s %q\n", n, len(c.Steps), a.Kind, a.Text)
	}
}
