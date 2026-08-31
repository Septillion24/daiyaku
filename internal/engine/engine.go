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
}

// Safe to call at most once; later calls are ignored.
func (ex *Exchange) Respond(a neutral.Action) {
	if atomic.CompareAndSwapInt32(&ex.done, 0, 1) {
		ex.reply <- a
	}
}

type Engine struct {
	pending chan *Exchange
	seq     int64
}

// buffer bounds how many requests may queue before the HTTP side blocks; 0 is a
// synchronous per-turn hand-off.
func New(buffer int) *Engine {
	return &Engine{pending: make(chan *Exchange, buffer)}
}

func (e *Engine) Submit(ctx context.Context, req *neutral.Request) (neutral.Action, error) {
	// The server assigns req.Seq (to log the inbound entry under the same number);
	// fall back to the engine's own counter if it was left unset.
	if req.Seq == 0 {
		req.Seq = int(atomic.AddInt64(&e.seq, 1))
	}
	ex := &Exchange{Req: req, reply: make(chan neutral.Action, 1)}
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
