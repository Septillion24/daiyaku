// Package server hosts the mock inference endpoint. It reads inbound harness
// requests, normalizes them through the active adapter, hands them to the
// engine for the operator to answer, and serializes the operator's action back
// in the provider's wire format.
package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
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
	tls     *tls.Config
	addr    string
	proxy   *Proxy
	seq     int64

	stopped chan struct{} // closed once ListenAndServe has fully shut down
}

// SetProxy switches the server to passthrough: every request, whatever its path,
// is forwarded upstream and nothing is answered by the mock. Call before serving.
func (s *Server) SetProxy(p *Proxy) { s.proxy = p }

// SetTLS makes ListenAndServe terminate TLS with cfg, which is how intercept
// mode answers at the vendor's own https address.
func (s *Server) SetTLS(cfg *tls.Config) { s.tls = cfg }

func New(a adapter.Adapter, e *engine.Engine, tx *Transcript, addr string) *Server {
	s := &Server{adapter: a, engine: e, tx: tx, mux: http.NewServeMux(), addr: addr,
		stopped: make(chan struct{})}
	routes := a.Routes()
	s.mux.HandleFunc(routes.Primary, s.handlePrimary)
	for pattern, h := range routes.Aux {
		s.mux.HandleFunc(pattern, s.wrapAux(h))
	}
	s.http = &http.Server{
		Addr:    addr,
		Handler: s.logRequests(http.HandlerFunc(s.dispatch)),
		// Bounds how long a client may dawdle over its headers. No Read/Write
		// timeout: a handler legitimately waits on the operator, and SSE responses
		// are long-lived.
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s
}

// dispatch sends a request to the mock's routes, or in passthrough straight to
// the upstream. The proxy has to see every path: a Step-0 capture that answered
// count_tokens or /v1/models from the mock's stubs, or 404'd anything else, would
// record a harness behaving differently from how it does against the real API.
func (s *Server) dispatch(w http.ResponseWriter, r *http.Request) {
	if s.proxy != nil {
		s.handleProxied(w, r)
		return
	}
	s.mux.ServeHTTP(w, r)
}

func (s *Server) Addr() string { return s.addr }

func (s *Server) Handler() http.Handler { return s.http.Handler }

// Wait blocks until ListenAndServe has finished shutting down (in-flight handlers
// drained, or the grace period spent), or until timeout. Callers use it so the
// process does not exit, and the transcript does not close, under a handler that
// is still writing evidence.
func (s *Server) Wait(timeout time.Duration) {
	select {
	case <-s.stopped:
	case <-time.After(timeout):
	}
}

func (s *Server) ListenAndServe(ctx context.Context) error {
	// Request contexts derive from ctx, so cancelling it releases handlers that are
	// blocked waiting for the operator. Shutdown alone does not: it waits for them.
	s.http.BaseContext = func(net.Listener) context.Context { return ctx }
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := s.http.Shutdown(shutCtx); err != nil {
			s.tx.Note("shutdown-error", map[string]string{"error": err.Error()})
		}
	}()
	var err error
	if s.tls != nil {
		s.http.TLSConfig = s.tls
		err = s.http.ListenAndServeTLS("", "") // certs come from TLSConfig.GetCertificate
	} else {
		err = s.http.ListenAndServe()
	}
	if err == http.ErrServerClosed {
		<-shutdownDone // ErrServerClosed is returned as soon as Shutdown begins
		err = nil
	}
	select {
	case <-s.stopped:
	default:
		close(s.stopped)
	}
	return err
}

// logRequests logs only unrouted (404) requests: they're evidence of harness
// feature probes and unexpected calls.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Marks every response as daiyaku's. Clients ignore unknown headers, and
		// it lets intercept mode prove it is the thing answering for the vendor
		// address rather than something else on the machine.
		w.Header().Set("x-daiyaku", version)
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

// handleProxied forwards one request to the upstream and records the exchange.
func (s *Server) handleProxied(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	seq := int(atomic.AddInt64(&s.seq, 1))
	s.tx.Inbound(seq, redactHeaders(r.Header), decodeForLog(body))
	s.proxy.Forward(w, r, body, s.tx, seq)
}

