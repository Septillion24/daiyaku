// Package engine brokers between the HTTP handler goroutines (which receive
// harness requests) and the operator console (which authors the responses).
// It is deliberately transport- and provider-agnostic.
package engine

import (
	"context"
	"sync/atomic"

	"daiyaku/internal/neutral"
)

type Exchange struct {
	Req   *neutral.Request
	reply chan neutral.Action
	done  int32
	gone  <-chan struct{} // closed when the harness stopped waiting for this reply
}

// Respond hands the operator's action to the waiting HTTP handler and reports
// whether that handler was still there to take it. A harness that disconnected
// or timed out while the operator was composing leaves the exchange orphaned,
// and an action delivered to nobody must never be reported or recorded as
// evidence that the harness executed it. reply is unbuffered, so the send either
// completes into the handler or loses the race to a cancelled request context.
// A second call is always a no-op and reports false.
func (ex *Exchange) Respond(a neutral.Action) (delivered bool) {
	if !atomic.CompareAndSwapInt32(&ex.done, 0, 1) {
		return false
	}
	select {
	case ex.reply <- a:
		return true
	case <-ex.gone:
		return false
	}
}

type Engine struct {
	pending chan *Exchange
	seq     int64

	// Auto, if set, is consulted for every request before it is queued for the
	// operator. When it returns ok, that action is used and the request never
	// reaches the console. It exists so side-channel calls the harness makes on a
	// tight deadline (e.g. the auto-approval safety classifier) are answered
	// instantly instead of stalling on a human. It must be safe to call from many
	// HTTP goroutines at once.
	Auto func(*neutral.Request) (neutral.Action, bool)

	inFlight int64 // requests received but not yet answered, incl. the one the operator holds
	autoDone int64 // requests answered by Auto without reaching the operator
}

// buffer bounds how many requests may queue before the HTTP side blocks; 0 is a
// synchronous per-turn hand-off.
func New(buffer int) *Engine {
	return &Engine{pending: make(chan *Exchange, buffer)}
}

// Waiting is the number of requests received but not yet answered, including the
// one currently in the operator's hands. Consoles subtract one to show how many
// requests are queued behind the one on screen.
func (e *Engine) Waiting() int { return int(atomic.LoadInt64(&e.inFlight)) }

// AutoAnswered is the running total of requests answered by Auto (never shown to
// the operator). Consoles diff it across turns to report how many classifier
// calls were graded automatically.
func (e *Engine) AutoAnswered() int { return int(atomic.LoadInt64(&e.autoDone)) }

func (e *Engine) Submit(ctx context.Context, req *neutral.Request) (neutral.Action, error) {
	// The server assigns req.Seq (to log the inbound entry under the same number);
	// fall back to the engine's own counter if it was left unset.
	if req.Seq == 0 {
		req.Seq = int(atomic.AddInt64(&e.seq, 1))
	}
	// Short-circuit side-channel calls (e.g. the safety classifier) before they
	// queue for a human. The server still records the inbound and this outbound
	// action, so the exchange stays in the transcript.
	if e.Auto != nil {
		if a, ok := e.Auto(req); ok {
			atomic.AddInt64(&e.autoDone, 1)
			return a, nil
		}
	}
	atomic.AddInt64(&e.inFlight, 1)
	defer atomic.AddInt64(&e.inFlight, -1)
	ex := &Exchange{Req: req, reply: make(chan neutral.Action), gone: ctx.Done()}
	select {
	case e.pending <- ex:
	case <-ctx.Done():
		return neutral.Action{}, ctx.Err()
	}
	select {
	case a := <-ex.reply:
		return a, nil
	case <-ctx.Done():
		return neutral.Action{}, ctx.Err()
	}
}

func (e *Engine) Next(ctx context.Context) (*Exchange, error) {
	select {
	case ex := <-e.pending:
		return ex, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
