// Package outbox holds the batch Sink port shared by every engine's relay and its acplug plugin adapter.
package outbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"

	acplugv1 "github.com/agents-control/plugkit/api/acplug/v1"
	"github.com/agents-control/plugkit/host"
)

// Event is a CloudEvents-shaped outbox row; engine-specific attributes go in Extensions as JSON values, kept typed on the wire.
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
	Extensions      map[string]any
}

// Sink delivers a batch at least once; it fails the whole batch unless every event was accepted.
type Sink interface {
	Publish(ctx context.Context, events []Event) error
}

var coreAttributes = []string{"id", "source", "specversion", "type", "subject", "time", "datacontenttype", "dataschema", "data", "tenant", "workspace"}

// MarshalJSON encodes CloudEvents JSON: Extensions become top-level attributes with their types kept and never shadow a core one.
// Core attributes except data are always emitted, as every engine does today.
func (e Event) MarshalJSON() ([]byte, error) {
	m := map[string]any{"id": e.ID, "source": e.Source, "specversion": e.SpecVersion, "type": e.Type, "subject": e.Subject,
		"time": e.Time.UTC().Format(time.RFC3339Nano), "datacontenttype": e.DataContentType, "dataschema": e.DataSchema,
		"tenant": e.Tenant, "workspace": e.Workspace}
	if len(e.Data) > 0 {
		m["data"] = e.Data
	}
	for k, v := range e.Extensions {
		if !slices.Contains(coreAttributes, k) {
			m[k] = v
		}
	}
	return json.Marshal(m)
}

// UnmarshalJSON decodes CloudEvents JSON; unknown attributes land in Extensions, numbers as json.Number.
func (e *Event) UnmarshalJSON(b []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	var core struct {
		ID, Source, SpecVersion, Type, Subject, DataContentType, DataSchema, Tenant, Workspace string
		Time                                                                                   time.Time
		Data                                                                                   json.RawMessage
	}
	in := map[string]*string{"id": &core.ID, "source": &core.Source, "specversion": &core.SpecVersion, "type": &core.Type, "subject": &core.Subject,
		"datacontenttype": &core.DataContentType, "dataschema": &core.DataSchema, "tenant": &core.Tenant, "workspace": &core.Workspace}
	out := Event{}
	for k, v := range raw {
		switch {
		case k == "time":
			if err := json.Unmarshal(v, &core.Time); err != nil {
				return fmt.Errorf("event time: %w", err)
			}
		case k == "data":
			core.Data = v
		case in[k] != nil:
			if err := json.Unmarshal(v, in[k]); err != nil {
				return fmt.Errorf("event %s: %w", k, err)
			}
		default:
			d := json.NewDecoder(bytes.NewReader(v))
			d.UseNumber()
			var x any
			if err := d.Decode(&x); err != nil {
				return fmt.Errorf("event %s: %w", k, err)
			}
			if out.Extensions == nil {
				out.Extensions = map[string]any{}
			}
			out.Extensions[k] = x
		}
	}
	out.ID, out.Source, out.SpecVersion, out.Type, out.Subject, out.DataContentType, out.DataSchema, out.Tenant, out.Workspace, out.Time, out.Data =
		core.ID, core.Source, core.SpecVersion, core.Type, core.Subject, core.DataContentType, core.DataSchema, core.Tenant, core.Workspace, core.Time, core.Data
	*e = out
	return nil
}

func stringExtensions(in map[string]any) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		if s, ok := v.(string); ok {
			out[k] = s
		} else if b, err := json.Marshal(v); err == nil {
			out[k] = string(b)
		}
	}
	return out
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
			Tenant: e.Tenant, Workspace: e.Workspace, Extensions: stringExtensions(e.Extensions),
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
