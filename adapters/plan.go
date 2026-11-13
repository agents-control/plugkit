package adapters

import (
	"maps"
	"slices"

	"github.com/agents-control/plugkit/discovery"
	"github.com/agents-control/plugkit/host"
)

// Change is the diff of one port between the running engine and the desired state.
// Port "mode" carries a change of the engine mode.
type Change struct {
	Port                  string   `json:"port"`
	From                  []string `json:"from"`
	To                    []string `json:"to"`
	Weakening             []string `json:"weakening,omitempty"`
	DataMigrationRequired bool     `json:"data_migration_required,omitempty"`
}

// Plan is the reviewable diff; apply is a restart with the desired file, and the engine never migrates data between stores.
type Plan struct {
	Changes []Change `json:"changes"`
}

// Weakening reports whether any change lowers a guarantee.
func (p Plan) Weakening() bool {
	return slices.ContainsFunc(p.Changes, func(c Change) bool { return len(c.Weakening) > 0 })
}

// DataMigrationRequired reports whether any stateful port changes its adapter.
func (p Plan) DataMigrationRequired() bool {
	return slices.ContainsFunc(p.Changes, func(c Change) bool { return c.DataMigrationRequired })
}

// Diff compares the desired state with the document a running engine serves at GET /v1/adapters.
func Diff(spec Spec, desired File, current discovery.Document) Plan {
	var p Plan
	if current.Mode != desired.Mode {
		c := Change{Port: "mode", From: []string{current.Mode}, To: []string{desired.Mode}}
		if current.Mode == ModeProduction {
			c.Weakening = []string{"dev mode drops the production guards"}
		}
		p.Changes = append(p.Changes, c)
	}
	running := map[string][]string{}
	for _, r := range current.Adapters {
		running[r.Port] = append(running[r.Port], r.Use())
	}
	plugins := map[string]host.Config{}
	for _, pl := range desired.Plugins {
		plugins[pl.Name] = pl
	}
	ports := slices.Sorted(maps.Keys(spec.Ports))
	for _, name := range ports {
		from, to := sorted(running[name]), sorted(desired.Ports[name].Use)
		if slices.Equal(from, to) {
			continue
		}
		ps := spec.Ports[name]
		c := Change{Port: name, From: from, To: to, DataMigrationRequired: ps.Stateful && len(from) > 0}
		for _, s := range to {
			c.Weakening = append(c.Weakening, weakens(desired, ps, plugins, s)...)
		}
		p.Changes = append(p.Changes, c)
	}
	return p
}

func weakens(f File, ps PortSpec, plugins map[string]host.Config, s string) []string {
	u, err := ParseUse(s)
	if err != nil {
		return nil
	}
	var out []string
	if u.Tier == TierMemory && f.Mode == ModeProduction {
		out = append(out, "memory in production")
	}
	if ps.Sensitive && (u.Tier == TierPlugin || slices.Contains(ps.Weak, s)) {
		out = append(out, "security port served by "+s)
	}
	if p, ok := plugins[u.Name]; ok && u.Tier == TierPlugin {
		if p.Plaintext() {
			out = append(out, "plaintext transport to "+s)
		}
		if p.Require.Conformance == "" {
			out = append(out, "no conformance report for "+s)
		}
	}
	return out
}

func sorted(s []string) []string {
	out := slices.Clone(s)
	slices.Sort(out)
	return out
}
