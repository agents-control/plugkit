package outbox

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	specSecret = "whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw"
	specID     = "msg_p5jXN8AQM9LWM0D4loKWxJek"
	specTS     = "1614265330"
	specBody   = `{"test": 2432232314}`
	specSig    = "v1,g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE="
)

func TestParseKey(t *testing.T) {
	for name, tc := range map[string]struct {
		secret  string
		wantLen int
		bad     bool
	}{
		"spec secret decodes":    {specSecret, 24, false},
		"raw 32 is its bytes":    {strings.Repeat("k", 32), 32, false},
		"raw has no cap":         {strings.Repeat("k", 200), 200, false},
		"raw 24 fails":           {strings.Repeat("k", 24), 0, true},
		"malformed base64 fails": {"whsec_!!!notbase64", 0, true},
		"short decoded fails":    {"whsec_YWJj", 0, true},
		"short raw fails":        {"short", 0, true},
		"decoded too long fails": {"whsec_" + base64.StdEncoding.EncodeToString(make([]byte, 65)), 0, true},
		"empty fails":            {"", 0, true},
	} {
		t.Run(name, func(t *testing.T) {
			key, err := ParseKey(tc.secret)
			if tc.bad != (err != nil) || (tc.bad && !errors.Is(err, ErrBadKey)) || len(key) != tc.wantLen {
				t.Fatalf("got %d bytes, err %v", len(key), err)
			}
		})
	}
}

func TestSignGoldenVector(t *testing.T) {
	key, _ := ParseKey(specSecret)
	if got := Sign(key, specID, specTS, []byte(specBody)); got != specSig {
		t.Fatalf("got %s want %s", got, specSig)
	}
}

func TestVerify(t *testing.T) {
	key, _ := ParseKey(specSecret)
	at := time.Unix(1614265330, 0)
	hdr := func(id, ts, sig string) http.Header {
		return http.Header{"Webhook-Id": {id}, "Webhook-Timestamp": {ts}, "Webhook-Signature": {sig}}
	}
	for name, tc := range map[string]struct {
		h    http.Header
		now  time.Time
		body string
		want error
	}{
		"golden vector verifies":        {hdr(specID, specTS, specSig), at, specBody, nil},
		"any listed signature verifies": {hdr(specID, specTS, "v1,AAAA "+specSig), at, specBody, nil},
		"tampered body fails":           {hdr(specID, specTS, specSig), at, specBody + " ", ErrBadSignature},
		"v2 only fails":                 {hdr(specID, specTS, "v2,"+specSig[3:]), at, specBody, ErrBadSignature},
		"old timestamp fails":           {hdr(specID, specTS, specSig), at.Add(6 * time.Minute), specBody, ErrStaleTimestamp},
		"future timestamp fails":        {hdr(specID, specTS, specSig), at.Add(-6 * time.Minute), specBody, ErrStaleTimestamp},
		"non numeric timestamp fails":   {hdr(specID, "soon", specSig), at, specBody, ErrStaleTimestamp},
		"missing header fails":          {hdr("", specTS, specSig), at, specBody, ErrMissingHeaders},
	} {
		t.Run(name, func(t *testing.T) {
			if err := Verify(key, tc.h, []byte(tc.body), tc.now, 5*time.Minute); !errors.Is(err, tc.want) {
				t.Fatalf("got %v want %v", err, tc.want)
			}
		})
	}
}

type posted struct {
	h    http.Header
	body []byte
}

func receiver(t *testing.T, status func(n int) int) (*httptest.Server, func() []posted) {
	t.Helper()
	var mu sync.Mutex
	var got []posted
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, posted{r.Header.Clone(), b})
		n := len(got)
		mu.Unlock()
		w.WriteHeader(status(n))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []posted { mu.Lock(); defer mu.Unlock(); return got }
}

func events(n int) []Event {
	out := make([]Event, n)
	for i := range out {
		out[i] = Event{ID: "e" + string(rune('1'+i)), Source: "s", SpecVersion: "1.0", Type: "t", Time: time.Unix(1, 0), Tenant: "acme", Extensions: map[string]any{"sequence": 7}}
	}
	return out
}

func TestBatchPost(t *testing.T) {
	srv, got := receiver(t, func(int) int { return 200 })
	s, err := NewWebhookSink(WebhookConfig{URL: srv.URL, Secret: specSecret})
	if err != nil {
		t.Fatal(err)
	}
	evs := events(2)
	evs[1].Extensions["traceparent"] = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	for range 2 {
		if err := s.Publish(t.Context(), evs); err != nil {
			t.Fatal(err)
		}
	}
	p := got()
	if len(p) != 2 || p[0].h.Get("webhook-id") != p[1].h.Get("webhook-id") || !strings.HasPrefix(p[0].h.Get("webhook-id"), "msg_") {
		t.Fatalf("webhook-id not stable across retries: %v", p)
	}
	if p[0].h.Get("Content-Type") != "application/cloudevents-batch+json" || p[0].h.Get("traceparent") == "" {
		t.Fatalf("headers %v", p[0].h)
	}
	key, _ := ParseKey(specSecret)
	if err := Verify(key, p[0].h, p[0].body, time.Now(), time.Minute); err != nil {
		t.Fatal(err)
	}
	var body []map[string]any
	if err := json.Unmarshal(p[0].body, &body); err != nil || len(body) != 2 || body[0]["sequence"] != float64(7) || body[0]["tenant"] != "acme" {
		t.Fatalf("body %s err %v", p[0].body, err)
	}
}

