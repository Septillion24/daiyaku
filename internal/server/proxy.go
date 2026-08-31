package server

import (
	"bytes"
	"io"
	"net/http"
	"net/textproto"
	"time"
	"unicode/utf8"
)

// hopByHop headers are connection-scoped and must not be forwarded by a proxy (RFC 7230 §6.1).
var hopByHop = map[string]bool{
	"Connection": true, "Keep-Alive": true, "Proxy-Authenticate": true,
	"Proxy-Authorization": true, "Te": true, "Trailer": true,
	"Transfer-Encoding": true, "Upgrade": true,
}

type Proxy struct {
	Upstream string
	client   *http.Client
}

// No client-wide Timeout: it covers reading the body too, so a long SSE capture
// (exactly what Step-0 is for) would be cut mid-stream. Bound the phases that
// should be quick instead, and let the request context carry cancellation.
func NewProxy(upstream string) *Proxy {
	return &Proxy{
		Upstream: upstream,
		client: &http.Client{
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				TLSHandshakeTimeout:   15 * time.Second,
				ResponseHeaderTimeout: 2 * time.Minute,
			},
		},
	}
}

func (p *Proxy) Forward(w http.ResponseWriter, r *http.Request, body []byte, tx *Transcript, seq int) {
	target := p.Upstream + r.URL.Path
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	up, err := http.NewRequestWithContext(r.Context(), r.Method, target, bytes.NewReader(body))
	if err != nil {
		http.Error(w, "proxy build: "+err.Error(), http.StatusBadGateway)
		return
	}
	// Dropping Accept-Encoding lets Go's transport request gzip and transparently
	// decompress, so body_sample is readable text; the tradeoff is the recorded
	// exchange is the decoded form, not the compressed wire.
	copyRequestHeaders(up, r)

	resp, err := p.client.Do(up)
	if err != nil {
		tx.Note("proxy-error", map[string]string{"error": err.Error(), "target": target})
		http.Error(w, "proxy: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Drop framing/hop-by-hop headers so they don't fight Go's own response
	// framing as we re-stream the body.
	copyResponseHeaders(w, resp)
	w.WriteHeader(resp.StatusCode)

	bodyLen, sample := streamBody(w, resp, tx)
	tx.Outbound(seq, map[string]interface{}{
		"proxied":     true,
		"status":      resp.StatusCode,
		"upstream":    target,
		"body_len":    bodyLen,
		"body_sample": sample,
	})
}

// copyRequestHeaders forwards the client's headers to the upstream request,
// dropping Host, Content-Length, Accept-Encoding, and hop-by-hop headers.
func copyRequestHeaders(up, r *http.Request) {
	for k, v := range r.Header {
		ck := textproto.CanonicalMIMEHeaderKey(k)
		if ck == "Host" || ck == "Content-Length" || ck == "Accept-Encoding" || hopByHop[ck] {
			continue
		}
		up.Header[k] = v
	}
}

// copyResponseHeaders copies the upstream response headers to the client, dropping
// framing/hop-by-hop headers that would fight Go's own response framing.
func copyResponseHeaders(w http.ResponseWriter, resp *http.Response) {
	for k, v := range resp.Header {
		ck := textproto.CanonicalMIMEHeaderKey(k)
		if ck == "Content-Length" || ck == "Transfer-Encoding" || hopByHop[ck] {
			continue
		}
		w.Header()[k] = v
	}
}

// streamBody re-streams the upstream body to the client while capturing it,
// returning the captured length and a bounded sample for the transcript.
func streamBody(w http.ResponseWriter, resp *http.Response, tx *Transcript) (int, string) {
	var captured bytes.Buffer
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 4096)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			w.Write(buf[:n])
			captured.Write(buf[:n])
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			tx.Note("proxy-read-error", map[string]string{"error": rerr.Error()})
			break
		}
	}
	return captured.Len(), firstN(captured.String(), 8000)
}

// firstN bounds the recorded sample without cutting a rune in half, which would
// put invalid UTF-8 (rendered as replacement characters) into the transcript.
func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
