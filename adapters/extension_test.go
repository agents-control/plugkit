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
	want := `{"sources":[{"accept":["test.suite.requested"],"name":"ci","secretRef":"redacted","trust":"reported"}]}`
	if got := string(d.Extensions["inbox"]); got != want {
		t.Fatalf("discovery = %s, want %s", got, want)
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
