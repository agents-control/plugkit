// Package corpus serves the reference sink plugin and the broken plugins the conform runner must refuse (UP-ADR-001 §6.3).
package corpus

import (
	"context"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	acplugv1 "github.com/agents-control/plugkit/api/acplug/v1"
	"github.com/agents-control/plugkit/conform"
	"github.com/agents-control/plugkit/host"
	"github.com/agents-control/plugkit/serve"
)

type fixture struct {
	name     string
	want     string
	manifest func(*acplugv1.ManifestResponse)
	health   acplugv1.HealthResponse_Status
	plugin   func(*serve.Plugin) acplugv1.PluginServer
	sink     acplugv1.EventSinkServer
}

func fixtures() []fixture {
	return []fixture{
		{name: "reference"},
		{name: "protocol-range-excludes-host", want: "handshake", manifest: func(m *acplugv1.ManifestResponse) {
			m.ProtocolVersion = &acplugv1.VersionRange{Min: 2, Max: 3}
		}},
		{name: "unknown-protocol", want: "handshake", manifest: func(m *acplugv1.ManifestResponse) { m.Protocol = "otherplug/1" }},
		{name: "port-without-schema", want: "handshake", manifest: func(m *acplugv1.ManifestResponse) { m.Ports[0].ConfigSchema = nil }},
		{name: "not-serving", want: "handshake", health: acplugv1.HealthResponse_STATUS_NOT_SERVING},
		{name: "manifest-drift", want: "manifest is stable", plugin: func(p *serve.Plugin) acplugv1.PluginServer { return &drifting{Plugin: p} }},
		{name: "sink-accepts-fewer", want: "batch is accepted whole", sink: partialSink{}},
		{name: "sink-refuses-redelivery", want: "redelivered batch is accepted", sink: &onceSink{}},
		{name: "ignores-deadline", want: "batch is accepted whole", sink: slowSink{}, manifest: func(m *acplugv1.ManifestResponse) {
			m.Ports[0].Limits = &acplugv1.Limits{DeadlineMs: 200}
		}},
	}
}

// Start serves every fixture on loopback and returns them with their expected verdicts and one stop function.
func Start() ([]conform.Fixture, func(), error) {
	var out []conform.Fixture
	var stops []func()
	stop := func() {
		for _, s := range stops {
			s()
		}
	}
	for _, f := range fixtures() {
		m := serve.SinkManifest(f.name)
		if f.manifest != nil {
			f.manifest(m)
		}
		p := serve.NewPlugin(m)
		if f.health != acplugv1.HealthResponse_STATUS_UNSPECIFIED {
			p.SetHealth(f.health)
		}
		var ps acplugv1.PluginServer = p
		if f.plugin != nil {
			ps = f.plugin(p)
		}
		var sink acplugv1.EventSinkServer = &serve.MemorySink{}
		if f.sink != nil {
			sink = f.sink
		}
		addr, s, err := serve.Start(func(srv *grpc.Server) {
			acplugv1.RegisterPluginServer(srv, ps)
			acplugv1.RegisterEventSinkServer(srv, sink)
		})
		if err != nil {
			stop()
			return nil, nil, err
		}
		stops = append(stops, s)
		out = append(out, conform.Fixture{Name: f.name, Want: f.want, Config: host.Config{Name: f.name, Endpoint: addr, TLS: host.TLS{Insecure: true}}})
	}
	return out, stop, nil
}

type drifting struct {
	*serve.Plugin
	calls atomic.Int32
}

func (d *drifting) Manifest(ctx context.Context, req *acplugv1.ManifestRequest) (*acplugv1.ManifestResponse, error) {
	m, err := d.Plugin.Manifest(ctx, req)
	if err != nil {
		return nil, err
	}
	if d.calls.Add(1) > 1 {
		m = proto.CloneOf(m)
		m.Version = "1.0.1"
	}
	return m, nil
}

type partialSink struct {
	acplugv1.UnimplementedEventSinkServer
}

func (partialSink) Publish(_ context.Context, req *acplugv1.PublishRequest) (*acplugv1.PublishResponse, error) {
	return &acplugv1.PublishResponse{Accepted: int32(max(len(req.GetEvents())-1, 0))}, nil
}

type onceSink struct {
	serve.MemorySink
}

func (s *onceSink) Publish(ctx context.Context, req *acplugv1.PublishRequest) (*acplugv1.PublishResponse, error) {
	for _, e := range req.GetEvents() {
		for _, id := range s.IDs() {
			if id == e.GetId() {
				return nil, status.Error(codes.AlreadyExists, "duplicate event "+id)
			}
		}
	}
	return s.MemorySink.Publish(ctx, req)
}

type slowSink struct {
	acplugv1.UnimplementedEventSinkServer
}

func (slowSink) Publish(_ context.Context, req *acplugv1.PublishRequest) (*acplugv1.PublishResponse, error) {
	if len(req.GetEvents()) > 0 {
		time.Sleep(time.Second)
	}
	return &acplugv1.PublishResponse{Accepted: int32(len(req.GetEvents()))}, nil
}
