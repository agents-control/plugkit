package adapters

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/agents-control/plugkit/conform"
	"github.com/agents-control/plugkit/discovery"
	"github.com/agents-control/plugkit/host"
)

// Plugins holds the dialed plugins of a desired state with their verified conformance.
type Plugins struct {
	clients     map[string]*host.Client
	conformance map[string]*discovery.Conformance
}

// Dial refuses a file that fails Check (so boot and `adapters check` cannot disagree), dials every plugin a port uses and verifies its report against suites; a missing or failed report is fatal
// in production and degrades the plugin in dev. One failure closes the plugins already dialed.
func Dial(ctx context.Context, h *host.Host, spec Spec, f File, suites ...conform.Suite) (*Plugins, error) {
	if err := Check(spec, f).Err(); err != nil {
		return nil, err
	}
	p := &Plugins{clients: map[string]*host.Client{}, conformance: map[string]*discovery.Conformance{}}
	for _, cfg := range f.Plugins {
		if len(cfg.Ports) == 0 {
			continue
		}
		if f.Mode == ModeProduction && (cfg.Require.Report == "" || cfg.Require.Conformance == "") {
			_ = p.Close()
			return nil, fmt.Errorf("plugin %s: production needs require.conformance and require.report", cfg.Name)
		}
		c, err := h.Dial(ctx, cfg)
		if err != nil {
			_ = p.Close()
			return nil, err
		}
		p.clients[cfg.Name] = c
		if cfg.Require.Report == "" {
			continue
		}
		r, err := conform.VerifyReport(cfg.Require.Report, cfg.Require.Conformance, c.Pin(), c.Ports(), suites)
		if err != nil && f.Mode == ModeProduction {
			_ = p.Close()
			return nil, fmt.Errorf("plugin %s: %w", cfg.Name, err)
		}
		suites := slices.Sorted(maps.Values(r.Suites))
		p.conformance[cfg.Name] = &discovery.Conformance{Report: cfg.Require.Conformance, Suite: strings.Join(suites, ","), Passed: err == nil}
	}
	return p, nil
}

// Client returns the dialed plugin called name.
func (p *Plugins) Client(name string) (*host.Client, bool) {
	c, ok := p.clients[name]
	return c, ok
}

// Close closes every plugin connection.
func (p *Plugins) Close() error {
	var errs []error
	for _, c := range p.clients {
		errs = append(errs, c.Close())
	}
	return errors.Join(errs...)
}

// Describe renders the GET /v1/adapters document of the bound state; a plugin without a verified report is degraded.
func (p *Plugins) Describe(spec Spec, f File, build string) discovery.Document {
	d := discovery.Document{Engine: spec.Engine, Build: build, Mode: f.Mode, Acknowledge: slices.Clone(f.Acknowledge), Extensions: describeExtensions(spec, f), Adapters: []discovery.Row{}}
	for _, name := range slices.Sorted(maps.Keys(f.Ports)) {
		for _, s := range f.Ports[name].Use {
			u, err := ParseUse(s)
			if err != nil {
				continue
			}
			row := discovery.Row{Kind: "adapter", Port: name, Tier: u.Tier, Name: u.Name, Status: host.StateServing, Capabilities: spec.Ports[name].Capabilities[s]}
			if u.Tier == TierMemory {
				row.Name = TierMemory
			}
			if c, ok := p.clients[u.Name]; ok && u.Tier == TierPlugin {
				pin, st := c.Pin(), c.Status()
				row.Version, row.Protocol, row.ManifestDigest = pin.Version, c.Protocol(name), pin.Digest
				row.Capabilities, row.Status = c.Capabilities(name), st.State
				row.Conformance, row.Plaintext = p.conformance[u.Name], c.Config().Plaintext()
				if st.State == host.StateServing && (row.Conformance == nil || !row.Conformance.Passed) {
					row.Status = host.StateDegraded
				}
			}
			d.Adapters = append(d.Adapters, row)
		}
	}
	return d
}
