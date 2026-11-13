package host

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	acplugv1 "github.com/agents-control/plugkit/api/acplug/v1"
)

// ProtocolID is the common protocol family; engines speak it as "acplug/1+<engine protocol>".
const ProtocolID = "acplug/1"

// ProtocolVersion is the acplug wire version this host speaks; a plugin's declared range must contain it.
const ProtocolVersion = 1

const handshakeTimeout = 10 * time.Second

const healthMethod = "/acplug.v1.Plugin/Health"

// Legacy accepts a pre-acplug plugin protocol during its deprecation window (UP-O1).
// Manifest calls the legacy service and converts its reply; Health must be wire-compatible with acplug's.
type Legacy struct {
	Protocol string
	Service  string
	Manifest func(ctx context.Context, conn grpc.ClientConnInterface) (*acplugv1.ManifestResponse, error)
}

// Host is the engine side of acplug/1. Zero values default per port to acplug/1, a 10 s unary deadline, 64 unary in flight,
// 1024 streams in their own pool, 60 s stream idle (negative disables), 4 MiB; per-tenant caps belong to the engine.
type Host struct {
	Engine            string
	Version           string
	Protocols         []string
	Secrets           SecretResolver
	Supported         map[string][]string
	Deadlines         map[string]time.Duration
	DefaultDeadline   time.Duration
	StreamIdle        map[string]time.Duration
	DefaultStreamIdle time.Duration
	StreamCaps        map[string]int
	DefaultStreamCap  int
	MaxInFlight       int
	MaxMessageBytes   int
	Legacy            []Legacy
}

// Dial connects to the plugin, negotiates the manifest, validates the port settings, requires SERVING and pins the manifest.
func (h *Host) Dial(ctx context.Context, cfg Config) (*Client, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	token, err := h.bearer(ctx, cfg)
	if err != nil {
		return nil, err
	}
	tlsCfg, err := tlsConfig(cfg.TLS)
	if err != nil {
		return nil, fmt.Errorf("plugin %s: %w", cfg.Name, err)
	}
	c := &Client{cfg: cfg, health: healthMethod}
	switch cfg.Protocol {
	case ProtocolHTTP:
		hc := &http.Client{
			Transport:     &http.Transport{TLSClientConfig: tlsCfg, MaxIdleConnsPerHost: 8},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
		c.conn = newHTTPConn(cfg.Endpoint, hc, token, int64(h.maxBytes()))
		c.close = func() error { hc.CloseIdleConnections(); return nil }
	default:
		cc, err := dialGRPC(cfg, tlsCfg, token, h.maxBytes())
		if err != nil {
			return nil, fmt.Errorf("plugin %s: %w", cfg.Name, err)
		}
		c.conn, c.close = cc, cc.Close
	}
	if err := h.handshake(ctx, c); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("plugin %s: %w", cfg.Name, err)
	}
	return c, nil
}

func (h *Host) handshake(ctx context.Context, c *Client) error {
	ctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	m, err := h.manifest(ctx, c)
	if err != nil {
		return err
	}
	if err := h.accept(m, c.cfg); err != nil {
		return err
	}
	c.manifest = m
	c.pin = Pin{Name: m.GetName(), Version: m.GetVersion(), Protocol: m.GetProtocol(), Digest: Digest(m)}
	c.ports = map[string]*enabledPort{}
	for _, p := range m.GetPorts() {
		if len(c.cfg.Ports) > 0 && !slices.Contains(c.cfg.Ports, p.GetPort()) {
			continue
		}
		if err := validateSettings(p, c.cfg.Settings[p.GetPort()]); err != nil {
			return err
		}
		c.ports[p.GetPort()] = &enabledPort{conn: h.guard(c.conn, p), capabilities: h.narrow(p), metadata: p.GetMetadata()}
	}
	if err := h.requireCapabilities(c); err != nil {
		return err
	}
	if err := c.Health(ctx); err != nil {
		return err
	}
	c.status = Status{State: StateServing}
	return nil
}

