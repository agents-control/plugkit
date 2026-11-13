package adapters

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/agents-control/plugkit/host"
)

// Finding levels.
const (
	LevelError   = "error"
	LevelWarning = "warning"
)

// Finding is one problem of a desired state; Where names the port, plugin or header field.
type Finding struct {
	Level   string
	Where   string
	Message string
}

func (f Finding) String() string { return f.Level + " " + f.Where + ": " + f.Message }

// Findings is the result of Check.
type Findings []Finding

// Err joins the error findings, nil when there are none.
func (fs Findings) Err() error {
	var errs []error
	for _, f := range fs {
		if f.Level == LevelError {
			errs = append(errs, errors.New(f.String()))
		}
	}
	return errors.Join(errs...)
}

// Warnings lists the warning findings.
func (fs Findings) Warnings() []string {
	var out []string
	for _, f := range fs {
		if f.Level == LevelWarning {
			out = append(out, f.String())
		}
	}
	return out
}

var secretKey = regexp.MustCompile(`(?i)(secret|token|password|passwd|credential|apikey|api_key|private)`)

// Check validates a loaded desired state offline: header, every selection against the spec, plugin wiring,
// the production guard, conformance requirements, literal secrets in config and the engine's own Spec.Validate rules.
func Check(spec Spec, f File) Findings {
	var fs Findings
	add := func(level, where, format string, args ...any) {
		fs = append(fs, Finding{Level: level, Where: where, Message: fmt.Sprintf(format, args...)})
	}
	if f.APIVersion != APIVersion || f.Kind != Kind {
		add(LevelError, "header", "apiVersion must be %s and kind %s", APIVersion, Kind)
	}
	if f.Engine != spec.Engine {
		add(LevelError, "engine", "file is for %q, this binary is %q", f.Engine, spec.Engine)
	}
	if f.Mode != ModeProduction && f.Mode != ModeDev {
		add(LevelError, "mode", "mode must be production or dev, got %q", f.Mode)
	}
	prod := f.Mode == ModeProduction
	for _, a := range f.Acknowledge {
		if !slices.Contains(spec.Acknowledgements, a) {
			add(LevelError, "acknowledge", "unknown acknowledgement %q; %s accepts %s", a, spec.Engine, strings.Join(spec.Acknowledgements, ", "))
		}
	}
	plugins := map[string]host.Config{}
	for _, p := range f.Plugins {
		where := "plugin " + p.Name
		if err := p.Validate(); err != nil {
			add(LevelError, where, "%v", err)
		}
		if _, dup := plugins[p.Name]; dup {
			add(LevelError, where, "duplicate plugin name")
		}
		plugins[p.Name] = p
		switch {
		case (p.Require.Conformance == "") != (p.Require.Report == ""):
			add(LevelError, where, "require.conformance and require.report go together")
		case p.Require.Conformance == "" && prod:
			add(LevelError, where, "production needs require.conformance and require.report: a passing conform report and its digest")
		case p.Require.Conformance == "":
			add(LevelWarning, where, "no conformance report required")
		}
		switch {
		case p.Plaintext() && p.CredentialRef != "" && prod:
			add(LevelError, where, "bearer token over plaintext to a non-loopback endpoint")
		case p.Plaintext():
			add(LevelWarning, where, "plaintext transport to a non-loopback endpoint")
		}
		if len(p.Ports) == 0 {
			add(LevelWarning, where, "no port uses this plugin")
		}
	}
	for _, name := range slices.Sorted(maps.Keys(f.Ports)) {
		port, where := f.Ports[name], "port "+name
		ps, known := spec.Ports[name]
		if !known {
			add(LevelError, where, "unknown port; %s has %s", spec.Engine, strings.Join(slices.Sorted(maps.Keys(spec.Ports)), ", "))
			continue
		}
		if len(port.Use) == 0 {
			add(LevelError, where, "use is empty")
		}
		if len(port.Use) > 1 && !ps.Multi {
			add(LevelError, where, "single-valued port selects %d adapters", len(port.Use))
		}
		seen := map[string]bool{}
		for _, s := range port.Use {
			if seen[s] {
				add(LevelError, where, "%s selected twice", s)
			}
			seen[s] = true
			u, err := ParseUse(s)
			if err != nil {
				add(LevelError, where, "%v", err)
				continue
			}
			switch u.Tier {
			case TierMemory:
				if !ps.Memory {
					add(LevelError, where, "has no memory adapter")
				} else if prod && !f.AllowMemory {
					add(LevelError, where, "memory is refused in production unless allowMemory or %s_ALLOW_MEMORY=true", spec.EnvPrefix)
				}
			case TierBuiltin:
				if !slices.Contains(ps.Builtins, u.Name) {
					add(LevelError, where, "unknown built-in %q; built-ins are %s", u.Name, strings.Join(ps.Builtins, ", "))
				}
			case TierPlugin:
				p, ok := plugins[u.Name]
				switch {
				case !ps.Plugin:
					add(LevelError, where, "cannot be served by a plugin")
				case !ok:
					add(LevelError, where, "plugin %q is not declared under plugins", u.Name)
				case !slices.Contains(p.Ports, name):
					add(LevelError, where, "plugin %q does not list this port under ports", u.Name)
				}
			}
		}
		for _, msg := range literalSecrets("", port.Config) {
			add(LevelError, where, "%s", msg)
		}
		for _, msg := range builtinConfig(ps, port) {
			add(LevelError, where, "%s", msg)
		}
	}
	for _, p := range f.Plugins {
		for _, name := range p.Ports {
			if _, ok := spec.Ports[name]; !ok {
				add(LevelError, "plugin "+p.Name, "lists unknown port %q", name)
			}
		}
	}
	if spec.Validate != nil {
		fs = append(fs, spec.Validate(f)...)
	}
	return fs
}

func builtinConfig(ps PortSpec, port Port) []string {
	declared := map[string]ConfigKind{}
	plugin := false
	for _, s := range port.Use {
		plugin = plugin || strings.HasPrefix(s, TierPlugin+":")
		maps.Copy(declared, ps.BuiltinConfig[s])
	}
	var out []string
	for _, k := range slices.Sorted(maps.Keys(port.Config)) {
		kind, ok := declared[k]
		if !ok {
			if !plugin {
				out = append(out, fmt.Sprintf("%s does not accept config key %q", strings.Join(port.Use, ", "), k))
			}
			continue
		}
		v := port.Config[k]
		var valid bool
		switch kind {
		case ConfigRef:
			s, isString := v.(string)
			valid = isString && host.IsRef(s)
		case ConfigString:
			_, valid = v.(string)
		case ConfigBool:
			_, valid = v.(bool)
		case ConfigNumber:
			switch v.(type) {
			case int, int64, uint64, float64:
				valid = true
			}
		}
		if !valid {
			out = append(out, fmt.Sprintf("config %s must be a %s", k, kind))
		}
	}
	return out
}

func literalSecrets(path string, cfg map[string]any) []string {
	var out []string
	for _, k := range slices.Sorted(maps.Keys(cfg)) {
		key := strings.TrimPrefix(path+"."+k, ".")
		switch v := cfg[k].(type) {
		case map[string]any:
			out = append(out, literalSecrets(key, v)...)
		case string:
			isRef := strings.HasSuffix(k, "Ref")
			if isRef && !host.IsRef(v) {
				out = append(out, fmt.Sprintf("config %s must be a reference such as env:NAME", key))
			}
			if !isRef && secretKey.MatchString(k) {
				out = append(out, fmt.Sprintf("config %s looks like a literal secret; use %sRef with a reference", key, k))
			}
		}
	}
	return out
}
