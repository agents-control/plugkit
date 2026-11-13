package host

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// SecretResolver turns a secret reference into its value in operator scope; engines adapt their secrets port to it.
type SecretResolver interface {
	Resolve(ctx context.Context, ref string) (string, error)
}

// ErrSecretNotFound is returned when a reference names no secret.
var ErrSecretNotFound = errors.New("secret not found")

var refPattern = regexp.MustCompile(`^[a-z][a-z0-9-]*:\S+$`)

// IsRef reports whether s has the reference form <resolver>:<ref>, such as env:NAME, file:/path or vault:path.
func IsRef(s string) bool { return refPattern.MatchString(s) && !strings.Contains(s, "://") }

// EnvSecrets resolves env:NAME and file:/path references; every other resolver is refused.
type EnvSecrets struct{}

// Resolve reads the variable or the file named by ref, trimming one trailing newline from files.
func (EnvSecrets) Resolve(_ context.Context, ref string) (string, error) {
	scheme, name, _ := strings.Cut(ref, ":")
	switch scheme {
	case "env":
		v, ok := os.LookupEnv(name)
		if !ok {
			return "", fmt.Errorf("%w: %s", ErrSecretNotFound, ref)
		}
		return v, nil
	case "file":
		data, err := os.ReadFile(name)
		if err != nil {
			return "", fmt.Errorf("%w: %s", ErrSecretNotFound, ref)
		}
		return strings.TrimSuffix(string(data), "\n"), nil
	}
	return "", fmt.Errorf("secret reference %q needs a resolver this host does not have", scheme)
}
