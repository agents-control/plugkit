package host

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	acplugv1 "github.com/agents-control/plugkit/api/acplug/v1"
)

// Adapter states, as served by GET /v1/adapters.
const (
	StateServing    = "serving"
	StateDegraded   = "degraded"
	StateNotServing = "not_serving"
)

// Pin is the identity recorded at the handshake; a later manifest with another Digest marks the plugin degraded.
type Pin struct {
	Name     string
	Version  string
	Protocol string
	Digest   string
}

// Status is the last observed state of a plugin and why.
type Status struct {
	State  string
	Detail string
}

type enabledPort struct {
	conn         *portConn
	capabilities []string
	metadata     map[string]string
}

// Client is a dialed, negotiated and pinned plugin; Port hands out guarded connections for engine adapters.
type Client struct {
	cfg      Config
	request  *acplugv1.ManifestRequest
	conn     grpc.ClientConnInterface
	close    func() error
	health   string
	legacy   bool
	manifest *acplugv1.ManifestResponse
	pin      Pin
	ports    map[string]*enabledPort

	mu     sync.Mutex
	status Status
}

// Config returns the configuration the plugin was dialed with.
func (c *Client) Config() Config { return c.cfg }

// Manifest returns a copy of the pinned manifest.
func (c *Client) Manifest() *acplugv1.ManifestResponse {
	return proto.CloneOf(c.manifest)
}

// Pin returns the identity recorded at the handshake.
func (c *Client) Pin() Pin { return c.pin }

// Legacy reports whether the plugin was accepted through a deprecated pre-acplug protocol.
func (c *Client) Legacy() bool { return c.legacy }

// Ports lists the enabled ports: declared by the manifest and allowed by the configuration.
func (c *Client) Ports() []string { return slices.Sorted(maps.Keys(c.ports)) }

// Port returns the connection for an enabled port, bounded by its deadline, bulkhead and message size.
func (c *Client) Port(name string) (grpc.ClientConnInterface, error) {
	p, ok := c.ports[name]
	if !ok {
		return nil, fmt.Errorf("plugin %s does not serve port %q", c.cfg.Name, name)
	}
	return p.conn, nil
}

// Capabilities returns the declared capabilities of an enabled port narrowed to what the host supports.
func (c *Client) Capabilities(name string) []string {
	if p, ok := c.ports[name]; ok {
		return slices.Clone(p.capabilities)
	}
	return nil
}

// Metadata returns a copy of the descriptive metadata an enabled port declares, nil when the port is not enabled.
func (c *Client) Metadata(name string) map[string]string {
	if p, ok := c.ports[name]; ok {
		return maps.Clone(p.metadata)
	}
	return nil
}

// Health returns nil when the plugin reports SERVING.
func (c *Client) Health(ctx context.Context) error {
	_, err := c.check(ctx)
	return err
}

// Status returns the last observed state.
func (c *Client) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

// Recheck refreshes the state from Health and the manifest digest; drift keeps the pinned contract and degrades.
func (c *Client) Recheck(ctx context.Context) Status {
	st := Status{State: StateServing}
	if s, err := c.check(ctx); err != nil {
		st = Status{State: StateNotServing, Detail: err.Error()}
		if s == acplugv1.HealthResponse_STATUS_DEGRADED {
			st.State = StateDegraded
		}
	} else if digest, ok := c.manifestDigest(ctx); ok && digest != c.pin.Digest {
		st = Status{State: StateDegraded, Detail: "manifest drift: " + digest + " differs from pinned " + c.pin.Digest}
	}
	c.mu.Lock()
	c.status = st
	c.mu.Unlock()
	return st
}

func (c *Client) manifestDigest(ctx context.Context) (string, bool) {
	if c.legacy {
		return "", false
	}
	ctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	m, err := acplugv1.NewPluginClient(c.conn).Manifest(ctx, c.request)
	if err != nil {
		return "", false
	}
	return Digest(m), true
}

// Close releases the connection.
func (c *Client) Close() error { return c.close() }

func (c *Client) check(ctx context.Context) (acplugv1.HealthResponse_Status, error) {
	ctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	resp := &acplugv1.HealthResponse{}
	if err := c.conn.Invoke(ctx, c.health, &acplugv1.HealthRequest{}, resp); err != nil {
		return acplugv1.HealthResponse_STATUS_UNSPECIFIED, fmt.Errorf("health: %w", err)
	}
	if resp.GetStatus() != acplugv1.HealthResponse_STATUS_SERVING {
		return resp.GetStatus(), fmt.Errorf("not serving: %s %s", resp.GetStatus(), resp.GetDetail())
	}
	return resp.GetStatus(), nil
}
