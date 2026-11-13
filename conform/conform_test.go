package conform_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	if _, err := conform.VerifyReport(path, r.Digest(), pin); err != nil {
		t.Fatalf("verify: %v", err)
	}
	for name, tc := range map[string]struct {
		digest string
		pin    host.Pin
		want   string
	}{
		"forged digest":  {digest: "sha256:00", pin: pin, want: "differs"},
		"other manifest": {digest: r.Digest(), pin: host.Pin{Digest: "sha256:other"}, want: "is for manifest"},
	} {
		if _, err := conform.VerifyReport(path, tc.digest, tc.pin); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	r.Passed = false
	raw, _ = json.Marshal(r)
	_ = os.WriteFile(path, raw, 0o600)
	if _, err := conform.VerifyReport(path, r.Digest(), pin); err == nil || !strings.Contains(err.Error(), "failed") {
		t.Errorf("failed report: %v", err)
	}
}
