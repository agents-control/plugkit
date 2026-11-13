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
// Acknowledgements names the opt-ins a file may list, Extensions its own sections; Validate adds engine rules to Check.
type Spec struct {
	Engine           string
	EnvPrefix        string
	Ports            map[string]PortSpec
	Acknowledgements []string
	Extensions       map[string]Extension
	Validate         func(File) Findings
}

// PortSpec says which selections a port accepts and how a change is judged: Stateful needs a data migration, Sensitive
// weakens on a plugin or Weak built-in; BuiltinConfig lists the config keys each "builtin:<name>" accepts, nothing else.
type PortSpec struct {
	Default       []string
	Builtins      []string
	Memory        bool
	Plugin        bool
	Multi         bool
	Stateful      bool
	Sensitive     bool
	Weak          []string
	Aliases       []string
	Capabilities  map[string][]string
	BuiltinConfig map[string]map[string]ConfigKind
}

// ConfigKind is the value type a built-in config key accepts.
type ConfigKind string

// Config kinds; ConfigRef values must be references such as env:NAME.
const (
	ConfigRef    ConfigKind = "ref"
	ConfigString ConfigKind = "string"
	ConfigBool   ConfigKind = "bool"
	ConfigNumber ConfigKind = "number"
)

// File is the adapters.yaml document, or the same desired state built from the env form.
type File struct {
	APIVersion  string          `yaml:"apiVersion"`
	Kind        string          `yaml:"kind"`
	Engine      string          `yaml:"engine"`
	Mode        string          `yaml:"mode"`
	AllowMemory bool            `yaml:"allowMemory"`
	Acknowledge []string        `yaml:"acknowledge"`
	Ports       map[string]Port `yaml:"ports"`
	Plugins     []host.Config   `yaml:"plugins"`
	Extensions  map[string]any  `yaml:"extensions"`
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
	if ack, ok := env[spec.EnvPrefix+"_ACKNOWLEDGE"]; ok {
		got := splitList(ack)
		switch {
		case data != nil && !sameSet(got, f.Acknowledge):
			return File{}, fmt.Errorf("%s_ACKNOWLEDGE=%s contradicts acknowledge of the adapters file", spec.EnvPrefix, ack)
		case data == nil && len(got) > 0 && f.Mode == ModeProduction:
			return File{}, fmt.Errorf("%s_ACKNOWLEDGE needs an adapters file in production: acknowledgements must be reviewable", spec.EnvPrefix)
		}
		f.Acknowledge = got
	}
	if err := loadExtensions(spec, &f, env); err != nil {
		return File{}, err
	}
	if f.Ports == nil {
		f.Ports = map[string]Port{}
	}
	for _, name := range slices.Sorted(maps.Keys(spec.Ports)) {
		ps := spec.Ports[name]
		uses, from, err := envUses(env, spec.portEnv(name))
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

func splitList(v string) []string {
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func sameSet(a, b []string) bool {
	a, b = slices.Sorted(slices.Values(a)), slices.Sorted(slices.Values(b))
	return slices.Equal(slices.Compact(a), slices.Compact(b))
}

// Acknowledged reports whether the file or <P>_ACKNOWLEDGE lists the opt-in name.
func (f File) Acknowledged(name string) bool { return slices.Contains(f.Acknowledge, name) }

// EnvNames lists every variable the adapters layer reads: <P>_ADAPTERS_FILE, <P>_MODE, <P>_ALLOW_MEMORY,
// <P>_ACKNOWLEDGE, each port's <P>_<PORT> and aliases in port order, then each <P>_EXTENSION_<KEY> in key order.
func EnvNames(spec Spec) []string {
	names := []string{spec.EnvPrefix + "_ADAPTERS_FILE", spec.EnvPrefix + "_MODE", spec.EnvPrefix + "_ALLOW_MEMORY", spec.EnvPrefix + "_ACKNOWLEDGE"}
	for _, port := range slices.Sorted(maps.Keys(spec.Ports)) {
		names = append(names, spec.portEnv(port)...)
	}
	for _, key := range slices.Sorted(maps.Keys(spec.Extensions)) {
		names = append(names, spec.extensionEnv(key))
	}
	return names
}

// Environ returns the KEY=value pairs of EnvNames that lookup finds, ready for Load; os.LookupEnv fits lookup.
func Environ(spec Spec, lookup func(string) (string, bool)) []string {
	var out []string
	for _, n := range EnvNames(spec) {
		if v, ok := lookup(n); ok {
			out = append(out, n+"="+v)
		}
	}
	return out
}

func (spec Spec) portEnv(port string) []string {
	return append([]string{spec.EnvPrefix + "_" + strings.ToUpper(port)}, spec.Ports[port].Aliases...)
}

// First returns the first selection of port, false when the port is unset or its first use does not parse.
func (f File) First(port string) (Use, bool) {
	uses := f.Ports[port].Use
	if len(uses) == 0 {
		return Use{}, false
	}
	u, err := ParseUse(uses[0])
	return u, err == nil
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
