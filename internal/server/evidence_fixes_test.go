package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// failingWriter accepts the first `accept` bytes of a write and then fails, the
// way a socket does when the harness disconnects part way through a response.
type failingWriter struct {
	*httptest.ResponseRecorder
	accept int
}

func (f *failingWriter) Write(p []byte) (int, error) {
	if len(p) <= f.accept {
		f.accept -= len(p)
		return f.ResponseRecorder.Write(p)
	}
	n := f.accept
	f.accept = 0
	f.ResponseRecorder.Write(p[:n])
	return n, errors.New("connection reset")
}

// The wire capture claims to be what the harness received, so a write the
// harness never got must not be in it.
func TestWireRecorderRecordsOnlyDeliveredBytes(t *testing.T) {
	rec := &wireRecorder{ResponseWriter: &failingWriter{ResponseRecorder: httptest.NewRecorder(), accept: 5}, status: http.StatusOK}

	n, err := rec.Write([]byte("0123456789"))
	if err == nil || n != 5 {
		t.Fatalf("Write = (%d, %v), want (5, error)", n, err)
	}
	if got := rec.buf.String(); got != "01234" {
		t.Errorf("recorded %q, want only the delivered %q", got, "01234")
	}
	if rec.err == nil {
		t.Error("the failed write was not remembered")
	}
}

func TestIsSensitiveHeader(t *testing.T) {
	for _, h := range []string{
		"authorization", "x-api-key", "cookie", "set-cookie", "proxy-authorization",
		"x-goog-api-key", "x-amz-security-token", "anthropic-auth-token", "x-auth-token",
		"openai-api-key", "x-signature", "x-client-secret",
	} {
		if !isSensitiveHeader(h) {
			t.Errorf("%q should be redacted", h)
		}
	}
	// Headers whose value is the evidence (versions, betas, identity of the
	// harness) must stay readable.
	for _, h := range []string{
		"user-agent", "content-type", "anthropic-version", "anthropic-beta",
		"x-claude-code-session-id", "accept", "content-length", "host",
		"x-stainless-lang", "x-app",
	} {
		if isSensitiveHeader(h) {
			t.Errorf("%q should not be redacted", h)
		}
	}
}
