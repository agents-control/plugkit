// Package sinktest is the conformance suite of the batch Sink port: Run checks a Go Sink, Suite checks a plugin through conform.
package sinktest

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/agents-control/plugkit/conform"
	"github.com/agents-control/plugkit/outbox"
)

// Version names this suite in conformance reports.
const Version = "sinktest@1"

// Suite returns the sink suite for the conform runner.
func Suite() conform.Suite {
	return conform.Suite{Port: "sink", Version: Version, Cases: func(_ context.Context, cc grpc.ClientConnInterface) []conform.Case {
		return Cases(outbox.PluginSink(cc))
	}}
}

// Run checks s with every case as a subtest.
func Run(t *testing.T, s outbox.Sink) {
	t.Helper()
	for _, c := range Cases(s) {
		t.Run(c.Name, func(t *testing.T) {
			if err := c.Run(t.Context()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Cases are the sink checks: an empty batch, a whole batch and its redelivery are all accepted.
// Ids and tenant are unique per call, so runs against a shared sink never collide.
func Cases(s outbox.Sink) []conform.Case {
	now, run := time.Now().UTC(), rand.Text()[:12]
	tenant := "conf-" + run
	batch := []outbox.Event{
		{ID: run + "-1", Source: "conformance", SpecVersion: "1.0", Type: "conformance.checked", Subject: "s", Time: now, DataContentType: "application/json", Data: json.RawMessage(`{"n":1}`), Tenant: tenant, Extensions: map[string]string{"sequence": "1"}},
		{ID: run + "-2", Source: "conformance", SpecVersion: "1.0", Type: "conformance.checked", Subject: "s", Time: now, Tenant: tenant, Workspace: "w"},
		{ID: run + "-3", Source: "conformance", SpecVersion: "1.0", Type: "conformance.checked", Subject: "s", Time: now, DataContentType: "application/json", Data: json.RawMessage(`[1,"two",null]`), Tenant: tenant},
	}
	return []conform.Case{
		{Name: "empty batch is accepted", Run: func(ctx context.Context) error { return s.Publish(ctx, nil) }},
		{Name: "batch is accepted whole", Run: func(ctx context.Context) error { return s.Publish(ctx, batch) }},
		{Name: "redelivered batch is accepted", Run: func(ctx context.Context) error { return s.Publish(ctx, batch) }},
	}
}
