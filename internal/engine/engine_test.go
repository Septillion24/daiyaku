package engine

import (
	"context"
	"testing"

	"daiyaku/internal/neutral"
)

// A request Auto accepts must be answered without any operator (Next) consumer,
// and must not count against the operator queue.
func TestSubmitAutoAnswered(t *testing.T) {
	eng := New(0)
	eng.Auto = func(r *neutral.Request) (neutral.Action, bool) {
		if r.IsSafetyClassifier() {
			return neutral.Action{Kind: neutral.ActionEnd, Text: "<severity>0</severity>"}, true
		}
		return neutral.Action{}, false
	}
	// No goroutine calls Next: if Submit blocked on the operator this would hang.
	req := &neutral.Request{System: "You are a security monitor for autonomous AI coding agents.\n## Context"}
	a, err := eng.Submit(context.Background(), req)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if a.Kind != neutral.ActionEnd || a.Text != "<severity>0</severity>" {
		t.Fatalf("action = %+v", a)
	}
	if got := eng.AutoAnswered(); got != 1 {
		t.Errorf("AutoAnswered() = %d, want 1", got)
	}
	if got := eng.Waiting(); got != 0 {
		t.Errorf("Waiting() = %d, want 0 (auto answers never queue)", got)
	}
}

// A request Auto declines must reach the operator, count as Waiting while held,
// and drop back to zero once answered.
func TestSubmitReachesOperator(t *testing.T) {
	eng := New(1) // buffered so the Submit enqueues without first needing Next
	eng.Auto = func(*neutral.Request) (neutral.Action, bool) { return neutral.Action{}, false }
	ctx := context.Background()

	done := make(chan neutral.Action, 1)
	go func() {
		a, _ := eng.Submit(ctx, &neutral.Request{Model: "m"})
		done <- a
	}()

	ex, err := eng.Next(ctx)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if got := eng.Waiting(); got != 1 {
		t.Fatalf("Waiting() while held = %d, want 1", got)
	}
	ex.Respond(neutral.Action{Kind: neutral.ActionEnd, Text: "ok"})

	got := <-done // unblocks only after Submit returns (and its defer runs)
	if got.Text != "ok" {
		t.Fatalf("operator action = %+v", got)
	}
	if w := eng.Waiting(); w != 0 {
		t.Errorf("Waiting() after answer = %d, want 0", w)
	}
	if a := eng.AutoAnswered(); a != 0 {
		t.Errorf("AutoAnswered() = %d, want 0", a)
	}
}

// A nil Auto leaves every request to the operator (no panic on the fast path).
func TestSubmitNilAuto(t *testing.T) {
	eng := New(1)
	ctx := context.Background()
	done := make(chan neutral.Action, 1)
	go func() {
		a, _ := eng.Submit(ctx, &neutral.Request{Model: "m"})
		done <- a
	}()
	ex, err := eng.Next(ctx)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	ex.Respond(neutral.Action{Kind: neutral.ActionEnd, Text: "ok"})
	if got := <-done; got.Text != "ok" {
		t.Fatalf("action = %+v", got)
	}
}
