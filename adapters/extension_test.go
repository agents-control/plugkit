package adapters_test

import (
	"encoding/json"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/agents-control/plugkit/adapters"
	"github.com/agents-control/plugkit/discovery"
)

type inboxSource struct {
	Name      string   `json:"name"`
	SecretRef string   `json:"secretRef"`
	Accept    []string `json:"accept"`
	Trust     string   `json:"trust"`
}

type inbox struct {
	Sources []inboxSource `json:"sources"`
}

func extSpec() adapters.Spec {
	s := spec
	s.Extensions = map[string]adapters.Extension{"inbox": {
		Schema: json.RawMessage(`{
			"type": "object", "additionalProperties": false, "required": ["sources"],
			"properties": {"sources": {"type": "array", "items": {
				"type": "object", "additionalProperties": false, "required": ["name", "secretRef", "accept", "trust"],
				"properties": {
					"name": {"type": "string"}, "secretRef": {"type": "string"}, "tenant": {"type": "string"},
					"accept": {"type": "array", "minItems": 1, "items": {"type": "string"}},
					"trust": {"enum": ["dispatched", "reported"]}}}}}}`),
		Discover: func(raw json.RawMessage) any {
			var in inbox
			_ = json.Unmarshal(raw, &in)
			return in
		},
		Weakening: func(from, to json.RawMessage) []string {
			var before, after inbox
			_ = json.Unmarshal(from, &before)
			_ = json.Unmarshal(to, &after)
			var out []string
			for _, s := range after.Sources {
				if s.Trust == "dispatched" && !slices.ContainsFunc(before.Sources, func(b inboxSource) bool { return b.Name == s.Name && b.Trust == s.Trust }) {
					out = append(out, "source "+s.Name+" trusted as dispatched")
				}
			}
			return out
		},
	}}
	return s
}

const extHead = "apiVersion: acplug/v1\nkind: Adapters\nengine: example-engine\nmode: production\n"

const ciSource = "  - {name: ci, secretRef: env:CI_SECRET, tenant: t1, accept: [test.suite.requested], trust: reported}\n"

const busSource = "  - {name: bus, secretRef: env:BUS_SECRET, accept: [test.attempt.*], trust: dispatched}\n"

