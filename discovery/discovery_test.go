package discovery_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agents-control/plugkit/discovery"
)

func TestFetchSendsAuth(t *testing.T) {
	doc := discovery.Handler(func() discovery.Document { return discovery.Document{Engine: "e"} })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer t" || r.Header.Get("X-Admin-Key") != "k" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		doc.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	if _, err := discovery.Fetch(t.Context(), srv.Client(), srv.URL); err == nil {
		t.Fatal("unauthenticated fetch must fail")
	}
	d, err := discovery.Fetch(t.Context(), srv.Client(), srv.URL, discovery.WithBearer("t"), discovery.WithHeader("X-Admin-Key", "k"))
	if err != nil || d.Engine != "e" {
		t.Fatalf("fetch = %+v %v", d, err)
	}
}

func TestReplacePortKeepsOtherProviders(t *testing.T) {
	d := discovery.Document{Adapters: []discovery.Row{
		{Kind: "adapter", Port: "store", Tier: "builtin", Name: "postgres", Status: "serving"},
		{Kind: "adapter", Port: "transport", Tier: "builtin", Name: "http", Status: "serving"},
	}}
	d.ReplacePort("store", discovery.EmbeddedRow("store", "custom-kv"))
	d.ReplacePort("transport", append(d.Rows("transport"), discovery.EmbeddedRow("transport", "inproc"))...)
	if len(d.Adapters) != 3 {
		t.Fatalf("rows = %+v", d.Adapters)
	}
	var uses []string
	for _, r := range d.Adapters {
		uses = append(uses, r.Port+" "+r.Use()+" "+r.Status)
	}
	want := []string{"store embedded:custom-kv serving", "transport builtin:http serving", "transport embedded:inproc serving"}
	if strings.Join(uses, "|") != strings.Join(want, "|") {
		t.Fatalf("rows = %v", uses)
	}
}