func TestSingleStopsAtFirstFailure(t *testing.T) {
	srv, got := receiver(t, func(n int) int {
		if n%2 == 0 {
			return 503
		}
		return 204
	})
	s, err := NewWebhookSink(WebhookConfig{URL: srv.URL, Mode: Single})
	if err != nil {
		t.Fatal(err)
	}
	n, err := s.Deliver(t.Context(), events(3))
	var se *StatusError
	if n != 1 || !errors.As(err, &se) || se.Code != 503 || len(got()) != 2 {
		t.Fatalf("accepted %d err %v posts %d", n, err, len(got()))
	}
	if got()[0].h.Get("webhook-id") != "e1" || got()[0].h.Get("webhook-signature") != "" {
		t.Fatalf("headers %v", got()[0].h)
	}
	if err := s.Publish(t.Context(), events(3)); err == nil {
		t.Fatal("Publish must fail on a partial batch")
	}
}

func TestRedirectIsNotFollowed(t *testing.T) {
	srv, got := receiver(t, func(int) int { return 302 })
	s, _ := NewWebhookSink(WebhookConfig{URL: srv.URL})
	var se *StatusError
	if err := s.Publish(t.Context(), events(1)); !errors.As(err, &se) || se.Code != 302 || len(got()) != 1 {
		t.Fatalf("err %v", err)
	}
}

func TestPerPostTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { time.Sleep(300 * time.Millisecond) }))
	defer srv.Close()
	s, _ := NewWebhookSink(WebhookConfig{URL: srv.URL, Timeout: 30 * time.Millisecond})
	start := time.Now()
	if err := s.Publish(context.Background(), events(1)); err == nil || time.Since(start) > 250*time.Millisecond {
		t.Fatalf("err %v after %v", err, time.Since(start))
	}
}

func TestNewWebhookSinkRejects(t *testing.T) {
	for name, cfg := range map[string]WebhookConfig{
		"relative url":  {URL: "/hook"},
		"ftp url":       {URL: "ftp://h/x"},
		"malformed key": {URL: "http://h/x", Secret: "whsec_!!"},
	} {
		if _, err := NewWebhookSink(cfg); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestEventExtensionsNeverShadowCoreAttributes(t *testing.T) {
	e := events(1)[0]
	e.Extensions["id"] = "evil"
	e.Extensions["data"] = "evil"
	b, _ := json.Marshal(e)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if m["id"] != "e1" || m["data"] != nil {
		t.Fatalf("%s", b)
	}
}

func TestVerifyRefusesWeakInputs(t *testing.T) {
	h := http.Header{"Webhook-Id": {specID}, "Webhook-Timestamp": {specTS}, "Webhook-Signature": {specSig}}
	now := time.Unix(1614265330, 0)
	if err := Verify(nil, h, []byte(specBody), now, time.Minute); !errors.Is(err, ErrBadKey) {
		t.Fatalf("empty key: %v", err)
	}
	if err := Verify(make([]byte, 23), h, []byte(specBody), now, time.Minute); !errors.Is(err, ErrBadKey) {
		t.Fatalf("short key: %v", err)
	}
	key, _ := ParseKey(specSecret)
	if err := Verify(key, h, []byte(specBody), now, -time.Minute); !errors.Is(err, ErrBadTolerance) {
		t.Fatalf("negative tolerance: %v", err)
	}
}

func TestBatchIDGolden(t *testing.T) {
	if got := batchID(events(2)); got != "msg_afd690b15b4e78be8eedf3eac5829d47" {
		t.Fatalf("got %s", got)
	}
}

func TestBatchFailureAcceptsNone(t *testing.T) {
	srv, _ := receiver(t, func(int) int { return 500 })
	s, _ := NewWebhookSink(WebhookConfig{URL: srv.URL})
	if n, err := s.Deliver(t.Context(), events(3)); n != 0 || err == nil {
		t.Fatalf("n %d err %v", n, err)
	}
}

func TestCallerClientIsNotModified(t *testing.T) {
	c := &http.Client{Timeout: time.Second}
	if _, err := NewWebhookSink(WebhookConfig{URL: "http://h/x", Client: c}); err != nil {
		t.Fatal(err)
	}
	if c.CheckRedirect != nil {
		t.Fatal("the caller's client was changed")
	}
}

func TestEventJSONRoundTripKeepsTypes(t *testing.T) {
	in := events(1)[0]
	in.Data = json.RawMessage(`{"n":1}`)
	in.Extensions = map[string]any{"sequence": 9007199254740993, "flag": true, "name": "x", "id": "evil"}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Event
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.ID != "e1" || string(out.Data) != `{"n":1}` || !out.Time.Equal(in.Time) || out.Tenant != "acme" {
		t.Fatalf("core lost: %+v", out)
	}
	if out.Extensions["sequence"] != json.Number("9007199254740993") || out.Extensions["flag"] != true || out.Extensions["name"] != "x" || len(out.Extensions) != 3 {
		t.Fatalf("extensions %v", out.Extensions)
	}
}

func TestEmptyOptionalAttributesAreOmitted(t *testing.T) {
	b, _ := json.Marshal(Event{ID: "e", Source: "s", SpecVersion: "1.0", Type: "t", Time: time.Unix(1, 0), Tenant: "acme"})
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	for _, k := range []string{"subject", "datacontenttype", "dataschema", "data"} {
		if _, ok := m[k]; ok {
			t.Errorf("%s emitted empty: %s", k, b)
		}
	}
	if _, ok := m["workspace"]; !ok || m["tenant"] != "acme" {
		t.Fatalf("tenant and workspace must always go out: %s", b)
	}
}
