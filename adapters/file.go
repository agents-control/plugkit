// Package adapters reads the adapters.yaml desired state and its env form, checks it, plans it against a running
// engine (weakening and data_migration_required flags), dials its plugins and describes the result (UP-ADR-002).
package adapters

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/agents-control/plugkit/host"
)

// File header values and modes.
const (
	APIVersion     = "acplug/v1"
	Kind           = "Adapters"
	ModeProduction = "production"
	ModeDev        = "dev"
)

// Tiers of an adapter selection.
const (
	TierBuiltin = "builtin"
	TierMemory  = "memory"
	TierPlugin  = "plugin"
)

// Spec describes one engine's ports; the engine declares it once and every check, plan and env alias derives from it.
type Spec struct {
	Engine    string
	EnvPrefix string
	Ports     map[string]PortSpec
}

// PortSpec says which selections a port accepts and how a change to it is judged.
// Stateful changes need a data migration; Sensitive ports weaken when served by a plugin or by a Weak built-in.
type PortSpec struct {
	Default      []string
	Builtins     []string
	Memory       bool
	Plugin       bool
	Multi        bool
	Stateful     bool
	Sensitive    bool
	Weak         []string
	Aliases      []string
	Capabilities map[string][]string
}

// File is the adapters.yaml document, or the same desired state built from the env form.
type File struct {
	APIVersion  string          `yaml:"apiVersion"`
	Kind        string          `yaml:"kind"`
	Engine      string          `yaml:"engine"`
	Mode        string          `yaml:"mode"`
	AllowMemory bool            `yaml:"allowMemory"`
	Ports       map[string]Port `yaml:"ports"`
	Plugins     []host.Config   `yaml:"plugins"`
}

// Port selects the adapters of one port; Config holds references, never secret values.
type Port struct {
	Use    Uses           `yaml:"use"`
	Config map[string]any `yaml:"config"`
}

// Uses is one selection or a list of them; only multi-valued ports accept more than one.
type Uses []string

// UnmarshalYAML accepts a scalar or a sequence.
func (u *Uses) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		*u = Uses{n.Value}
		return nil
	}
	var list []string
	if err := n.Decode(&list); err != nil {
		return err
	}
	*u = list
	return nil
}

// Use is a parsed selection.
type Use struct {
	Tier string
	Name string
}

// ParseUse reads "memory", "builtin:<name>" or "plugin:<name>".
func ParseUse(s string) (Use, error) {
	if s == TierMemory {
		return Use{Tier: TierMemory}, nil
	}
	tier, name, ok := strings.Cut(s, ":")
	if !ok || name == "" || (tier != TierBuiltin && tier != TierPlugin) {
		return Use{}, fmt.Errorf("use %q must be memory, builtin:<name> or plugin:<name>", s)
	}
	return Use{Tier: tier, Name: name}, nil
}

// Parse decodes adapters.yaml strictly: unknown keys are errors.
func Parse(data []byte) (File, error) {
	var f File
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) {
		return File{}, fmt.Errorf("adapters file: %w", err)
	}
	return f, nil
}

// Load builds the desired state from the file (nil when absent) and the env form, then applies the port defaults.
// A port set in both the file and env, or by two env names with different values, is an error: there is no silent precedence.
func Load(spec Spec, data []byte, environ []string) (File, error) {
	env := map[string]string{}
	for _, kv := range environ {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	f := File{APIVersion: APIVersion, Kind: Kind, Engine: spec.Engine, Mode: ModeProduction}
	if data != nil {
		var err error
		if f, err = Parse(data); err != nil {
			return File{}, err
		}
	}
	if mode, ok := env[spec.EnvPrefix+"_MODE"]; ok {
		if data != nil {
			return File{}, fmt.Errorf("%s_MODE is set and the adapters file sets mode", spec.EnvPrefix)
		}
		f.Mode = mode
	}
	if allow, ok := env[spec.EnvPrefix+"_ALLOW_MEMORY"]; ok {
		if data != nil && (allow == "true") != f.AllowMemory {
			return File{}, fmt.Errorf("%s_ALLOW_MEMORY=%s contradicts allowMemory of the adapters file", spec.EnvPrefix, allow)
		}
		f.AllowMemory = allow == "true"
	}
	if f.Ports == nil {
		f.Ports = map[string]Port{}
	}
	for _, name := range slices.Sorted(maps.Keys(spec.Ports)) {
		ps := spec.Ports[name]
		uses, from, err := envUses(env, append([]string{spec.EnvPrefix + "_" + strings.ToUpper(name)}, ps.Aliases...))
		if err != nil {
			return File{}, err
		}
		_, inFile := f.Ports[name]
		switch {
		case uses != nil && inFile:
			return File{}, fmt.Errorf("port %s is set in the adapters file and by %s", name, from)
		case uses != nil:
			f.Ports[name] = Port{Use: uses}
		case !inFile && len(ps.Default) > 0:
			f.Ports[name] = Port{Use: slices.Clone(ps.Default)}
		}
	}
	bindPlugins(&f)
	return f, nil
}

func envUses(env map[string]string, names []string) (Uses, string, error) {
	var uses Uses
	from := ""
	for _, n := range names {
		v, ok := env[n]
		if !ok || v == "" {
			continue
		}
		var got Uses
		for _, s := range strings.Split(v, ",") {
			s = strings.TrimSpace(s)
			if s != TierMemory && !strings.Contains(s, ":") {
				s = TierBuiltin + ":" + s
			}
			got = append(got, s)
		}
		if uses != nil && !slices.Equal(uses, got) {
			return nil, "", fmt.Errorf("%s and %s select different adapters", from, n)
		}
		uses, from = got, n
	}
	return uses, from, nil
}

func bindPlugins(f *File) {
	for i := range f.Plugins {
		p := &f.Plugins[i]
		var used []string
		for _, name := range slices.Sorted(maps.Keys(f.Ports)) {
			if slices.Contains(f.Ports[name].Use, TierPlugin+":"+p.Name) {
				used = append(used, name)
				if cfg := f.Ports[name].Config; cfg != nil {
					if p.Settings == nil {
						p.Settings = map[string]any{}
					}
					p.Settings[name] = cfg
				}
			}
		}
		if len(p.Ports) == 0 {
			p.Ports = used
		}
	}
}
