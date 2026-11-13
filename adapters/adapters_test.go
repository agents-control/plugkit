package adapters_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"google.golang.org/grpc"

	"github.com/agents-control/plugkit/adapters"
	acplugv1 "github.com/agents-control/plugkit/api/acplug/v1"
	"github.com/agents-control/plugkit/conform"
	"github.com/agents-control/plugkit/discovery"
	"github.com/agents-control/plugkit/host"
	"github.com/agents-control/plugkit/outbox/sinktest"
	"github.com/agents-control/plugkit/serve"
)

var spec = adapters.Spec{
	Engine:    "example-engine",
	EnvPrefix: "EX",
	Ports: map[string]adapters.PortSpec{
		"store":     {Default: []string{"builtin:postgres"}, Builtins: []string{"postgres"}, Memory: true, Plugin: true, Stateful: true, Aliases: []string{"EX_STORE_KIND"}},
		"sink":      {Default: []string{"builtin:log"}, Builtins: []string{"log", "http"}, Memory: true, Plugin: true, Capabilities: map[string][]string{"builtin:http": {"batch"}}},
		"policy":    {Default: []string{"builtin:grants"}, Builtins: []string{"grants", "allow-all"}, Plugin: true, Sensitive: true, Weak: []string{"builtin:allow-all"}},
		"transport": {Default: []string{"builtin:http"}, Builtins: []string{"http", "exec"}, Plugin: true, Multi: true},
	},
}

const sample = `
apiVersion: acplug/v1
kind: Adapters
engine: example-engine
mode: production
ports:
  store: {use: builtin:postgres, config: {databaseUrlRef: env:EX_DATABASE_URL}}
  sink:  {use: plugin:py-sink, config: {}}
  transport: {use: [builtin:http, plugin:py-sink]}
plugins:
  - name: py-sink
    endpoint: 127.0.0.1:9443
    tls: {insecure: true}
    credentialRef: env:EX_SINK_TOKEN
    require: {version: ">=1.0.0", conformance: "sha256:abc", report: /etc/ex/py-sink.json}
`

func load(t *testing.T, data string, env ...string) adapters.File {
	t.Helper()
	var raw []byte
	if data != "" {
		raw = []byte(data)
	}
	f, err := adapters.Load(spec, raw, env)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return f
}

func TestLoadBindsPluginsAndDefaults(t *testing.T) {
	f := load(t, sample)
	if got := f.Ports["policy"].Use; !slices.Equal(got, []string{"builtin:grants"}) {
		t.Fatalf("default policy = %v", got)
	}
	p := f.Plugins[0]
	if !slices.Equal(p.Ports, []string{"sink", "transport"}) {
		t.Fatalf("plugin ports = %v", p.Ports)
	}
	if _, ok := p.Settings["sink"]; !ok {
		t.Fatalf("sink settings not bound: %v", p.Settings)
	}
	if err := adapters.Check(spec, f).Err(); err != nil {
		t.Fatal(err)
	}
}

func TestLoadEnvForm(t *testing.T) {
	f := load(t, "", "EX_STORE_KIND=memory", "EX_TRANSPORT=http, exec", "EX_MODE=dev")
	if !slices.Equal(f.Ports["store"].Use, []string{"memory"}) || !slices.Equal(f.Ports["transport"].Use, []string{"builtin:http", "builtin:exec"}) || f.Mode != "dev" {
		t.Fatalf("env form = %+v", f)
	}
	for name, env := range map[string][]string{
		"file and env":       {"EX_SINK=log"},
		"mode in both":       {"EX_MODE=dev"},
		"alias disagreement": {"EX_STORE=postgres", "EX_STORE_KIND=memory"},
	} {
		data := []byte(sample)
		if name == "alias disagreement" {
			data = nil
		}
		if _, err := adapters.Load(spec, data, env); err == nil {
			t.Errorf("%s: conflict must be an error", name)
		}
	}
	if _, err := adapters.Load(spec, nil, []string{"EX_STORE=postgres", "EX_STORE_KIND=postgres"}); err != nil {
		t.Errorf("agreeing alias: %v", err)
	}
}

func TestParseRefusesUnknownKeys(t *testing.T) {
	if _, err := adapters.Parse([]byte("apiVersion: acplug/v1\nkind: Adapters\nportz: {}\n")); err == nil {
		t.Fatal("unknown key must be an error")
	}
}

