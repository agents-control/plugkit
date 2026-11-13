package host

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	acplugv1 "github.com/agents-control/plugkit/api/acplug/v1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestConfigValidate(t *testing.T) {
	cases := map[string]struct {
		cfg  Config
		want string
	}{
		"ok grpc":          {cfg: Config{Name: "a", Endpoint: "x:1"}},
		"ok http":          {cfg: Config{Name: "a", Endpoint: "http://x:1", Protocol: ProtocolHTTP, TLS: TLS{Insecure: true}}},
		"missing endpoint": {cfg: Config{Name: "a"}, want: "required"},
		"bad protocol":     {cfg: Config{Name: "a", Endpoint: "x", Protocol: "ws"}, want: "grpc or http"},
		"half mtls":        {cfg: Config{Name: "a", Endpoint: "x", TLS: TLS{Cert: "c"}}, want: "go together"},
		"insecure with ca": {cfg: Config{Name: "a", Endpoint: "x", TLS: TLS{Insecure: true, CA: "c"}}, want: "excludes"},
		"http without tls": {cfg: Config{Name: "a", Endpoint: "http://x", Protocol: ProtocolHTTP}, want: "tls.insecure"},
		"https insecure":   {cfg: Config{Name: "a", Endpoint: "https://x", Protocol: ProtocolHTTP, TLS: TLS{Insecure: true}}, want: "TLS settings"},
		"literal token":    {cfg: Config{Name: "a", Endpoint: "x", CredentialRef: "hunter2"}, want: "reference"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if (err == nil) != (tc.want == "") || (err != nil && !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestPlaintext(t *testing.T) {
	for endpoint, want := range map[string]bool{
		"127.0.0.1:9000": false, "localhost:9000": false, "[::1]:9000": false, "http://127.0.0.1:8099": false,
		"unix:///run/p.sock": false, "plugin:9000": true, "10.0.0.4:9000": true, "http://plugin:80": true,
	} {
		if got := (Config{Endpoint: endpoint, TLS: TLS{Insecure: true}}).Plaintext(); got != want {
			t.Errorf("%s: plaintext = %t", endpoint, got)
		}
	}
	if (Config{Endpoint: "plugin:9000"}).Plaintext() {
		t.Error("TLS is never plaintext")
	}
}

func TestRefs(t *testing.T) {
	for s, want := range map[string]bool{"env:X": true, "file:/p": true, "vault:wf/t": true, "hunter2": false, "https://x": false, "env:": false, "Env:X": false} {
		if IsRef(s) != want {
			t.Errorf("IsRef(%q) = %t", s, !want)
		}
	}
	t.Setenv("PLUGKIT_SECRET", "v")
	if v, err := (EnvSecrets{}).Resolve(t.Context(), "env:PLUGKIT_SECRET"); err != nil || v != "v" {
		t.Fatalf("env = %q %v", v, err)
	}
	if _, err := (EnvSecrets{}).Resolve(t.Context(), "env:PLUGKIT_MISSING"); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("missing = %v", err)
	}
	if _, err := (EnvSecrets{}).Resolve(t.Context(), "vault:x"); err == nil {
		t.Fatal("unknown resolver must fail")
	}
}

func TestSatisfies(t *testing.T) {
	for _, tc := range []struct {
		version, constraint string
		ok                  bool
	}{
		{"1.2.3", "", true}, {"1.2.3", ">=1.0.0", true}, {"1.2.3", ">=1.0.0 <1.2.0", false}, {"0.3.0", ">=0.3.0,<1", true},
		{"1.0.0", "=1.0.0", true}, {"1.0.0", "1.0.1", false}, {"dev", ">=1.0.0", false}, {"1.0.0", "~1.0.0", false},
	} {
		if err := satisfies(tc.version, tc.constraint); (err == nil) != tc.ok {
			t.Errorf("satisfies(%q, %q) = %v", tc.version, tc.constraint, err)
		}
	}
}

func TestToValuePrecision(t *testing.T) {
	for raw, ok := range map[string]bool{`{"a":0.1,"b":[1e2,-3]}`: true, `9007199254740992`: true, `9007199254740993`: false, `{"n":12345678901234567890}`: false, `"9007199254740993"`: true} {
		v, err := ToValue(json.RawMessage(raw))
		if (err == nil) != ok {
			t.Errorf("%s: err = %v", raw, err)
		}
		if ok {
			back, _ := FromValue(v)
			var x, y any
			_ = json.Unmarshal([]byte(raw), &x)
			_ = json.Unmarshal(back, &y)
			if a, b := mustJSON(x), mustJSON(y); a != b {
				t.Errorf("round trip %s -> %s", a, b)
			}
		}
	}
	if v, err := ToValue(nil); v != nil || err != nil {
		t.Fatal("empty input is a nil value")
	}
}

func mustJSON(v any) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}

func TestUnavailable(t *testing.T) {
	if !Unavailable(status.Error(codes.Unavailable, "")) || Unavailable(status.Error(codes.InvalidArgument, "")) || Unavailable(errors.New("x")) {
		t.Fatal("classification")
	}
}

func TestStreamLineIsCapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(`{"detail":"` + strings.Repeat("x", 64<<10) + `"}` + "\n"))
	}))
	t.Cleanup(srv.Close)
	conn := newHTTPConn(srv.URL, srv.Client(), "", 1024)
	stream, err := conn.NewStream(t.Context(), &grpc.StreamDesc{ServerStreams: true}, "/acplug.v1.Plugin/Health")
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.SendMsg(&acplugv1.HealthRequest{}); err != nil {
		t.Fatal(err)
	}
	if err := stream.RecvMsg(&acplugv1.HealthResponse{}); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("oversized stream line = %v, want RESOURCE_EXHAUSTED", err)
	}
}

func TestStreamSendDoesNotWaitOnABlockedReceive(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	stream, err := newHTTPConn(srv.URL, srv.Client(), "", 1024).NewStream(t.Context(), &grpc.StreamDesc{ServerStreams: true}, "/x.v1.S/Watch")
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.SendMsg(&acplugv1.HealthRequest{}); err != nil {
		t.Fatal(err)
	}
	go func() { _ = stream.RecvMsg(&acplugv1.HealthResponse{}) }()
	time.Sleep(100 * time.Millisecond)
	done := make(chan error, 1)
	go func() { done <- stream.SendMsg(&acplugv1.HealthRequest{}) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a second request on a server stream must be refused")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SendMsg blocked behind a receive waiting on the plugin")
	}
}
