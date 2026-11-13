package discovery_test

import (
	"net/http"
	"net/http/httptest"
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
