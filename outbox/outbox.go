// Package outbox holds the batch Sink port shared by every engine's relay and its acplug plugin adapter.
package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"

	acplugv1 "github.com/agents-control/plugkit/api/acplug/v1"
	"github.com/agents-control/plugkit/host"
)

// Event is a CloudEvents-shaped outbox row; engine-specific attributes go in Extensions.
type Event struct {
	ID              string
	Source          string
	SpecVersion     string
	Type            string
	Subject         string
	Time            time.Time
	DataContentType string
	DataSchema      string
	Data            json.RawMessage
	Tenant          string
	Workspace       string
	Extensions      map[string]string
}

// Sink delivers a batch at least once; it fails the whole batch unless every event was accepted.
type Sink interface {
	Publish(ctx context.Context, events []Event) error
}

type pluginSink struct{ client acplugv1.EventSinkClient }

// PluginSink returns a Sink served by the acplug EventSink behind cc, normally host.Client.Port("sink").
func PluginSink(cc grpc.ClientConnInterface) Sink {
	return pluginSink{client: acplugv1.NewEventSinkClient(cc)}
}

func (s pluginSink) Publish(ctx context.Context, events []Event) error {
	req := &acplugv1.PublishRequest{Events: make([]*acplugv1.Event, 0, len(events))}
	for _, e := range events {
		data, err := host.ToValue(e.Data)
		if err != nil {
			return fmt.Errorf("event %s data: %w", e.ID, err)
		}
		req.Events = append(req.Events, &acplugv1.Event{
			Id: e.ID, Source: e.Source, SpecVersion: e.SpecVersion, Type: e.Type, Subject: e.Subject,
			Time: timestamppb.New(e.Time), DataContentType: e.DataContentType, DataSchema: e.DataSchema, Data: data,
			Tenant: e.Tenant, Workspace: e.Workspace, Extensions: e.Extensions,
		})
	}
	resp, err := s.client.Publish(ctx, req)
	if err != nil {
		return fmt.Errorf("plugin sink: %w", err)
	}
	if int(resp.GetAccepted()) != len(events) {
		return fmt.Errorf("plugin sink accepted %d of %d events", resp.GetAccepted(), len(events))
	}
	return nil
}
