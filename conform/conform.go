// Package conform runs port suites against any plugin endpoint and writes the conformance report;
// engines register their suites, and Verify proves that every broken fixture is refused.
package conform

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"time"

	"google.golang.org/grpc"

	"github.com/agents-control/plugkit/host"
)

// ReportFormat identifies the report schema.
const ReportFormat = "acplug.conform/1"

const (
	caseTimeout   = 30 * time.Second
	latencySample = 20
)

// Case is one observable check; Run returns nil when the plugin behaves as the port requires.
type Case struct {
	Name string
	Run  func(ctx context.Context) error
}

// Suite is the conformance suite of one port; Cases builds the checks against the port's guarded connection.
type Suite struct {
	Port    string
	Version string
	Cases   func(conn grpc.ClientConnInterface) []Case
}

// Result is the outcome of one case; Port is empty for the protocol cases every plugin runs.
type Result struct {
	Port   string  `json:"port,omitempty"`
	Case   string  `json:"case"`
	Passed bool    `json:"passed"`
	Error  string  `json:"error,omitempty"`
	Millis float64 `json:"ms"`
}

// Latency holds Health round-trip percentiles in milliseconds.
type Latency struct {
	P50 float64 `json:"p50Ms"`
	P99 float64 `json:"p99Ms"`
}

// Report is the JSON document a conformance run produces; its Digest is what an adapter file requires.
type Report struct {
	Format         string            `json:"format"`
	Plugin         string            `json:"plugin,omitempty"`
	Version        string            `json:"version,omitempty"`
	Protocol       string            `json:"protocol,omitempty"`
	ManifestDigest string            `json:"manifestDigest,omitempty"`
	Suites         map[string]string `json:"suites"`
	Cases          []Result          `json:"cases"`
	Latency        Latency           `json:"latency"`
	Passed         bool              `json:"passed"`
}

// Digest is sha256 over the report's JSON encoding.
func (r Report) Digest() string {
	raw, _ := json.Marshal(r)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Failed lists the names of the failing cases.
func (r Report) Failed() []string {
	var out []string
	for _, c := range r.Cases {
		if !c.Passed {
			out = append(out, c.Case)
		}
	}
	return out
}

// Run dials the plugin through h, checks the protocol cases and runs the suite of every enabled port.
// A port without a registered suite fails: nothing proves it.
func Run(ctx context.Context, h *host.Host, cfg host.Config, suites ...Suite) Report {
	r := Report{Format: ReportFormat, Suites: map[string]string{}}
	c, err := h.Dial(ctx, cfg)
	if err != nil {
		r.Cases = append(r.Cases, Result{Case: "handshake", Error: err.Error()})
		return r
	}
	defer func() { _ = c.Close() }()
	pin := c.Pin()
	r.Plugin, r.Version, r.Protocol, r.ManifestDigest = pin.Name, pin.Version, pin.Protocol, pin.Digest
	r.Cases = append(r.Cases, Result{Case: "handshake", Passed: true})
	r.record(ctx, "", Case{Name: "manifest is stable", Run: func(ctx context.Context) error {
		if st := c.Recheck(ctx); st.State != host.StateServing {
			return fmt.Errorf("%s: %s", st.State, st.Detail)
		}
		return nil
	}})
	r.record(ctx, "", Case{Name: "health answers", Run: func(ctx context.Context) error {
		var err error
		r.Latency, err = healthLatency(ctx, c)
		return err
	}})
	for _, port := range c.Ports() {
		i := slices.IndexFunc(suites, func(s Suite) bool { return s.Port == port })
		if i < 0 {
			r.Cases = append(r.Cases, Result{Port: port, Case: "suite available", Error: "no suite for this port in this binary; run the engine's conform"})
			continue
		}
		r.Suites[port] = suites[i].Version
		conn, _ := c.Port(port)
		for _, k := range suites[i].Cases(conn) {
			r.record(ctx, port, k)
		}
	}
	r.Passed = len(r.Failed()) == 0
	return r
}

func (r *Report) record(ctx context.Context, port string, k Case) {
	ctx, cancel := context.WithTimeout(ctx, caseTimeout)
	defer cancel()
	start := time.Now()
	err := k.Run(ctx)
	res := Result{Port: port, Case: k.Name, Passed: err == nil, Millis: float64(time.Since(start).Microseconds()) / 1000}
	if err != nil {
		res.Error = err.Error()
	}
	r.Cases = append(r.Cases, res)
}

func healthLatency(ctx context.Context, c *host.Client) (Latency, error) {
	samples := make([]float64, 0, latencySample)
	for range latencySample {
		start := time.Now()
		if err := c.Health(ctx); err != nil {
			return Latency{}, err
		}
		samples = append(samples, float64(time.Since(start).Microseconds())/1000)
	}
	slices.Sort(samples)
	return Latency{P50: samples[len(samples)/2], P99: samples[len(samples)*99/100]}, nil
}

// Fixture is a plugin endpoint with its expected verdict: Want names the case that must fail, empty when it must pass.
type Fixture struct {
	Name   string
	Config host.Config
	Want   string
}

// Verify runs every fixture and returns an error for each one whose verdict differs from Want.
func Verify(ctx context.Context, h *host.Host, fixtures []Fixture, suites ...Suite) error {
	var errs []error
	for _, f := range fixtures {
		r := Run(ctx, h, f.Config, suites...)
		failed := r.Failed()
		switch {
		case f.Want == "" && !r.Passed:
			errs = append(errs, fmt.Errorf("%s: must pass, failed %v", f.Name, failed))
		case f.Want != "" && !slices.Contains(failed, f.Want):
			errs = append(errs, fmt.Errorf("%s: case %q must fail, failed %v", f.Name, f.Want, failed))
		}
	}
	return errors.Join(errs...)
}

// VerifyReport reads the report at path and requires its digest, a pass, and the manifest digest the host pinned.
func VerifyReport(path, digest string, pin host.Pin) (Report, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Report{}, fmt.Errorf("conformance report: %w", err)
	}
	var r Report
	if err := json.Unmarshal(raw, &r); err != nil {
		return Report{}, fmt.Errorf("conformance report: %w", err)
	}
	switch {
	case r.Format != ReportFormat:
		return r, fmt.Errorf("conformance report format %q, want %s", r.Format, ReportFormat)
	case r.Digest() != digest:
		return r, fmt.Errorf("conformance report digest %s differs from the required %s", r.Digest(), digest)
	case !r.Passed:
		return r, fmt.Errorf("conformance report failed %v", r.Failed())
	case r.ManifestDigest != pin.Digest:
		return r, fmt.Errorf("conformance report is for manifest %s, plugin serves %s", r.ManifestDigest, pin.Digest)
	}
	return r, nil
}
