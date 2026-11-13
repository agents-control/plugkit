// Package serve helps write an acplug/1 plugin in Go: the Plugin service, a reference memory sink,
// constant-time bearer checks and a loopback server for tests and the conformance corpus.
package serve

import (
	"context"
	"crypto/subtle"
	"net"
	"slices"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	acplugv1 "github.com/agents-control/plugkit/api/acplug/v1"
	"github.com/agents-control/plugkit/host"
)

// Plugin serves acplug.v1.Plugin from a fixed manifest and a settable health status.
type Plugin struct {
	acplugv1.UnimplementedPluginServer

	mu       sync.Mutex
	manifest *acplugv1.ManifestResponse
	health   acplugv1.HealthResponse_Status
}

// NewPlugin returns a SERVING plugin that answers with manifest.
func NewPlugin(manifest *acplugv1.ManifestResponse) *Plugin {
	return &Plugin{manifest: manifest, health: acplugv1.HealthResponse_STATUS_SERVING}
}

// SetHealth changes the status Health reports.
func (p *Plugin) SetHealth(s acplugv1.HealthResponse_Status) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.health = s
}

// SetManifest replaces the manifest Manifest returns.
func (p *Plugin) SetManifest(m *acplugv1.ManifestResponse) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.manifest = m
}

// Manifest returns a copy of the configured manifest.
func (p *Plugin) Manifest(context.Context, *acplugv1.ManifestRequest) (*acplugv1.ManifestResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return proto.CloneOf(p.manifest), nil
}

// Health reports the configured status.
func (p *Plugin) Health(context.Context, *acplugv1.HealthRequest) (*acplugv1.HealthResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return &acplugv1.HealthResponse{Status: p.health}, nil
}

// SinkManifest is the manifest of a plugin that serves only the sink port with an object config schema.
func SinkManifest(name string) *acplugv1.ManifestResponse {
	schema, _ := structpb.NewValue(map[string]any{"type": "object"})
	return &acplugv1.ManifestResponse{
		Protocol: host.ProtocolID, ProtocolVersion: &acplugv1.VersionRange{Min: 1, Max: 1}, Name: name, Version: "1.0.0",
		Ports: []*acplugv1.ProvidedPort{{Port: "sink", ConfigSchema: schema, Capabilities: []string{"batch"}}},
	}
}

// MemorySink is the reference EventSink: it keeps every event once, by id, in arrival order.
type MemorySink struct {
	acplugv1.UnimplementedEventSinkServer

	mu     sync.Mutex
	events map[string]*acplugv1.Event
	order  []string
}

// Publish stores the batch, ignoring ids already seen, and accepts every event.
func (s *MemorySink) Publish(_ context.Context, req *acplugv1.PublishRequest) (*acplugv1.PublishResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.events == nil {
		s.events = map[string]*acplugv1.Event{}
	}
	for _, e := range req.GetEvents() {
		if e.GetId() == "" {
			return nil, status.Error(codes.InvalidArgument, "event without id")
		}
		if _, dup := s.events[e.GetId()]; !dup {
			s.events[e.GetId()] = e
			s.order = append(s.order, e.GetId())
		}
	}
	return &acplugv1.PublishResponse{Accepted: int32(len(req.GetEvents()))}, nil
}

// IDs lists the stored event ids in arrival order.
func (s *MemorySink) IDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.order)
}

// Bearer returns server options that refuse any call whose bearer token differs from token, compared in constant time.
func Bearer(token string) []grpc.ServerOption {
	check := func(ctx context.Context) error {
		md, _ := metadata.FromIncomingContext(ctx)
		got := ""
		if v := md.Get("authorization"); len(v) == 1 {
			got = v[0]
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte("Bearer "+token)) != 1 {
			return status.Error(codes.Unauthenticated, "bad bearer token")
		}
		return nil
	}
	return []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
			if err := check(ctx); err != nil {
				return nil, err
			}
			return next(ctx, req)
		}),
		grpc.ChainStreamInterceptor(func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, next grpc.StreamHandler) error {
			if err := check(ss.Context()); err != nil {
				return err
			}
			return next(srv, ss)
		}),
	}
}

// Start serves on a loopback port: register adds the services, the result is the address and a stop function.
func Start(register func(*grpc.Server), opts ...grpc.ServerOption) (string, func(), error) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	srv := grpc.NewServer(opts...)
	register(srv)
	go func() { _ = srv.Serve(lis) }()
	return lis.Addr().String(), srv.Stop, nil
}
