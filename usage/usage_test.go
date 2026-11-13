package usage_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/invopop/jsonschema"

	"github.com/agents-control/plugkit/usage"
)

type author struct {
	Kind      string `json:"kind"`
	Principal string `json:"principal" usage:"actorFrom=author.kind"`
}

type line struct {
	Bytes int `json:"bytes" usage:"quantity=byte"`
}

type payload struct {
	Author   author             `json:"author"`
	Runner   string             `json:"runner" usage:"dimension=runner"`
	Duration int64              `json:"durationMs" usage:"quantity=millisecond"`
	Usage    map[string]float64 `json:"usage" usage:"quantities"`
	Lines    []line             `json:"lines"`
	Size     int64              `json:"size"`
}

func annotated(t *testing.T, u usage.Event) map[string]any {
	t.Helper()
	s := (&jsonschema.Reflector{DoNotReference: true}).Reflect(payload{})
	usage.Annotate(s, reflect.TypeOf(payload{}), u, map[string]string{"person": "user"})
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func prop(doc map[string]any, path ...string) any {
	var cur any = doc
	for _, p := range path {
		cur = cur.(map[string]any)[p]
	}
	return cur
}

func TestABillingEventGetsEveryAnnotation(t *testing.T) {
	doc := annotated(t, usage.Event{Count: "run"})
	want := map[string]any{
		"root":     map[string]any{"count": "run"},
		"runner":   map[string]any{"dimension": "runner"},
		"duration": map[string]any{"quantity": "millisecond"},
		"usage":    map[string]any{"quantities": true},
		"lines":    map[string]any{"quantity": "byte"},
		"actor":    map[string]any{"actor": map[string]any{"roleFrom": "author.kind", "roles": map[string]any{"person": "user"}}},
	}
	got := map[string]any{
		"root":     prop(doc, "x-usage"),
		"runner":   prop(doc, "properties", "runner", "x-usage"),
		"duration": prop(doc, "properties", "durationMs", "x-usage"),
		"usage":    prop(doc, "properties", "usage", "x-usage"),
		"lines":    prop(doc, "properties", "lines", "items", "properties", "bytes", "x-usage"),
		"actor":    prop(doc, "properties", "author", "properties", "principal", "x-usage"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
	if prop(doc, "properties", "size", "x-usage") != nil {
		t.Fatal("an untagged field is annotated")
	}
}

func TestAStockOnlyEventCarriesNoQuantityAndDimensionsOnlyWhenAsked(t *testing.T) {
	stock := &usage.Stock{Field: "size", Unit: "byte", Sign: -1, Key: "blob.bytes"}
	doc := annotated(t, usage.Event{Stock: stock})
	if prop(doc, "properties", "durationMs", "x-usage") != nil || prop(doc, "properties", "runner", "x-usage") != nil || prop(doc, "properties", "usage", "x-usage") != nil {
		t.Fatalf("stock-only event billed: %v", doc["properties"])
	}
	if prop(doc, "properties", "author", "properties", "principal", "x-usage") == nil {
		t.Fatal("attribution is kept on a stock event")
	}
	doc = annotated(t, usage.Event{Stock: stock, StockDimensions: true})
	if prop(doc, "properties", "runner", "x-usage") == nil || prop(doc, "properties", "durationMs", "x-usage") != nil {
		t.Fatalf("StockDimensions: %v", doc["properties"])
	}
}
