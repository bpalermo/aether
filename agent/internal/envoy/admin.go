// Package envoy provides a client for interacting with the Envoy proxy admin interface.
package envoy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	pollInterval = 100 * time.Millisecond
)

// AdminClient communicates with the Envoy admin interface to query
// listener configuration state.
type AdminClient struct {
	address    string
	httpClient *http.Client
}

// NewAdminClient creates a new AdminClient that connects to the Envoy admin
// interface at the given address (host:port).
func NewAdminClient(address string) *AdminClient {
	return &AdminClient{
		address: address,
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
}

// WaitForListenerPresent polls the Envoy config dump until a dynamic listener
// matching the given network namespace appears, or the context deadline expires.
// This is best-effort: a timeout returns an error but callers should treat it
// as non-fatal.
func (c *AdminClient) WaitForListenerPresent(ctx context.Context, netns string) error {
	return c.poll(ctx, netns, true)
}

// WaitForListenerRemoval polls the Envoy config dump until no dynamic listener
// matching the given network namespace remains, or the context deadline expires.
// This is best-effort: a timeout returns an error but callers should treat it
// as non-fatal.
func (c *AdminClient) WaitForListenerRemoval(ctx context.Context, netns string) error {
	return c.poll(ctx, netns, false)
}

func (c *AdminClient) poll(ctx context.Context, netns string, wantPresent bool) error {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		found, err := c.hasListenerForNetns(ctx, netns)
		if err == nil {
			if wantPresent && found {
				return nil
			}
			if !wantPresent && !found {
				return nil
			}
		}

		select {
		case <-ctx.Done():
			if wantPresent {
				return fmt.Errorf("timed out waiting for listener with netns %s to appear", netns)
			}
			return fmt.Errorf("timed out waiting for listener with netns %s to be removed", netns)
		case <-ticker.C:
		}
	}
}

func (c *AdminClient) hasListenerForNetns(ctx context.Context, netns string) (bool, error) {
	url := fmt.Sprintf("http://%s/config_dump?resource=dynamic_listeners&name_regex=inbound_http", c.address)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("failed to query envoy admin: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("envoy admin returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, fmt.Errorf("failed to read response body: %w", err)
	}

	return parseConfigDumpForNetns(body, netns)
}

// configDump represents the relevant structure of the Envoy config_dump response
// for dynamic listeners.
type configDump struct {
	Configs []configEntry `json:"configs"`
}

type configEntry struct {
	DynamicListeners []dynamicListener `json:"dynamic_listeners"`
}

type dynamicListener struct {
	ActiveState *activeState `json:"active_state"`
}

type activeState struct {
	Listener listenerConfig `json:"listener"`
}

type listenerConfig struct {
	Address *addressConfig `json:"address"`
}

type addressConfig struct {
	SocketAddress *socketAddress `json:"socket_address"`
}

type socketAddress struct {
	NetworkNamespaceFilepath string `json:"network_namespace_filepath"`
}

func parseConfigDumpForNetns(data []byte, netns string) (bool, error) {
	var dump configDump
	if err := json.Unmarshal(data, &dump); err != nil {
		return false, fmt.Errorf("failed to parse config dump: %w", err)
	}

	for _, cfg := range dump.Configs {
		for _, dl := range cfg.DynamicListeners {
			if dl.ActiveState == nil {
				continue
			}
			addr := dl.ActiveState.Listener.Address
			if addr == nil || addr.SocketAddress == nil {
				continue
			}
			if addr.SocketAddress.NetworkNamespaceFilepath == netns {
				return true, nil
			}
		}
	}

	return false, nil
}