func TestCheckFindings(t *testing.T) {
	cases := map[string]struct {
		yaml string
		env  []string
		want string
	}{
		"memory in production": {yaml: "ports: {store: {use: memory}}", want: "memory is refused"},
		"memory allowed":       {yaml: "ports: {store: {use: memory}}", env: []string{"EX_ALLOW_MEMORY=true"}},
		"no memory adapter":    {yaml: "mode: dev\nports: {policy: {use: memory}}", want: "no memory adapter"},
		"unknown builtin":      {yaml: "ports: {sink: {use: builtin:kafka}}", want: "unknown built-in"},
		"unknown port":         {yaml: "ports: {cache: {use: memory}}", want: "unknown port"},
		"multi on single":      {yaml: "ports: {sink: {use: [builtin:log, builtin:http]}}", want: "single-valued"},
		"literal secret":       {yaml: "ports: {sink: {use: builtin:http, config: {url: x, nested: {secret: hunter2}}}}", want: "literal secret"},
		"ref not a ref":        {yaml: "ports: {sink: {use: builtin:http, config: {secretRef: hunter2}}}", want: "must be a reference"},
		"undeclared plugin":    {yaml: "ports: {sink: {use: plugin:nope}}", want: "not declared"},
		"bad use":              {yaml: "ports: {sink: {use: 'plugin:'}}", want: "must be memory"},
		"wrong engine":         {yaml: "engine: other", want: "this binary"},
		"production conformance": {
			yaml: "ports: {sink: {use: plugin:p}}\nplugins: [{name: p, endpoint: 'p:1'}]", want: "production needs require.conformance",
		},
		"plugin port list": {
			yaml: "mode: dev\nports: {sink: {use: plugin:p}}\nplugins: [{name: p, endpoint: 'p:1', ports: [transport]}]", want: "does not list this port",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			doc := tc.yaml
			if !strings.Contains(doc, "engine:") {
				doc = "engine: example-engine\n" + doc
			}
			if !strings.Contains(doc, "mode:") {
				doc = "mode: production\n" + doc
			}
			f := load(t, "apiVersion: acplug/v1\nkind: Adapters\n"+doc, tc.env...)
			err := adapters.Check(spec, f).Err()
			if (err == nil) != (tc.want == "") || (err != nil && !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
	dev := load(t, "apiVersion: acplug/v1\nkind: Adapters\nengine: example-engine\nmode: dev\nports: {sink: {use: plugin:p}}\nplugins: [{name: p, endpoint: '10.0.0.9:1', tls: {insecure: true}}]")
	fs := adapters.Check(spec, dev)
	if fs.Err() != nil || len(fs.Warnings()) != 2 {
		t.Fatalf("dev findings = %v", fs)
	}
}

func TestDiffFlags(t *testing.T) {
	current := discovery.Document{Engine: "example-engine", Mode: "production", Adapters: []discovery.Row{
		{Port: "store", Tier: "builtin", Name: "postgres"},
		{Port: "sink", Tier: "builtin", Name: "log"},
		{Port: "policy", Tier: "builtin", Name: "grants"},
		{Port: "transport", Tier: "builtin", Name: "http"},
		{Port: "transport", Tier: "plugin", Name: "py-sink"},
	}}
	desired := load(t, sample)
	p := adapters.Diff(spec, desired, current)
	if len(p.Changes) != 1 || p.Changes[0].Port != "sink" || p.Weakening() || p.DataMigrationRequired() {
		t.Fatalf("plan = %+v", p)
	}
	risky := load(t, "apiVersion: acplug/v1\nkind: Adapters\nengine: example-engine\nmode: dev\nports:\n  store: {use: memory}\n  policy: {use: builtin:allow-all}\n  transport: {use: [builtin:http, plugin:open]}\nplugins: [{name: open, endpoint: '10.0.0.9:1', tls: {insecure: true}}]")
	p = adapters.Diff(spec, risky, current)
	byPort := map[string]adapters.Change{}
	for _, c := range p.Changes {
		byPort[c.Port] = c
	}
	if c := byPort["mode"]; len(c.Weakening) != 1 {
		t.Errorf("mode change = %+v", c)
	}
	if c := byPort["store"]; !c.DataMigrationRequired {
		t.Errorf("store change = %+v", c)
	}
	if c := byPort["policy"]; len(c.Weakening) != 1 {
		t.Errorf("policy change = %+v", c)
	}
	if c := byPort["transport"]; len(c.Weakening) != 2 {
		t.Errorf("transport change = %+v", c)
	}
	if !p.Weakening() || !p.DataMigrationRequired() {
		t.Fatal("plan must flag weakening and data migration")
	}
}

func TestDialVerifiesConformanceAndDescribes(t *testing.T) {
	addr, stop, err := serve.Start(func(s *grpc.Server) {
		acplugv1.RegisterPluginServer(s, serve.NewPlugin(serve.SinkManifest("ref")))
		acplugv1.RegisterEventSinkServer(s, &serve.MemorySink{})
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	h := &host.Host{}
	report := conform.Run(t.Context(), h, host.Config{Name: "ref", Endpoint: addr, TLS: host.TLS{Insecure: true}}, sinktest.Suite())
	if !report.Passed {
		t.Fatalf("reference failed: %v", report.Failed())
	}
	path := filepath.Join(t.TempDir(), "report.json")
	raw, _ := json.MarshalIndent(report, "", "  ")
	_ = os.WriteFile(path, raw, 0o600)

	doc := func(digest string) string {
		return "apiVersion: acplug/v1\nkind: Adapters\nengine: example-engine\nmode: production\nports: {sink: {use: plugin:ref}}\nplugins: [{name: ref, endpoint: '" + addr + "', tls: {insecure: true}, require: {conformance: '" + digest + "', report: '" + path + "'}}]"
	}
	f := load(t, doc(report.Digest()))
	plugins, err := adapters.Dial(t.Context(), h, f, sinktest.Suite())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = plugins.Close() })
	d := plugins.Describe(spec, f, "abc123")
	var row discovery.Row
	for _, r := range d.Adapters {
		if r.Port == "sink" {
			row = r
		}
	}
	if row.Status != host.StateServing || row.Conformance == nil || !row.Conformance.Passed || row.ManifestDigest == "" || row.Conformance.Suite != sinktest.Version {
		t.Fatalf("row = %+v", row)
	}
	if _, err := adapters.Dial(context.Background(), h, load(t, doc("sha256:forged")), sinktest.Suite()); err == nil {
		t.Fatal("a report digest that does not match must refuse the plugin in production")
	}
	noReport := load(t, doc(report.Digest()))
	noReport.Plugins[0].Require = host.Require{}
	if p, err := adapters.Dial(context.Background(), h, noReport, sinktest.Suite()); err == nil {
		_ = p.Close()
		t.Fatal("production must refuse a plugin without a conformance report even when Check was skipped")
	}
	if _, err := adapters.Dial(context.Background(), h, load(t, doc(report.Digest()))); err == nil {
		t.Fatal("a report the host has no suite to check must refuse the plugin in production")
	}
}

func TestMainCheckAndPlan(t *testing.T) {
	path := filepath.Join(t.TempDir(), "adapters.yaml")
	_ = os.WriteFile(path, []byte(sample), 0o600)
	var out, errOut bytes.Buffer
	if code := adapters.Main(spec, []string{"check", "--file", path}, nil, &out, &errOut); code != 0 {
		t.Fatalf("check = %d: %s %s", code, out.String(), errOut.String())
	}
	srv := httptest.NewServer(discovery.Handler(func() discovery.Document {
		return discovery.Document{Engine: "example-engine", Mode: "production", Adapters: []discovery.Row{{Port: "store", Tier: "memory", Name: "memory"}}}
	}))
	t.Cleanup(srv.Close)
	out.Reset()
	if code := adapters.Main(spec, []string{"plan", "--file", path, "--against", srv.URL}, nil, &out, &errOut); code != 0 {
		t.Fatalf("plan = %d: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "~ store: [memory] -> [builtin:postgres] [data_migration_required]") {
		t.Fatalf("plan output:\n%s", out.String())
	}
	if code := adapters.Main(spec, []string{"check"}, []string{"EX_STORE=memory"}, &out, &errOut); code != 1 {
		t.Fatalf("memory in production must fail the check, got %d", code)
	}
}
