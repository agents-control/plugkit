package outbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Mode picks the body of a webhook post.
type Mode int

const (
	// Batch posts every event of a Publish as one CloudEvents JSON batch.
	Batch Mode = iota
	// Single posts each event on its own, in order, stopping at the first failure.
	Single
)

const defaultTimeout = 15 * time.Second

// WebhookConfig configures a WebhookSink; an empty Secret leaves posts unsigned, a zero Timeout means 15s.
type WebhookConfig struct {
	URL     string
	Secret  string
	Mode    Mode
	Timeout time.Duration
	Client  *http.Client
}

// StatusError is a non-2xx answer, a redirect included; relays map Code to their own retry classes.
type StatusError struct{ Code int }

// Error describes the answer.
func (e *StatusError) Error() string { return fmt.Sprintf("sink answered %d", e.Code) }

// WebhookSink posts events as Standard Webhooks signed CloudEvents JSON and never follows redirects.
type WebhookSink struct {
	url     string
	key     []byte
	mode    Mode
	timeout time.Duration
	client  http.Client
	now     func() time.Time
}

// NewWebhookSink validates cfg: an absolute http(s) URL and, when set, a Secret that ParseKey accepts.
func NewWebhookSink(cfg WebhookConfig) (*WebhookSink, error) {
	if u, err := url.Parse(cfg.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("outbox: the sink URL must be an absolute http or https URL")
	}
	s := &WebhookSink{url: cfg.URL, mode: cfg.Mode, timeout: cfg.Timeout, now: time.Now}
	if s.timeout == 0 {
		s.timeout = defaultTimeout
	}
	if cfg.Client != nil {
		s.client = *cfg.Client
	}
	s.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if cfg.Secret != "" {
		key, err := ParseKey(cfg.Secret)
		if err != nil {
			return nil, err
		}
		s.key = key
	}
	return s, nil
}

// Publish implements Sink: it fails unless Deliver accepted every event.
func (s *WebhookSink) Publish(ctx context.Context, events []Event) error {
	n, err := s.Deliver(ctx, events)
	if err == nil && n != len(events) {
		return fmt.Errorf("sink accepted %d of %d events", n, len(events))
	}
	return err
}

// Deliver posts events and returns how many leading ones were accepted, with the error that stopped the next one.
// Batch mode accepts all or none; webhook-id is the event id (Single) or a hash of the ids (Batch), so retries repeat it.
func (s *WebhookSink) Deliver(ctx context.Context, events []Event) (int, error) {
	if len(events) == 0 {
		return 0, nil
	}
	if s.mode == Batch {
		if err := s.post(ctx, batchID(events), "application/cloudevents-batch+json", events, events); err != nil {
			return 0, err
		}
		return len(events), nil
	}
	for i, e := range events {
		if err := s.post(ctx, e.ID, "application/cloudevents+json", e, events[i:i+1]); err != nil {
			return i, err
		}
	}
	return len(events), nil
}

func batchID(events []Event) string {
	h := sha256.New()
	for _, e := range events {
		h.Write([]byte(e.ID + "\n"))
	}
	return "msg_" + hex.EncodeToString(h.Sum(nil))[:32]
}

func (s *WebhookSink) post(ctx context.Context, id, contentType string, payload any, carried []Event) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode payload: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(body))
	if err != nil {
		return errors.New("build request")
	}
	ts := strconv.FormatInt(s.now().Unix(), 10)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("webhook-id", id)
	req.Header.Set("webhook-timestamp", ts)
	if len(s.key) > 0 {
		req.Header.Set("webhook-signature", Sign(s.key, id, ts, body))
	}
	for _, e := range carried {
		if tp := e.Extensions["traceparent"]; tp != "" {
			req.Header.Set("traceparent", tp)
			break
		}
	}
	res, err := s.client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("post: %w", err)
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<16))
	if res.StatusCode/100 != 2 {
		return &StatusError{Code: res.StatusCode}
	}
	return nil
}

var reservedAttributes = strings.Fields("id source specversion type subject time datacontenttype dataschema data tenant workspace")

// MarshalJSON encodes the event as CloudEvents JSON: Extensions become top-level attributes and never shadow a core one.
func (e Event) MarshalJSON() ([]byte, error) {
	m := map[string]any{"id": e.ID, "source": e.Source, "specversion": e.SpecVersion, "type": e.Type, "time": e.Time.UTC().Format(time.RFC3339Nano)}
	for k, v := range map[string]string{"subject": e.Subject, "datacontenttype": e.DataContentType, "dataschema": e.DataSchema, "tenant": e.Tenant, "workspace": e.Workspace} {
		if v != "" {
			m[k] = v
		}
	}
	if len(e.Data) > 0 {
		m["data"] = e.Data
	}
	for k, v := range e.Extensions {
		if !slices.Contains(reservedAttributes, k) {
			m[k] = v
		}
	}
	return json.Marshal(m)
}
