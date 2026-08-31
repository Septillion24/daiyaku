// Package adapter defines the per-provider serialization boundary. Each adapter
// turns an inbound HTTP request into a neutral.Request and serializes a
// neutral.Action back into the provider's wire format, in both blocking and
// streaming (SSE) forms.
package adapter

import (
	"net/http"

	"daiyaku/internal/neutral"
)

type Adapter interface {
	Provider() string

	Normalize(h http.Header, body []byte) (*neutral.Request, error)

	// WriteResponse honors req.Stream (SSE vs blocking JSON) and writes the full
	// response, headers included.
	WriteResponse(w http.ResponseWriter, req *neutral.Request, action neutral.Action) error

	Routes() Routes
}

type Routes struct {
	Primary string
	// Aux endpoints bypass the operator loop.
	Aux map[string]http.HandlerFunc
}

type Factory func() Adapter

var registry = map[string]Factory{}

func Register(id string, f Factory) { registry[id] = f }

func New(id string) (Adapter, bool) {
	f, ok := registry[id]
	if !ok {
		return nil, false
	}
	return f(), true
}

func Providers() []string {
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	return out
}
