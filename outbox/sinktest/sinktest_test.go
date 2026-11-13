package sinktest_test

import (
	"testing"

	"google.golang.org/grpc"

	acplugv1 "github.com/agents-control/plugkit/api/acplug/v1"
	"github.com/agents-control/plugkit/host"
	"github.com/agents-control/plugkit/outbox"
	"github.com/agents-control/plugkit/outbox/sinktest"
	"github.com/agents-control/plugkit/serve"
)

func TestPluginSinkPassesSuite(t *testing.T) {
	sink := &serve.MemorySink{}
	addr, stop, err := serve.Start(func(s *grpc.Server) {
		acplugv1.RegisterPluginServer(s, serve.NewPlugin(serve.SinkManifest("ref")))
		acplugv1.RegisterEventSinkServer(s, sink)
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	c, err := (&host.Host{}).Dial(t.Context(), host.Config{Name: "ref", Endpoint: addr, TLS: host.TLS{Insecure: true}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	cc, _ := c.Port("sink")
	sinktest.Run(t, outbox.PluginSink(cc))
	sinktest.Run(t, outbox.PluginSink(cc))
	if ids := sink.IDs(); len(ids) != 6 {
		t.Fatalf("stored %v, want each run's events once and runs not colliding", ids)
	}
	err = outbox.PluginSink(cc).Publish(t.Context(), []outbox.Event{{ID: "big", Data: []byte(`{"n":9007199254740993}`)}})
	if err == nil {
		t.Fatal("a number a double cannot hold must be refused before sending")
	}
}
