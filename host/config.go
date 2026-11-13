package host

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strings"
)

// Protocols a plugin endpoint can speak.
const (
	ProtocolGRPC = "grpc"
	ProtocolHTTP = "http"
)

// TLS configures transport security. Plaintext needs Insecure set explicitly; it is never the default.
type TLS struct {
	CA         string `yaml:"ca"`
	Cert       string `yaml:"cert"`
	Key        string `yaml:"key"`
	ServerName string `yaml:"serverName"`
	Insecure   bool   `yaml:"insecure"`
}

// Require holds what a plugin must offer to be accepted: a semver constraint, capabilities, and the digest
// of a passing conform report with the path where the host can read that report back.
type Require struct {
	Version      string   `yaml:"version"`
	Capabilities []string `yaml:"capabilities"`
	Conformance  string   `yaml:"conformance"`
	Report       string   `yaml:"report"`
}

// Config declares one plugin endpoint; CredentialRef is a secret reference sent as a bearer token, never a value.
// Ports is the allow-list of ports the host uses (empty means every declared port); Settings holds each port's config.
type Config struct {
	Name          string         `yaml:"name"`
	Endpoint      string         `yaml:"endpoint"`
	Protocol      string         `yaml:"protocol"`
	TLS           TLS            `yaml:"tls"`
	CredentialRef string         `yaml:"credentialRef"`
	Ports         []string       `yaml:"ports"`
	Require       Require        `yaml:"require"`
	Settings      map[string]any `yaml:"-"`
}

// Validate fills the default protocol and refuses incomplete or contradictory endpoint and TLS settings.
func (c *Config) Validate() error {
	if c.Name == "" || c.Endpoint == "" {
		return errors.New("plugin name and endpoint are required")
	}
	if c.Protocol == "" {
		c.Protocol = ProtocolGRPC
	}
	if c.Protocol != ProtocolGRPC && c.Protocol != ProtocolHTTP {
		return fmt.Errorf("plugin %s: protocol must be grpc or http", c.Name)
	}
	if (c.TLS.Cert == "") != (c.TLS.Key == "") {
		return fmt.Errorf("plugin %s: tls cert and key go together", c.Name)
	}
	if c.TLS.Insecure && (c.TLS.CA != "" || c.TLS.Cert != "") {
		return fmt.Errorf("plugin %s: tls.insecure excludes ca, cert and key", c.Name)
	}
	if c.Protocol == ProtocolHTTP {
		secure := strings.HasPrefix(c.Endpoint, "https://")
		if !secure && !strings.HasPrefix(c.Endpoint, "http://") {
			return fmt.Errorf("plugin %s: http endpoint needs an http:// or https:// prefix", c.Name)
		}
		if secure == c.TLS.Insecure {
			return fmt.Errorf("plugin %s: https endpoints need TLS settings, http:// endpoints need tls.insecure: true", c.Name)
		}
	}
	if c.CredentialRef != "" && !IsRef(c.CredentialRef) {
		return fmt.Errorf("plugin %s: credentialRef must be a reference such as env:NAME, never a value", c.Name)
	}
	return nil
}

func (c Config) allows(port string) bool { return len(c.Ports) == 0 || slices.Contains(c.Ports, port) }

// Plaintext reports whether traffic leaves the machine unencrypted: insecure TLS on an endpoint that is not loopback or a Unix socket.
func (c Config) Plaintext() bool {
	if !c.TLS.Insecure {
		return false
	}
	host := c.Endpoint
	if u, err := url.Parse(c.Endpoint); err == nil && u.Host != "" {
		host = u.Host
	}
	if strings.HasPrefix(host, "unix:") || strings.HasPrefix(c.Endpoint, "unix:") {
		return false
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host == "localhost" {
		return false
	}
	ip := net.ParseIP(host)
	return ip == nil || !ip.IsLoopback()
}
