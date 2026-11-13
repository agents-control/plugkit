package outbox

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	secretPrefix = "whsec_"
	minKeyBytes  = 24
	maxKeyBytes  = 64
)

var (
	// ErrBadKey marks a secret that cannot be a Standard Webhooks signing key.
	ErrBadKey = errors.New("outbox: bad webhook key")
	// ErrMissingHeaders marks a delivery without webhook-id, webhook-timestamp or webhook-signature.
	ErrMissingHeaders = errors.New("outbox: missing webhook headers")
	// ErrStaleTimestamp marks a webhook-timestamp outside the tolerance or not a Unix second.
	ErrStaleTimestamp = errors.New("outbox: webhook timestamp outside tolerance")
	// ErrBadSignature marks a delivery no v1 signature of which matches the key.
	ErrBadSignature = errors.New("outbox: webhook signature mismatch")
)

// ParseKey returns the signing key of a secret: standard base64 after "whsec_" is decoded, anything else is its raw bytes.
// The key must be 24 to 64 bytes; a malformed secret fails, it never falls back to raw.
func ParseKey(secret string) ([]byte, error) {
	key := []byte(secret)
	if b64, ok := strings.CutPrefix(secret, secretPrefix); ok {
		var err error
		if key, err = base64.StdEncoding.DecodeString(b64); err != nil {
			return nil, fmt.Errorf("%w: a whsec_ secret must hold standard base64", ErrBadKey)
		}
	}
	if len(key) < minKeyBytes || len(key) > maxKeyBytes {
		return nil, fmt.Errorf("%w: the key must be %d to %d bytes", ErrBadKey, minKeyBytes, maxKeyBytes)
	}
	return key, nil
}

func mac(key []byte, id, timestamp string, body []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(id + "." + timestamp + "."))
	h.Write(body)
	return h.Sum(nil)
}

// Sign returns the webhook-signature value ("v1,<base64>") of a message under key.
func Sign(key []byte, id, timestamp string, body []byte) string {
	return "v1," + base64.StdEncoding.EncodeToString(mac(key, id, timestamp, body))
}

// Verify checks a received delivery in constant time: headers present, timestamp within tolerance of now, and one of
// the space-separated v1 signatures matches. Callers holding several keys try each.
func Verify(key []byte, h http.Header, body []byte, now time.Time, tolerance time.Duration) error {
	id, ts, sigs := h.Get("webhook-id"), h.Get("webhook-timestamp"), h.Get("webhook-signature")
	if id == "" || ts == "" || sigs == "" {
		return ErrMissingHeaders
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || now.Sub(time.Unix(sec, 0)).Abs() > tolerance {
		return ErrStaleTimestamp
	}
	want := mac(key, id, ts, body)
	ok := false
	for _, sig := range strings.Fields(sigs) {
		b64, found := strings.CutPrefix(sig, "v1,")
		got, err := base64.StdEncoding.DecodeString(b64)
		if found && err == nil && hmac.Equal(want, got) {
			ok = true
		}
	}
	if !ok {
		return ErrBadSignature
	}
	return nil
}