func (h *Host) manifest(ctx context.Context, c *Client) (*acplugv1.ManifestResponse, error) {
	c.request = &acplugv1.ManifestRequest{HostProtocols: h.protocols(), HostEngine: h.Engine, HostVersion: h.Version}
	m, err := acplugv1.NewPluginClient(c.conn).Manifest(ctx, c.request)
	if status.Code(err) != codes.Unimplemented {
		if err != nil {
			return nil, fmt.Errorf("manifest: %w", err)
		}
		return m, nil
	}
	for _, l := range h.Legacy {
		m, lerr := l.Manifest(ctx, c.conn)
		if status.Code(lerr) == codes.Unimplemented {
			continue
		}
		if lerr != nil {
			return nil, fmt.Errorf("legacy %s manifest: %w", l.Protocol, lerr)
		}
		if m.GetProtocol() != l.Protocol {
			return nil, fmt.Errorf("legacy plugin speaks %q, expected %q", m.GetProtocol(), l.Protocol)
		}
		c.health, c.legacy = "/"+l.Service+"/Health", true
		return m, nil
	}
	return nil, fmt.Errorf("manifest: %w", err)
}

func (h *Host) accept(m *acplugv1.ManifestResponse, cfg Config) error {
	accepted := h.protocols()
	for _, l := range h.Legacy {
		accepted = append(accepted, l.Protocol)
	}
	if !slices.Contains(accepted, m.GetProtocol()) {
		return fmt.Errorf("plugin speaks %q, host speaks %s", m.GetProtocol(), strings.Join(accepted, ", "))
	}
	if m.GetName() == "" || m.GetVersion() == "" {
		return errors.New("manifest needs a name and a version")
	}
	r := m.GetProtocolVersion()
	if r == nil || r.GetMin() < 1 || r.GetMax() < r.GetMin() {
		return errors.New("manifest declares no valid protocol_version range")
	}
	if ProtocolVersion < r.GetMin() || ProtocolVersion > r.GetMax() {
		return fmt.Errorf("plugin supports protocol versions [%d, %d], host speaks %d", r.GetMin(), r.GetMax(), ProtocolVersion)
	}
	if len(m.GetPorts()) == 0 {
		return errors.New("manifest declares no ports")
	}
	declared := map[string]bool{}
	for _, p := range m.GetPorts() {
		if p.GetPort() == "" {
			return errors.New("manifest declares a port without a name")
		}
		if !isSchema(p.GetConfigSchema()) {
			return fmt.Errorf("port %q declares no config_schema (a JSON Schema object or boolean)", p.GetPort())
		}
		declared[p.GetPort()] = true
	}
	for _, want := range cfg.Ports {
		if !declared[want] {
			return fmt.Errorf("configured port %q is not provided by the plugin", want)
		}
	}
	return satisfies(m.GetVersion(), cfg.Require.Version)
}

func (h *Host) narrow(p *acplugv1.ProvidedPort) []string {
	supported, ok := h.Supported[p.GetPort()]
	if !ok {
		return slices.Clone(p.GetCapabilities())
	}
	var out []string
	for _, c := range p.GetCapabilities() {
		if slices.Contains(supported, capabilityKey(c)) {
			out = append(out, c)
		}
	}
	return out
}

func (h *Host) requireCapabilities(c *Client) error {
	for _, want := range c.cfg.Require.Capabilities {
		found := false
		for _, p := range c.ports {
			for _, have := range p.capabilities {
				if have == want || (!strings.Contains(want, "=") && capabilityKey(have) == want) {
					found = true
				}
			}
		}
		if !found {
			return fmt.Errorf("required capability %q is not offered by the enabled ports", want)
		}
	}
	return nil
}