func (s *Server) handlePrimary(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	seq := int(atomic.AddInt64(&s.seq, 1))

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

	rec := &wireRecorder{ResponseWriter: w, status: http.StatusOK}

	// A malformed action (typo in the composer, hand-written sequence file) must
	// not reach the wire: the blocking encoder would fail after the 200 header is
	// already sent, and the streaming path would ship the fragment verbatim. Fail
	// the request visibly instead, so the harness reports a real error. It is
	// checked before the action is recorded as a "response": that entry is what
	// `daiyaku report` counts as a tool call and replays, and a refused action was
	// never sent. The rejected action is kept in the note as evidence of the attempt.
	if err := action.Validate(); err != nil {
		// The input is stored as a string: it is the thing that failed to parse as
		// JSON, so embedding it as raw JSON would make the note itself unwritable.
		s.tx.Note("invalid-action", map[string]interface{}{
			"error": err.Error(), "seq": req.Seq,
			"action": map[string]string{
				"kind": string(action.Kind), "tool_name": action.ToolName,
				"tool_input": string(action.ToolInput), "text": action.Text,
			},
		})
		writeWireError(rec, err.Error())
		s.tx.Wire(req.Seq, rec.status, headerSnapshot(rec.Header()), rec.buf.String())
		return
	}

	s.tx.Outbound(req.Seq, action)
	if err := s.adapter.WriteResponse(rec, req, action); err != nil {
		s.tx.Note("write-error", map[string]string{"error": err.Error()})
	}
	if rec.err != nil {
		s.tx.Note("client-write-failed", map[string]interface{}{
			"seq": req.Seq, "error": rec.err.Error(), "bytes_received": rec.buf.Len(),
		})
	}
	s.tx.Wire(req.Seq, rec.status, headerSnapshot(rec.Header()), rec.buf.String())
}

// sensitiveFragments mark a header as carrying a credential. Matching on a
// fragment of the name, not an exact list, catches the headers a fixed list
// misses: proxy-authorization, x-goog-api-key, x-amz-security-token, set-cookie,
// anthropic-auth-token, and whatever a harness adds next release.
var sensitiveFragments = []string{
	"authorization", "authentication", "auth-", "-auth", "api-key", "apikey",
	"token", "cookie", "secret", "credential", "password", "signature",
}

func isSensitiveHeader(lowerName string) bool {
	if lowerName == "auth" {
		return true
	}
	for _, f := range sensitiveFragments {
		if strings.Contains(lowerName, f) {
			return true
		}
	}
	return false
}

// Sensitive headers are recorded as "<redacted:present>": evidence shows auth
// was sent without capturing the secret.
func redactHeaders(h http.Header) map[string]string {
	out := map[string]string{}
	for k, v := range h {
		lk := strings.ToLower(k)
		val := strings.Join(v, ", ")
		if isSensitiveHeader(lk) {
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

// wireRecorder tees the response to the transcript as it is written, so the
// evidence holds the exact bytes the harness received rather than a summary of
// what the operator authored. Flush is reimplemented because SSE streaming
// depends on the type assertion to http.Flusher succeeding through the wrapper.
type wireRecorder struct {
	http.ResponseWriter
	buf    bytes.Buffer
	status int
	err    error // first failed write: the harness stopped listening
}

func (wr *wireRecorder) WriteHeader(code int) {
	wr.status = code
	wr.ResponseWriter.WriteHeader(code)
}

// Write records only the bytes the underlying writer accepted. A harness that
// disconnects mid-stream makes the real write fail part way, and buffering the
// whole of p first would claim the harness received bytes it never got.
func (wr *wireRecorder) Write(p []byte) (int, error) {
	n, err := wr.ResponseWriter.Write(p)
	wr.buf.Write(p[:n])
	if err != nil && wr.err == nil {
		wr.err = err
	}
	return n, err
}

func (wr *wireRecorder) Flush() {
	if f, ok := wr.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// headerSnapshot copies the response headers as they stood when the body was
// written. Values here are ours, so nothing needs redacting.
func headerSnapshot(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		out[strings.ToLower(k)] = strings.Join(v, ", ")
	}
	return out
}

// version identifies the mock in the x-daiyaku response header. It is set from
// main at startup; the default keeps the header non-empty in tests.
var version = "dev"

// SetVersion records the build version used in the x-daiyaku header.
func SetVersion(v string) {
	if v != "" {
		version = v
	}
}
