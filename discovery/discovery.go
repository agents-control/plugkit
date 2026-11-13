// Package discovery is the GET /v1/adapters document every engine serves: which adapter runs each port,
// its tier, pinned identity, narrowed capabilities, status and conformance, in the shape of AD-7 GET /extensions rows.
package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
)

// Path is where engines serve the document.
const Path = "/v1/adapters"

// Document lists the adapters bound at boot.
type Document struct {
	Engine string `json:"engine"`
	Build  string `json:"build,omitempty"`
	Mode   string `json:"mode,omitempty"`
	// Acknowledge lists the opt-ins the running engine was started with.
	Acknowledge []string `json:"acknowledge,omitempty"`
	Adapters    []Row    `json:"adapters"`
}

// Row is one adapter serving one port; a multi-valued port has one row per provider.
type Row struct {
	Kind           string       `json:"kind"`
	Port           string       `json:"port"`
	Tier           string       `json:"tier"`
	Name           string       `json:"name"`
	Version        string       `json:"version,omitempty"`
	Protocol       string       `json:"protocol,omitempty"`
	ManifestDigest string       `json:"manifestDigest,omitempty"`
	Capabilities   []string     `json:"capabilities,omitempty"`
	Status         string       `json:"status"`
	Plaintext      bool         `json:"plaintext,omitempty"`
	Conformance    *Conformance `json:"conformance,omitempty"`
}

// Conformance is the verified conformance report of a plugin row.
type Conformance struct {
	Report string `json:"report"`
	Suite  string `json:"suite,omitempty"`
	Passed bool   `json:"passed"`
}

// TierEmbedded marks a port served by an adapter the embedder injected in Go, outside the adapter file.
const TierEmbedded = "embedded"

// EmbeddedRow is the serving row of an adapter the embedder injected for port under name.
func EmbeddedRow(port, name string) Row {
	return Row{Kind: "adapter", Port: port, Tier: TierEmbedded, Name: name, Status: "serving"}
}

// Rows returns a copy of the rows of port, in document order.
func (d Document) Rows(port string) []Row {
	var out []Row
	for _, r := range d.Adapters {
		if r.Port == port {
			out = append(out, r)
		}
	}
	return out
}

// ReplacePort swaps every row of port for rows, kept in place of the first old row or appended; other ports are untouched.
func (d *Document) ReplacePort(port string, rows ...Row) {
	i := slices.IndexFunc(d.Adapters, func(r Row) bool { return r.Port == port })
	if i < 0 {
		i = len(d.Adapters)
	}
	kept := slices.DeleteFunc(slices.Clone(d.Adapters), func(r Row) bool { return r.Port == port })
	i = min(i, len(kept))
	d.Adapters = slices.Concat(kept[:i], rows, kept[i:])
}

// Use renders the row as an adapter selection: "memory", "builtin:<name>", "plugin:<name>" or "embedded:<name>".
func (r Row) Use() string {
	if r.Tier == "memory" {
		return "memory"
	}
	return r.Tier + ":" + r.Name
}

// Handler serves the document returned by doc on GET.
func Handler(doc func() Document) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(doc())
	})
}

// Option adjusts the Fetch request, typically to authenticate against an operator-only endpoint.
type Option func(*http.Request)

// WithHeader sets one request header.
func WithHeader(name, value string) Option {
	return func(r *http.Request) { r.Header.Set(name, value) }
}

// WithBearer sends Authorization: Bearer token.
func WithBearer(token string) Option { return WithHeader("Authorization", "Bearer "+token) }

// Fetch reads the document from a running engine at base, for example http://127.0.0.1:8080.
func Fetch(ctx context.Context, client *http.Client, base string, opts ...Option) (Document, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+Path, nil)
	if err != nil {
		return Document{}, err
	}
	for _, o := range opts {
		o(req)
	}
	resp, err := client.Do(req)
	if err != nil {
		return Document{}, fmt.Errorf("fetch adapters: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Document{}, fmt.Errorf("fetch adapters: http status %d", resp.StatusCode)
	}
	var d Document
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&d); err != nil {
		return Document{}, fmt.Errorf("decode adapters: %w", err)
	}
	return d, nil
}
