// Package usage writes the neutral x-usage annotations of an event schema (MT-ADR-010) from the usage tags of its Go payload type.
package usage

import (
	"reflect"
	"strings"

	"github.com/invopop/jsonschema"
)

// Stock says a field adds (Sign 1) or removes (Sign -1) from the level named Key, in Unit.
type Stock struct {
	Field string `json:"field"`
	Unit  string `json:"unit"`
	Sign  int    `json:"sign"`
	Key   string `json:"key"`
}

// Exclude drops events whose Field equals Equals from usage.
type Exclude struct {
	Field  string `json:"field"`
	Equals string `json:"equals"`
}

// Event is the root annotation of an event schema: what one event counts, which stock it moves and which events are not
// usage; StockDimensions keeps the dimension tags on a stock-only event whose adder has them.
type Event struct {
	Count           string   `json:"count,omitempty"`
	Stock           *Stock   `json:"stock,omitempty"`
	Exclude         *Exclude `json:"exclude,omitempty"`
	StockDimensions bool     `json:"-"`
}

// Annotate sets u as the root x-usage of s and annotates the properties t tags with usage:"quantity=<unit>", "quantities",
// "dimension=<name>", "actor=<role>" or "actorFrom=<path>" (with roles); quantities need a count, dimensions a count or StockDimensions.
func Annotate(s *jsonschema.Schema, t reflect.Type, u Event, roles map[string]string) {
	if s.Extras == nil {
		s.Extras = map[string]any{}
	}
	s.Extras["x-usage"] = u
	annotate(s, t, u.Count != "", u.Count != "" || u.StockDimensions, roles)
}

func annotate(s *jsonschema.Schema, t reflect.Type, billing, dimensions bool, roles map[string]string) {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice {
		if s.Items != nil {
			s = s.Items
		}
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || s == nil {
		return
	}
	for f := range t.Fields() {
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if f.Anonymous && name == "" {
			annotate(s, f.Type, billing, dimensions, roles)
			continue
		}
		p, ok := s.Properties.Get(name)
		if !ok {
			continue
		}
		if k, ann, ok := note(f.Tag.Get("usage"), roles); ok && (billing || !strings.HasPrefix(k, "quantit")) && (dimensions || k != "dimension") {
			if p.Extras == nil {
				p.Extras = map[string]any{}
			}
			p.Extras["x-usage"] = map[string]any{k: ann}
		}
		annotate(p, f.Type, billing, dimensions, roles)
	}
}

func note(tag string, roles map[string]string) (string, any, bool) {
	k, v, found := strings.Cut(tag, "=")
	switch {
	case tag == "":
		return "", nil, false
	case k == "actorFrom":
		return "actor", map[string]any{"roleFrom": v, "roles": roles}, true
	case !found:
		return k, true, true
	}
	return k, v, true
}
