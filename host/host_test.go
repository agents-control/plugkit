package host_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	acplugv1 "github.com/agents-control/plugkit/api/acplug/v1"
	"github.com/agents-control/plugkit/host"
	"github.com/agents-control/plugkit/serve"
)

func start(t *testing.T, p acplugv1.PluginServer, sink acplugv1.EventSinkServer, opts ...grpc.ServerOption) string {
	t.Helper()
	addr, stop, err := serve.Start(func(s *grpc.Server) {
		acplugv1.RegisterPluginServer(s, p)
		acplugv1.RegisterEventSinkServer(s, sink)
	}, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	return addr
}

func plain(addr string) host.Config {
	return host.Config{Name: "p", Endpoint: addr, TLS: host.TLS{Insecure: true}}
}

func dial(t *testing.T, h *host.Host, cfg host.Config) *host.Client {
	t.Helper()
	c, err := h.Dial(t.Context(), cfg)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func publish(ctx context.Context, cc grpc.ClientConnInterface, n int) error {
	req := &acplugv1.PublishRequest{}
	for i := range n {
		req.Events = append(req.Events, &acplugv1.Event{Id: strings.Repeat("e", i+1)})
	}
	_, err := acplugv1.NewEventSinkClient(cc).Publish(ctx, req)
	return err
}

func TestDialPinsAndEnablesPorts(t *testing.T) {
	c := dial(t, &host.Host{}, plain(start(t, serve.NewPlugin(serve.SinkManifest("ref")), &serve.MemorySink{})))
	if got := c.Ports(); len(got) != 1 || got[0] != "sink" {
		t.Fatalf("ports = %v", got)
	}
	pin := c.Pin()
	if pin.Name != "ref" || pin.Protocol != host.ProtocolID || pin.Digest != host.Digest(c.Manifest()) || !strings.HasPrefix(pin.Digest, "sha256:") {
		t.Fatalf("pin = %+v", pin)
	}
	if _, err := c.Port("transport"); err == nil {
		t.Fatal("undeclared port must not be served")
	}
	cc, _ := c.Port("sink")
	if err := publish(t.Context(), cc, 2); err != nil {
		t.Fatal(err)
	}
	if st := c.Status(); st.State != host.StateServing {
		t.Fatalf("status = %+v", st)
	}
}

func TestHandshakeRefusals(t *testing.T) {
	cases := map[string]struct {
		mutate func(*acplugv1.ManifestResponse)
		cfg    func(*host.Config)
		h      host.Host
		want   string
	}{
		"protocol range":       {mutate: func(m *acplugv1.ManifestResponse) { m.ProtocolVersion = &acplugv1.VersionRange{Min: 2, Max: 2} }, want: "protocol versions"},
		"protocol not spoken":  {h: host.Host{Protocols: []string{"acplug/1+wfplugin/1"}}, want: "speaks"},
		"no schema":            {mutate: func(m *acplugv1.ManifestResponse) { m.Ports[0].ConfigSchema = nil }, want: "config_schema"},
		"no ports":             {mutate: func(m *acplugv1.ManifestResponse) { m.Ports = nil }, want: "no ports"},
		"port not provided":    {cfg: func(c *host.Config) { c.Ports = []string{"journal"} }, want: "not provided"},
		"version constraint":   {cfg: func(c *host.Config) { c.Require.Version = ">=2.0.0" }, want: "does not satisfy"},
		"missing capability":   {cfg: func(c *host.Config) { c.Require.Capabilities = []string{"sink.ordered"} }, want: "required capability"},
		"capability narrowed":  {h: host.Host{Supported: map[string][]string{"sink": {"other"}}}, cfg: func(c *host.Config) { c.Require.Capabilities = []string{"batch"} }, want: "required capability"},
		"config schema":        {cfg: func(c *host.Config) { c.Settings = map[string]any{"sink": "not an object"} }, want: "config_schema"},
		"credential no secret": {cfg: func(c *host.Config) { c.CredentialRef = "env:X" }, want: "secret resolver"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			m := serve.SinkManifest("ref")
			if tc.mutate != nil {
				tc.mutate(m)
			}
			cfg := plain(start(t, serve.NewPlugin(m), &serve.MemorySink{}))
			if tc.cfg != nil {
				tc.cfg(&cfg)
			}
			c, err := tc.h.Dial(t.Context(), cfg)
			if err == nil {
				_ = c.Close()
				t.Fatal("dial must fail")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestRequirementsAccepted(t *testing.T) {
	cfg := plain(start(t, serve.NewPlugin(serve.SinkManifest("ref")), &serve.MemorySink{}))
	cfg.Require = host.Require{Version: ">=1.0.0 <2.0.0", Capabilities: []string{"batch"}}
	cfg.Settings = map[string]any{"sink": map[string]any{"any": 1}}
	cfg.Ports = []string{"sink"}
	c := dial(t, &host.Host{Supported: map[string][]string{"sink": {"batch"}}}, cfg)
	if got := c.Capabilities("sink"); len(got) != 1 || got[0] != "batch" {
		t.Fatalf("capabilities = %v", got)
	}
}

func TestNotServingIsRefused(t *testing.T) {
	p := serve.NewPlugin(serve.SinkManifest("ref"))
	p.SetHealth(acplugv1.HealthResponse_STATUS_NOT_SERVING)
	if c, err := (&host.Host{}).Dial(t.Context(), plain(start(t, p, &serve.MemorySink{}))); err == nil {
		_ = c.Close()
		t.Fatal("a plugin that is not serving must be refused")
	}
}

func TestRecheckDetectsDriftAndHealth(t *testing.T) {
	p := serve.NewPlugin(serve.SinkManifest("ref"))
	c := dial(t, &host.Host{}, plain(start(t, p, &serve.MemorySink{})))
	pinned := c.Pin()
	if st := c.Recheck(t.Context()); st.State != host.StateServing {
		t.Fatalf("status = %+v", st)
	}
	m := serve.SinkManifest("ref")
	m.Version = "1.1.0"
	p.SetManifest(m)
	if st := c.Recheck(t.Context()); st.State != host.StateDegraded || !strings.Contains(st.Detail, "drift") {
		t.Fatalf("drift status = %+v", st)
	}
	if c.Pin() != pinned {
		t.Fatal("drift must keep the pinned contract")
	}
	p.SetHealth(acplugv1.HealthResponse_STATUS_DEGRADED)
	if st := c.Recheck(t.Context()); st.State != host.StateDegraded {
		t.Fatalf("degraded health = %+v", st)
	}
	p.SetHealth(acplugv1.HealthResponse_STATUS_NOT_SERVING)
	if st := c.Recheck(t.Context()); st.State != host.StateNotServing || c.Status() != st {
		t.Fatalf("not serving = %+v", st)
	}
}

type blockingSink struct {
	acplugv1.UnimplementedEventSinkServer
	release chan struct{}
	entered chan struct{}
}

func (b *blockingSink) Publish(ctx context.Context, req *acplugv1.PublishRequest) (*acplugv1.PublishResponse, error) {
	b.entered <- struct{}{}
	select {
	case <-b.release:
	case <-ctx.Done():
	}
	return &acplugv1.PublishResponse{Accepted: int32(len(req.GetEvents()))}, nil
}

func TestBulkheadAndDeadline(t *testing.T) {
	m := serve.SinkManifest("ref")
	m.Ports[0].Limits = &acplugv1.Limits{MaxInFlight: 1, DeadlineMs: 300}
	sink := &blockingSink{release: make(chan struct{}), entered: make(chan struct{}, 4)}
	c := dial(t, &host.Host{MaxInFlight: 8, DefaultDeadline: time.Minute}, plain(start(t, serve.NewPlugin(m), sink)))
	cc, _ := c.Port("sink")
	var wg sync.WaitGroup
	var first error
	wg.Go(func() { first = publish(context.Background(), cc, 1) })
	<-sink.entered
	if err := publish(t.Context(), cc, 1); status.Code(err) != codes.ResourceExhausted || !host.Unavailable(err) {
		t.Fatalf("second call = %v, want RESOURCE_EXHAUSTED from the bulkhead", err)
	}
	wg.Wait()
	if status.Code(first) != codes.DeadlineExceeded {
		t.Fatalf("first call = %v, want DEADLINE_EXCEEDED from the plugin's 300 ms limit", first)
	}
	close(sink.release)
}

func TestMessageSizeLimit(t *testing.T) {
	m := serve.SinkManifest("ref")
	m.Ports[0].Limits = &acplugv1.Limits{MaxMessageBytes: 64}
	c := dial(t, &host.Host{}, plain(start(t, serve.NewPlugin(m), &serve.MemorySink{})))
	cc, _ := c.Port("sink")
	if err := publish(t.Context(), cc, 20); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("oversized request = %v", err)
	}
}

type secrets map[string]string

func (s secrets) Resolve(_ context.Context, ref string) (string, error) {
	if v, ok := s[ref]; ok {
		return v, nil
	}
	return "", host.ErrSecretNotFound
}

func TestBearerFromSecretReference(t *testing.T) {
	addr := start(t, serve.NewPlugin(serve.SinkManifest("ref")), &serve.MemorySink{}, serve.Bearer("s3cret")...)
	cfg := plain(addr)
	cfg.CredentialRef = "vault:plugins/ref"
	dial(t, &host.Host{Secrets: secrets{"vault:plugins/ref": "s3cret"}}, cfg)
	c, err := (&host.Host{Secrets: secrets{"vault:plugins/ref": "wrong"}}).Dial(t.Context(), cfg)
	if err == nil {
		_ = c.Close()
		t.Fatal("a wrong token must be refused")
	}
	if !strings.Contains(err.Error(), "Unauthenticated") {
		t.Fatalf("err = %v", err)
	}
}

type pki struct {
	ca     *x509.Certificate
	caKey  *ecdsa.PrivateKey
	dir    string
	serial int64
}

func newPKI(t *testing.T) (*pki, string) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-ca"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(der)
	p := &pki{ca: ca, caKey: key, dir: t.TempDir(), serial: 1}
	caFile := filepath.Join(p.dir, "ca.pem")
	_ = os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	return p, caFile
}

func (p *pki) issue(t *testing.T, name string, usage x509.ExtKeyUsage) (certFile, keyFile string) {
	t.Helper()
	p.serial++
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(p.serial), Subject: pkix.Name{CommonName: name}, DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{usage}, KeyUsage: x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, tpl, p.ca, &key.PublicKey, p.caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	certFile, keyFile = filepath.Join(p.dir, name+".pem"), filepath.Join(p.dir, name+".key")
	_ = os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	_ = os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600)
	return certFile, keyFile
}

func TestMutualTLS(t *testing.T) {
	p, caFile := newPKI(t)
	sCert, sKey := p.issue(t, "server", x509.ExtKeyUsageServerAuth)
	pair, err := tls.LoadX509KeyPair(sCert, sKey)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(p.ca)
	creds := credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{pair}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS12})
	addr := start(t, serve.NewPlugin(serve.SinkManifest("ref")), &serve.MemorySink{}, grpc.Creds(creds))

	cCert, cKey := p.issue(t, "client", x509.ExtKeyUsageClientAuth)
	dial(t, &host.Host{}, host.Config{Name: "m", Endpoint: addr, TLS: host.TLS{CA: caFile, Cert: cCert, Key: cKey, ServerName: "localhost"}})

	if c, err := (&host.Host{}).Dial(t.Context(), host.Config{Name: "m", Endpoint: addr, TLS: host.TLS{CA: caFile, ServerName: "localhost"}}); err == nil {
		_ = c.Close()
		t.Fatal("a client without a certificate must be refused")
	}
	if c, err := (&host.Host{}).Dial(t.Context(), plain(addr)); err == nil {
		_ = c.Close()
		t.Fatal("plaintext to a TLS plugin must fail")
	}
}

func legacyServer(t *testing.T) string {
	t.Helper()
	schema, _ := structpb.NewValue(true)
	unary := func(name string, reply func() any) grpc.MethodDesc {
		return grpc.MethodDesc{MethodName: name, Handler: func(_ any, _ context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
			if err := dec(&acplugv1.HealthRequest{}); err != nil {
				return nil, err
			}
			return reply(), nil
		}}
	}
	desc := grpc.ServiceDesc{ServiceName: "legacy.v1.Plugin", HandlerType: (*any)(nil), Methods: []grpc.MethodDesc{
		unary("Manifest", func() any {
			return &acplugv1.ManifestResponse{Protocol: "legacy/1", ProtocolVersion: &acplugv1.VersionRange{Min: 1, Max: 1}, Name: "old", Version: "0.9.0", Ports: []*acplugv1.ProvidedPort{{Port: "sink", ConfigSchema: schema}}}
		}),
		unary("Health", func() any { return &acplugv1.HealthResponse{Status: acplugv1.HealthResponse_STATUS_SERVING} }),
	}}
	addr, stop, err := serve.Start(func(s *grpc.Server) { s.RegisterService(&desc, struct{}{}) })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	return addr
}

func TestLegacyManifestAccepted(t *testing.T) {
	addr := legacyServer(t)
	legacy := host.Legacy{Protocol: "legacy/1", Service: "legacy.v1.Plugin", Manifest: func(ctx context.Context, cc grpc.ClientConnInterface) (*acplugv1.ManifestResponse, error) {
		m := &acplugv1.ManifestResponse{}
		return m, cc.Invoke(ctx, "/legacy.v1.Plugin/Manifest", &acplugv1.HealthRequest{}, m)
	}}
	c := dial(t, &host.Host{Legacy: []host.Legacy{legacy}}, plain(addr))
	if !c.Legacy() || c.Pin().Protocol != "legacy/1" {
		t.Fatalf("legacy = %t pin = %+v", c.Legacy(), c.Pin())
	}
	if st := c.Recheck(t.Context()); st.State != host.StateServing {
		t.Fatalf("legacy health = %+v", st)
	}
	if c, err := (&host.Host{}).Dial(t.Context(), plain(addr)); err == nil {
		_ = c.Close()
		t.Fatal("without the legacy hook the old protocol must be refused")
	}
}

func TestHTTPMapping(t *testing.T) {
	var gotDeadline, gotAuth string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotDeadline, gotAuth = r.Header.Get(host.DeadlineHeader), r.Header.Get("Authorization")
		mu.Unlock()
		switch r.URL.Path {
		case "/acplug.v1.Plugin/Manifest":
			_, _ = w.Write([]byte(`{"protocol":"acplug/1","protocol_version":{"min":1,"max":1},"name":"h","version":"1.0.0","ports":[{"port":"sink","config_schema":true,"unknown_field":1}]}`))
		case "/acplug.v1.Plugin/Health":
			_, _ = w.Write([]byte(`{"status":"STATUS_SERVING"}`))
		case "/acplug.v1.EventSink/Publish":
			var req map[string][]any
			_ = json.NewDecoder(r.Body).Decode(&req)
			if len(req["events"]) > 2 {
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"error":{"code":"RESOURCE_EXHAUSTED","message":"batch too large"}}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]int{"accepted": len(req["events"])})
		default:
			http.Error(w, `{"error":{"code":"UNIMPLEMENTED","message":"no"}}`, http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("PLUGKIT_TEST_TOKEN", "tok")
	cfg := host.Config{Name: "h", Endpoint: srv.URL, Protocol: host.ProtocolHTTP, TLS: host.TLS{Insecure: true}, CredentialRef: "env:PLUGKIT_TEST_TOKEN"}
	c := dial(t, &host.Host{Secrets: host.EnvSecrets{}}, cfg)
	cc, _ := c.Port("sink")
	if err := publish(t.Context(), cc, 2); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if gotDeadline == "" || gotAuth != "Bearer tok" {
		t.Fatalf("deadline header %q, auth %q", gotDeadline, gotAuth)
	}
	mu.Unlock()
	if err := publish(t.Context(), cc, 3); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("mapped error = %v", err)
	}
}