func (h *Host) guard(cc grpc.ClientConnInterface, p *acplugv1.ProvidedPort) *portConn {
	deadline := h.DefaultDeadline
	if d, ok := h.Deadlines[p.GetPort()]; ok {
		deadline = d
	}
	if deadline <= 0 {
		deadline = 10 * time.Second
	}
	inFlight, maxBytes := h.MaxInFlight, h.maxBytes()
	if inFlight <= 0 {
		inFlight = 64
	}
	l := p.GetLimits()
	if d := time.Duration(l.GetDeadlineMs()) * time.Millisecond; d > 0 && d < deadline {
		deadline = d
	}
	if n := int(l.GetMaxInFlight()); n > 0 && n < inFlight {
		inFlight = n
	}
	if n := int(l.GetMaxMessageBytes()); n > 0 && n < maxBytes {
		maxBytes = n
	}
	idle := h.DefaultStreamIdle
	if d, ok := h.StreamIdle[p.GetPort()]; ok {
		idle = d
	}
	if idle == 0 {
		idle = time.Minute
	}
	streams := h.DefaultStreamCap
	if n, ok := h.StreamCaps[p.GetPort()]; ok {
		streams = n
	}
	if streams <= 0 {
		streams = 1024
	}
	return &portConn{cc: cc, deadline: deadline, idle: idle, slots: make(chan struct{}, inFlight), streams: make(chan struct{}, streams), maxBytes: maxBytes}
}

func (h *Host) protocols() []string {
	if len(h.Protocols) == 0 {
		return []string{ProtocolID}
	}
	return slices.Clone(h.Protocols)
}

func (h *Host) maxBytes() int {
	if h.MaxMessageBytes > 0 {
		return h.MaxMessageBytes
	}
	return 4 << 20
}

func (h *Host) bearer(ctx context.Context, cfg Config) (string, error) {
	if cfg.CredentialRef == "" {
		return "", nil
	}
	if h.Secrets == nil {
		return "", fmt.Errorf("plugin %s: credentialRef needs a secret resolver", cfg.Name)
	}
	token, err := h.Secrets.Resolve(ctx, cfg.CredentialRef)
	if err != nil {
		return "", fmt.Errorf("plugin %s: resolve credential: %w", cfg.Name, err)
	}
	return token, nil
}

// Digest is the pinned identity of a manifest: sha256 over its deterministic protobuf encoding.
func Digest(m *acplugv1.ManifestResponse) string {
	raw, _ := proto.MarshalOptions{Deterministic: true}.Marshal(m)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func capabilityKey(c string) string {
	k, _, _ := strings.Cut(c, "=")
	return k
}

func isSchema(v *structpb.Value) bool {
	switch v.GetKind().(type) {
	case *structpb.Value_StructValue, *structpb.Value_BoolValue:
		return true
	}
	return false
}

func validateSettings(p *acplugv1.ProvidedPort, settings any) error {
	if settings == nil {
		settings = map[string]any{}
	}
	raw, err := json.Marshal(settings)
	if err != nil {
		return fmt.Errorf("port %q config: %w", p.GetPort(), err)
	}
	if err := validateJSON(p.GetConfigSchema(), raw); err != nil {
		return fmt.Errorf("port %q config does not match the plugin's config_schema: %w", p.GetPort(), err)
	}
	return nil
}

type tokenAuth struct {
	token  string
	secure bool
}

func (t tokenAuth) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + t.token}, nil
}

func (t tokenAuth) RequireTransportSecurity() bool { return t.secure }

func dialGRPC(cfg Config, tlsCfg *tls.Config, token string, maxBytes int) (*grpc.ClientConn, error) {
	opts := []grpc.DialOption{grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxBytes), grpc.MaxCallSendMsgSize(maxBytes))}
	if cfg.TLS.Insecure {
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	} else {
		opts = append(opts, grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)))
	}
	if token != "" {
		opts = append(opts, grpc.WithPerRPCCredentials(tokenAuth{token: token, secure: !cfg.TLS.Insecure}))
	}
	return grpc.NewClient(cfg.Endpoint, opts...)
}

func tlsConfig(t TLS) (*tls.Config, error) {
	if t.Insecure {
		return nil, nil
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: t.ServerName}
	if t.CA != "" {
		pem, err := os.ReadFile(t.CA)
		if err != nil {
			return nil, fmt.Errorf("read tls ca: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("tls ca holds no certificate")
		}
		cfg.RootCAs = pool
	}
	if t.Cert != "" {
		pair, err := tls.LoadX509KeyPair(t.Cert, t.Key)
		if err != nil {
			return nil, fmt.Errorf("load tls client certificate: %w", err)
		}
		cfg.Certificates = []tls.Certificate{pair}
	}
	return cfg, nil
}
