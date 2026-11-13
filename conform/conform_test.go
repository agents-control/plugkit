package conform_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	acplugv1 "github.com/agents-control/plugkit/api/acplug/v1"
	"github.com/agents-control/plugkit/conform"
	"github.com/agents-control/plugkit/host"
	"github.com/agents-control/plugkit/outbox/sinktest"
	"github.com/agents-control/plugkit/serve"
)

func TestReportRoundTripAndVerify(t *testing.T) {
	addr, stop, err := serve.Start(func(s *grpc.Server) {
		acplugv1.RegisterPluginServer(s, serve.NewPlugin(serve.SinkManifest("ref")))
		acplugv1.RegisterEventSinkServer(s, &serve.MemorySink{})
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	r := conform.Run(t.Context(), &host.Host{}, host.Config{Name: "ref", Endpoint: addr, TLS: host.TLS{Insecure: true}}, sinktest.Suite())
	if !r.Passed || r.Suites["sink"] != sinktest.Version || r.Latency.P99 < r.Latency.P50 || r.ManifestDigest == "" {
		t.Fatalf("report = %+v", r)
	}
	path := filepath.Join(t.TempDir(), "r.json")
	raw, _ := json.MarshalIndent(r, "", "  ")
	_ = os.WriteFile(path, raw, 0o600)
	pin := host.Pin{Digest: r.ManifestDigest}
	sink := []string{"sink"}
	suites := []conform.Suite{sinktest.Suite()}
	if _, err := conform.VerifyReport(path, r.Digest(), pin, sink, suites); err != nil {
		t.Fatalf("verify: %v", err)
	}
	other := sinktest.Suite()
	other.Version = "sinktest@0"
	for name, tc := range map[string]struct {
		ports  []string
		suites []conform.Suite
		want   string
	}{
		"uncovered port": {ports: []string{"sink", "transport"}, suites: suites, want: "transport"},
		"other version":  {ports: sink, suites: []conform.Suite{other}, want: "sinktest@0"},
		"no host suite":  {ports: sink, want: "no suite"},
	} {
		if _, err := conform.VerifyReport(path, r.Digest(), pin, tc.ports, tc.suites); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	for name, tc := range map[string]struct {
		digest string
		pin    host.Pin
		want   string
	}{
		"forged digest":  {digest: "sha256:00", pin: pin, want: "differs"},
		"other manifest": {digest: r.Digest(), pin: host.Pin{Digest: "sha256:other"}, want: "is for manifest"},
	} {
		if _, err := conform.VerifyReport(path, tc.digest, tc.pin, sink, suites); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	r.Cases[len(r.Cases)-1].Passed = false
	raw, _ = json.Marshal(r)
	_ = os.WriteFile(path, raw, 0o600)
	if _, err := conform.VerifyReport(path, r.Digest(), pin, sink, suites); err == nil || !strings.Contains(err.Error(), "failed") {
		t.Errorf("report claiming a pass with a failing case: %v", err)
	}
}

func caseSuite(cases ...conform.Case) conform.Suite {
	return conform.Suite{Port: "sink", Version: "t@1", Cases: func(context.Context, grpc.ClientConnInterface) []conform.Case { return cases }}
}

func refConfig(t *testing.T) host.Config {
	t.Helper()
	addr, stop, err := serve.Start(func(s *grpc.Server) {
		acplugv1.RegisterPluginServer(s, serve.NewPlugin(serve.SinkManifest("ref")))
		acplugv1.RegisterEventSinkServer(s, &serve.MemorySink{})
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	return host.Config{Name: "ref", Endpoint: addr, TLS: host.TLS{Insecure: true}}
}

func TestSkippedCases(t *testing.T) {
	cfg := refConfig(t)
	skip := func(context.Context) error { return conform.Skip("needs feature x") }
	req := conform.Run(t.Context(), &host.Host{}, cfg, caseSuite(conform.Case{Name: "needs x", Run: skip}))
	opt := conform.Run(t.Context(), &host.Host{}, cfg, caseSuite(conform.Case{Name: "needs x", Optional: true, Run: skip}))
	if req.Passed || !slices.Equal(req.Failed(), []string{"needs x"}) || !slices.Equal(req.Skipped(), []string{"needs x"}) {
		t.Fatalf("required skip: passed=%t failed=%v skipped=%v", req.Passed, req.Failed(), req.Skipped())
	}
	if !opt.Passed || len(opt.Failed()) != 0 || !slices.Equal(opt.Skipped(), []string{"needs x"}) {
		t.Fatalf("optional skip: passed=%t failed=%v skipped=%v", opt.Passed, opt.Failed(), opt.Skipped())
	}
	last := opt.Cases[len(opt.Cases)-1]
	if !last.Skipped || last.Passed || last.Reason != "needs feature x" || last.Error != "" {
		t.Fatalf("skipped result = %+v", last)
	}
	for name, tc := range map[string]struct {
		r   conform.Report
		err bool
	}{"required": {req, true}, "optional": {opt, false}} {
		path := filepath.Join(t.TempDir(), "r.json")
		raw, _ := json.Marshal(tc.r)
		_ = os.WriteFile(path, raw, 0o600)
		_, err := conform.VerifyReport(path, tc.r.Digest(), host.Pin{Digest: tc.r.ManifestDigest}, []string{"sink"}, []conform.Suite{caseSuite()})
		if (err != nil) != tc.err {
			t.Errorf("%s skip: VerifyReport err = %v", name, err)
		}
	}
}

func TestRunnerContextReachesSuite(t *testing.T) {
	cfg := refConfig(t)
	type key struct{}
	ctx, cancel := context.WithTimeout(context.WithValue(t.Context(), key{}, "run"), 150*time.Millisecond)
	defer cancel()
	var built any
	suite := conform.Suite{Port: "sink", Version: "t@1", Cases: func(ctx context.Context, _ grpc.ClientConnInterface) []conform.Case {
		built = ctx.Value(key{})
		return []conform.Case{
			{Name: "runner deadline", Run: func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }},
			{Name: "case timeout", Timeout: 20 * time.Millisecond, Run: func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }},
		}
	}}
	start := time.Now()
	r := conform.Run(ctx, &host.Host{}, cfg, suite)
	if built != "run" || !slices.Contains(r.Failed(), "runner deadline") || !slices.Contains(r.Failed(), "case timeout") || time.Since(start) > 2*time.Second {
		t.Fatalf("suite ctx = %v failed = %v after %v", built, r.Failed(), time.Since(start))
	}
}

func TestCLIPrintsSkipWithReason(t *testing.T) {
	cfg := refConfig(t)
	cli := conform.CLI{Suites: []conform.Suite{caseSuite(conform.Case{Name: "needs x", Optional: true, Run: func(context.Context) error { return conform.Skip("needs feature x") }})}}
	var out, errb strings.Builder
	if code := cli.Main([]string{"-endpoint", cfg.Endpoint, "-insecure", "-timeout", "20s"}, &out, &errb); code != 0 || !strings.Contains(out.String(), "SKIP sink/needs x") || !strings.Contains(out.String(), "needs feature x") {
		t.Fatalf("code=%d out=%s err=%s", code, out.String(), errb.String())
	}
}
