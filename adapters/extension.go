package adapters

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/agents-control/plugkit/discovery"
	"github.com/agents-control/plugkit/host"
)

// Extension declares the engine section extensions.<key>; Schema, Discover and Weakening are all required.
// Discover returns what GET /v1/adapters shows (refused if it exposes a secret); Weakening judges two such projections.
type Extension struct {
	Schema    json.RawMessage
	Discover  func(json.RawMessage) any
	Weakening func(from, to json.RawMessage) []string
}

// Redacted prefixes the digest shown in discovery in place of every *Ref value.
const Redacted = "redacted:"

var (
	disclosing      = regexp.MustCompile(`(?i)(pass|auth|signing|dsn|cookie|session|key$)`)
	credentialValue = regexp.MustCompile(`(?i)(\bpassword=|\bpwd=|\bbearer\s|\bbasic\s)`)
)

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
	for _, key := range slices.Sorted(maps.Keys(spec.Extensions)) {
		ext := spec.Extensions[key]
		err := host.ValidateJSON(ext.Schema, nil)
		if ext.Discover == nil || ext.Weakening == nil {
			err = errors.Join(err, errors.New("an extension needs Discover and Weakening"))
		}
		if err != nil {
			fs = append(fs, Finding{LevelError, "spec extensions." + key, err.Error()})
		}
	}
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
	if ext.Discover == nil {
		return nil, errors.New("no Discover projection")
	}
	raw, err := json.Marshal(v)
	if err == nil {
		raw, err = json.Marshal(ext.Discover(raw))
	}
	var generic any
	if err == nil {
		err = json.Unmarshal(raw, &generic)
	}
	if err != nil {
		return nil, fmt.Errorf("discovery projection: %w", err)
	}
	if bad := exposures("", generic); len(bad) > 0 {
		return nil, fmt.Errorf("discovery projection exposes %s; leave secrets out of Discover", strings.Join(bad, ", "))
	}
	return json.Marshal(redactRefs(generic))
}

func exposures(path string, v any) []string {
	var out []string
	switch v := v.(type) {
	case map[string]any:
		for _, k := range slices.Sorted(maps.Keys(v)) {
			p := strings.TrimPrefix(path+"."+k, ".")
			s, isString := v[k].(string)
			switch {
			case strings.HasSuffix(k, "Ref"):
				if !isString || !host.IsRef(s) {
					out = append(out, p)
				}
			case secretKey.MatchString(k) || disclosing.MatchString(k):
				out = append(out, p)
			default:
				out = append(out, exposures(p, v[k])...)
			}
		}
	case []any:
		for i, e := range v {
			out = append(out, exposures(fmt.Sprintf("%s[%d]", path, i), e)...)
		}
	case string:
		if credentialValue.MatchString(v) || embedsCredentials(v) {
			out = append(out, path)
		}
	}
	return out
}

func redactRefs(v any) any {
	switch v := v.(type) {
	case map[string]any:
		for k, e := range v {
			if s, ok := e.(string); ok && strings.HasSuffix(k, "Ref") {
				sum := sha256.Sum256([]byte(s))
				v[k] = Redacted + hex.EncodeToString(sum[:4])
			} else {
				v[k] = redactRefs(e)
			}
		}
	case []any:
		for i, e := range v {
			v[i] = redactRefs(e)
		}
	}
	return v
}

func describeExtensions(spec Spec, f File) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	for key, v := range f.Extensions {
		if raw, err := project(spec.Extensions[key], v); err == nil {
			out[key] = raw
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
		ext, port := spec.Extensions[key], "extensions."+key
		var to json.RawMessage
		if v, ok := desired.Extensions[key]; ok {
			var err error
			if to, err = project(ext, v); err != nil {
				out = append(out, Change{Port: port, Weakening: []string{err.Error()}})
				continue
			}
		}
		from := current.Extensions[key]
		before, after := decode(from), decode(to)
		if reflect.DeepEqual(before, after) {
			continue
		}
		c := Change{Port: port, From: rawList(from), To: rawList(to), Notes: refChanges("", before, after)}
		if ext.Weakening == nil {
			c.Weakening = []string{"extension " + key + " changed; no classifier"}
		} else {
			c.Weakening = ext.Weakening(from, to)
		}
		out = append(out, c)
	}
	return out
}

func decode(raw json.RawMessage) any {
	var v any
	if raw != nil && json.Unmarshal(raw, &v) != nil {
		return string(raw)
	}
	return v
}

func refChanges(path string, a, b any) []string {
	var out []string
	switch a := a.(type) {
	case map[string]any:
		if b, ok := b.(map[string]any); ok {
			for _, k := range slices.Sorted(maps.Keys(a)) {
				out = append(out, refChanges(strings.TrimPrefix(path+"."+k, "."), a[k], b[k])...)
			}
		}
	case []any:
		if b, ok := b.([]any); ok {
			for i := range min(len(a), len(b)) {
				out = append(out, refChanges(fmt.Sprintf("%s[%d]", path, i), a[i], b[i])...)
			}
		}
	case string:
		if b, ok := b.(string); ok && a != b && strings.HasPrefix(a, Redacted) && strings.HasPrefix(b, Redacted) {
			out = append(out, "credential reference changed at "+path)
		}
	}
	return out
}

func rawList(raw json.RawMessage) []string {
	if raw == nil {
		return nil
	}
	return []string{string(raw)}
}