func TestExtensionSchemaAndSecrets(t *testing.T) {
	s := extSpec()
	cases := map[string]struct{ body, want string }{
		"schema rejection":   {"extensions: {inbox: {sources: [{name: ci, secretRef: env:X, accept: [], trust: attested}]}}\n", "extensions.inbox"},
		"literal secret ref": {"extensions: {inbox: {sources: [{name: ci, secretRef: hunter2, accept: [a], trust: reported}]}}\n", "inbox.sources[0].secretRef must be a reference"},
		"unknown extension":  {"extensions: {outbox: {}}\n", "extensions.outbox: unknown extension; example-engine accepts [inbox]"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f, err := adapters.Load(s, []byte(extHead+tc.body), nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := adapters.Check(s, f).Err(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
	f := load(t, extHead+"extensions:\n inbox:\n  sources:\n"+ciSource)
	if err := adapters.Check(s, f).Err(); err != nil {
		t.Fatalf("valid extension refused: %v", err)
	}
	var got inbox
	if err := f.Extension("inbox", &got); err != nil || len(got.Sources) != 1 || got.Sources[0].SecretRef != "env:CI_SECRET" {
		t.Fatalf("decode = %+v %v", got, err)
	}
}

func TestExtensionDiscoveryIsProjectedAndRedacted(t *testing.T) {
	s := extSpec()
	f, err := adapters.Load(s, []byte(extHead+"extensions:\n inbox:\n  sources:\n"+ciSource), nil)
	if err != nil {
		t.Fatal(err)
	}
	d := (&adapters.Plugins{}).Describe(s, f, "b")
	got := string(d.Extensions["inbox"])
	if !strings.HasPrefix(got, `{"sources":[{"accept":["test.suite.requested"],"name":"ci","secretRef":"redacted:`) || strings.Contains(got, "CI_SECRET") || strings.Contains(got, "t1") {
		t.Fatalf("discovery = %s", got)
	}
}

func TestExtensionDiscoveryRefusesExposedSecrets(t *testing.T) {
	s := spec
	s.Extensions = map[string]adapters.Extension{"raw": {
		Schema:    json.RawMessage(`{}`),
		Discover:  func(raw json.RawMessage) any { return raw },
		Weakening: func(_, _ json.RawMessage) []string { return nil },
	}}
	cases := map[string]string{
		"nested password":      "auth: {pass: hunter2}",
		"dsn with password":    "conn: 'host=db password=hunter2'",
		"signing key":          "signingKey: c2VjcmV0",
		"authorization header": "headers: {Authorization: 'Bearer abc'}",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			f, err := adapters.Load(s, []byte(extHead+"extensions: {raw: {"+body+"}}\n"), nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := adapters.Check(s, f).Err(); err == nil || !strings.Contains(err.Error(), "exposes") {
				t.Fatalf("check = %v", err)
			}
			if d := (&adapters.Plugins{}).Describe(s, f, "b"); d.Extensions != nil {
				t.Fatalf("discovery shows %s", d.Extensions["raw"])
			}
		})
	}
}

func TestExtensionSpecIsCheckedAtBoot(t *testing.T) {
	s := spec
	s.Extensions = map[string]adapters.Extension{
		"bad":     {Schema: json.RawMessage(`{"type": 7}`), Discover: func(r json.RawMessage) any { return r }, Weakening: func(_, _ json.RawMessage) []string { return nil }},
		"partial": {Schema: json.RawMessage(`{}`)},
	}
	err := adapters.Check(s, load(t, extHead)).Err()
	if err == nil || !strings.Contains(err.Error(), "spec extensions.bad: invalid schema") || !strings.Contains(err.Error(), "spec extensions.partial: an extension needs Discover and Weakening") {
		t.Fatalf("check = %v", err)
	}
}

func TestExtensionPlanNotesRotatedReference(t *testing.T) {
	s := extSpec()
	running := load(t, extHead+"extensions:\n inbox:\n  sources:\n"+ciSource)
	current := (&adapters.Plugins{}).Describe(s, running, "b")
	desired := load(t, extHead+"extensions:\n inbox:\n  sources:\n"+strings.Replace(ciSource, "CI_SECRET", "CI_SECRET_V2", 1))
	p := adapters.Diff(s, desired, current)
	i := slices.IndexFunc(p.Changes, func(c adapters.Change) bool { return c.Port == "extensions.inbox" })
	if i < 0 || p.Weakening() || !slices.Equal(p.Changes[i].Notes, []string{"credential reference changed at sources[0].secretRef"}) {
		t.Fatalf("plan = %+v", p)
	}
	if out, _ := json.Marshal(p); strings.Contains(string(out), "CI_SECRET") {
		t.Fatalf("plan reveals the reference: %s", out)
	}
}

func TestExtensionPlanFlagsWeakening(t *testing.T) {
	s := extSpec()
	running, err := adapters.Load(s, []byte(extHead+"extensions:\n inbox:\n  sources:\n"+ciSource), nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(discovery.Handler(func() discovery.Document { return (&adapters.Plugins{}).Describe(s, running, "b") }))
	t.Cleanup(srv.Close)
	current, err := discovery.Fetch(t.Context(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range adapters.Diff(s, running, current).Changes {
		if strings.HasPrefix(c.Port, "extensions.") {
			t.Fatalf("unchanged extension planned as %+v", c)
		}
	}
	desired, err := adapters.Load(s, []byte(extHead+"extensions:\n inbox:\n  sources:\n"+ciSource+busSource), nil)
	if err != nil {
		t.Fatal(err)
	}
	p := adapters.Diff(s, desired, current)
	i := slices.IndexFunc(p.Changes, func(c adapters.Change) bool { return c.Port == "extensions.inbox" })
	if i < 0 || !p.Weakening() || !slices.Equal(p.Changes[i].Weakening, []string{"source bus trusted as dispatched"}) || len(p.Changes[i].From) != 1 || strings.Contains(p.Changes[i].To[0], "BUS_SECRET") {
		t.Fatalf("plan = %+v", p)
	}
}

func TestExtensionEnvAlias(t *testing.T) {
	s := extSpec()
	value := `{"sources":[{"name":"ci","secretRef":"env:CI_SECRET","accept":["a"],"trust":"reported"}]}`
	if _, err := adapters.Load(s, nil, []string{"EX_EXTENSION_INBOX=" + value}); err == nil || !strings.Contains(err.Error(), "reviewable") {
		t.Fatalf("env alias in production: %v", err)
	}
	f, err := adapters.Load(s, nil, []string{"EX_MODE=dev", "EX_EXTENSION_INBOX=" + value})
	if err != nil || adapters.Check(s, f).Err() != nil {
		t.Fatalf("env alias in dev: %+v %v", f, err)
	}
	devFile := strings.Replace(extHead, "production", "dev", 1) + "extensions: {inbox: {sources: []}}\n"
	if _, err := adapters.Load(s, []byte(devFile), []string{"EX_EXTENSION_INBOX=" + value}); err == nil || !strings.Contains(err.Error(), "set in the adapters file and by EX_EXTENSION_INBOX") {
		t.Fatalf("file and env: %v", err)
	}
	if !slices.Contains(adapters.EnvNames(s), "EX_EXTENSION_INBOX") {
		t.Fatal("EnvNames lacks EX_EXTENSION_INBOX")
	}
}
