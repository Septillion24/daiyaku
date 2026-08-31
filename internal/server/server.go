// Package server hosts the mock inference endpoint. It reads inbound harness
// requests, normalizes them through the active adapter, hands them to the
// engine for the operator to answer, and serializes the operator's action back
// in the provider's wire format.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"daiyaku/internal/adapter"
	"daiyaku/internal/engine"
)

type Server struct {
	adapter adapter.Adapter
	engine  *engine.Engine
	tx      *Transcript
	mux     *http.ServeMux
	http    *http.Server
	addr    string
	proxy   *Proxy
	seq     int64
}

func (s *Server) SetProxy(p *Proxy) { s.proxy = p }

func New(a adapter.Adapter, e *engine.Engine, tx *Transcript, addr string) *Server {
	s := &Server{adapter: a, engine: e, tx: tx, mux: http.NewServeMux(), addr: addr}
	routes := a.Routes()
	s.mux.HandleFunc(routes.Primary, s.handlePrimary)
	for pattern, h := range routes.Aux {
		s.mux.HandleFunc(pattern, s.wrapAux(h))
	}
	s.http = &http.Server{Addr: addr, Handler: s.logRequests(s.mux)}
	return s
}

func (s *Server) Addr() string { return s.addr }

func (s *Server) Handler() http.Handler { return s.http.Handler }

func (s *Server) ListenAndServe(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		s.http.Shutdown(shutCtx)
	}()
	err := s.http.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

// logRequests logs only unrouted (404) requests: they're evidence of harness
// feature probes and unexpected calls.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if rec.status == http.StatusNotFound {
			s.tx.Note("unrouted", map[string]string{
				"method": r.Method, "path": r.URL.Path, "status": "404",
			})
		}
	})
}

// statusRecorder captures the final status code while preserving http.Flusher,
// which SSE streaming responses depend on.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (rec *statusRecorder) WriteHeader(code int) {
	rec.status = code
	rec.ResponseWriter.WriteHeader(code)
}

// Reimplemented so the wrapper still satisfies http.Flusher: without it the type
// assertion fails and SSE streaming breaks.
func (rec *statusRecorder) Flush() {
	if f, ok := rec.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) wrapAux(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.tx.Note("aux-endpoint", map[string]string{"method": r.Method, "path": r.URL.Path})
		h(w, r)
	}
}

func (s *Server) handlePrimary(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	seq := int(atomic.AddInt64(&s.seq, 1))

	if s.proxy != nil {
		s.tx.Inbound(seq, redactHeaders(r.Header), decodeForLog(body))
		s.proxy.Forward(w, r, body, s.tx, seq)
		return
	}

	req, err := s.adapter.Normalize(r.Header, body)
	if err != nil {
		s.tx.Note("normalize-error", map[string]string{"error": err.Error()})
		http.Error(w, fmt.Sprintf("normalize: %v", err), http.StatusBadRequest)
		return
	}
	req.Raw = body
	req.Headers = redactHeaders(r.Header)
	req.Seq = seq // so the inbound entry and the response share the same seq

	s.tx.Inbound(seq, req.Headers, decodeForLog(body))

	action, err := s.engine.Submit(r.Context(), req)
	if err != nil {
		// Client gone or shutting down.
		return
	}
	s.tx.Outbound(req.Seq, action)

	// A malformed action (typo in the composer, hand-written sequence file) must
	// not reach the wire: the blocking encoder would fail after the 200 header is
	// already sent, and the streaming path would ship the fragment verbatim. Fail
	// the request visibly instead, so the harness reports a real error.
	if err := action.Validate(); err != nil {
		s.tx.Note("invalid-action", map[string]string{"error": err.Error(), "seq": fmt.Sprint(req.Seq)})
		writeWireError(w, err.Error())
		return
	}

	if err := s.adapter.WriteResponse(w, req, action); err != nil {
		s.tx.Note("write-error", map[string]string{"error": err.Error()})
	}
}

// sensitive headers are recorded as "<redacted:present>": evidence shows auth
// was sent without capturing the secret.
var sensitive = map[string]bool{
	"authorization": true,
	"x-api-key":     true,
	"cookie":        true,
}

func redactHeaders(h http.Header) map[string]string {
	out := map[string]string{}
	for k, v := range h {
		lk := strings.ToLower(k)
		val := strings.Join(v, ", ")
		if sensitive[lk] {
			val = "<redacted:present>"
		}
		out[lk] = val
	}
	return out
}

func decodeForLog(body []byte) interface{} {
	var v interface{}
	if err := json.Unmarshal(body, &v); err == nil {
		return v
	}
	return string(body)
}

// writeWireError reports a mock-side failure to the harness in a shape both
// client SDKs surface to the user. Anthropic's envelope nests the message under
// "error", which is also where the OpenAI SDKs look, so one body serves both.
func writeWireError(w http.ResponseWriter, msg string) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"type": "error",
		"error": map[string]string{
			"type":    "invalid_request_error",
			"message": "daiyaku: " + msg,
		},
	})
}
