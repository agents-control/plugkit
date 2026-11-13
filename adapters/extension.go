package adapters

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/agents-control/plugkit/discovery"
	"github.com/agents-control/plugkit/host"
)

// Extension declares the engine section extensions.<key> of the adapters file; Check validates it against Schema.
// Discover narrows what GET /v1/adapters shows, always redacted; Weakening judges a change of that shown value in plan.
type Extension struct {
	Schema    json.RawMessage
	Discover  func(json.RawMessage) any
	Weakening func(from, to json.RawMessage) []string
}

// Redacted replaces, in discovery, every value whose key ends in Ref or looks like a secret.
const Redacted = "redacted"

// Extension decodes extensions.<key> into v with JSON tags; an absent section leaves v untouched.
func (f File) Extension(key string, v any) error {
	val, ok := f.Extensions[key]
	if !ok {
		return nil
	}
	raw, err := json.Marshal(val)
	if err == nil {
		err = json.Unmarshal(raw, v)
	}
	if err != nil {
		return fmt.Errorf("extensions.%s: %w", key, err)
	}
	return nil
}

func (spec Spec) extensionEnv(key string) string {
	return spec.EnvPrefix + "_EXTENSION_" + strings.ToUpper(key)
}

func loadExtensions(spec Spec, f *File, env map[string]string) error {
	for _, key := range slices.Sorted(maps.Keys(spec.Extensions)) {
		name := spec.extensionEnv(key)
		v, ok := env[name]
		if !ok || v == "" {
			continue
		}
		if f.Mode == ModeProduction {
			return fmt.Errorf("%s needs an adapters file in production: extensions must be reviewable", name)
		}
		if _, inFile := f.Extensions[key]; inFile {
			return fmt.Errorf("extensions.%s is set in the adapters file and by %s", key, name)
		}
		var val any
		if err := yaml.Unmarshal([]byte(v), &val); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if f.Extensions == nil {
			f.Extensions = map[string]any{}
		}
		f.Extensions[key] = val
	}
	return nil
}

func checkExtensions(spec Spec, f File) Findings {
	var fs Findings
	for _, key := range slices.Sorted(maps.Keys(f.Extensions)) {
		where := "extensions." + key
		ext, ok := spec.Extensions[key]
		if !ok {
			fs = append(fs, Finding{LevelError, where, fmt.Sprintf("unknown extension; %s accepts [%s]", spec.Engine, strings.Join(slices.Sorted(maps.Keys(spec.Extensions)), ", "))})
			continue
		}
		raw, err := json.Marshal(f.Extensions[key])
		if err == nil {
			err = host.ValidateJSON(ext.Schema, raw)
		}
		if err == nil {
			_, err = project(ext, f.Extensions[key])
		}
		if err != nil {
			fs = append(fs, Finding{LevelError, where, err.Error()})
		}
		for _, msg := range literalSecrets(key, f.Extensions[key]) {
			fs = append(fs, Finding{LevelError, where, msg})
		}
	}
	return fs
}

func project(ext Extension, v any) (json.RawMessage, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	if ext.Discover != nil {
		if raw, err = json.Marshal(ext.Discover(raw)); err != nil {
			return nil, fmt.Errorf("discovery projection: %w", err)
		}
	}
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return nil, err
	}
	return json.Marshal(redact(generic))
}

func redact(v any) any {
	switch v := v.(type) {
	case map[string]any:
		for k, e := range v {
			if strings.HasSuffix(k, "Ref") || secretKey.MatchString(k) {
				v[k] = Redacted
			} else {
				v[k] = redact(e)
			}
		}
	case []any:
		for i, e := range v {
			v[i] = redact(e)
		}
	}
	return v
}

func describeExtensions(spec Spec, f File) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	for key, v := range f.Extensions {
		if ext, ok := spec.Extensions[key]; ok {
			if raw, err := project(ext, v); err == nil {
				out[key] = raw
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func diffExtensions(spec Spec, desired File, current discovery.Document) []Change {
	var out []Change
	for _, key := range slices.Sorted(maps.Keys(spec.Extensions)) {
		ext := spec.Extensions[key]
		var to json.RawMessage
		if v, ok := desired.Extensions[key]; ok {
			to, _ = project(ext, v)
		}
		from := current.Extensions[key]
		if bytes.Equal(from, to) {
			continue
		}
		c := Change{Port: "extensions." + key, From: rawList(from), To: rawList(to)}
		if ext.Weakening != nil {
			c.Weakening = ext.Weakening(from, to)
		}
		out = append(out, c)
	}
	return out
}

func rawList(raw json.RawMessage) []string {
	if raw == nil {
		return nil
	}
	return []string{string(raw)}
}
