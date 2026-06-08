// Package admin provides a client for interacting with the Envoy proxy admin interface.
package admin

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	pollInterval = 100 * time.Millisecond
)

// Client communicates with the Envoy admin interface to query
// listener configuration state.
type Client struct {
	address    string
	httpClient *http.Client
}

// NewClient creates a new Client that connects to the Envoy admin
// interface at the given address (host:port).
func NewClient(address string) *Client {
	return &Client{
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
func (c *Client) WaitForListenerPresent(ctx context.Context, netns string) error {
	return c.poll(ctx, netns, true)
}

// WaitForListenerRemoval polls the Envoy config dump until no dynamic listener
// matching the given network namespace remains, or the context deadline expires.
// This is best-effort: a timeout returns an error but callers should treat it
// as non-fatal.
func (c *Client) WaitForListenerRemoval(ctx context.Context, netns string) error {
	return c.poll(ctx, netns, false)
}

func (c *Client) poll(ctx context.Context, netns string, wantPresent bool) error {
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

func (c *Client) hasListenerForNetns(ctx context.Context, netns string) (bool, error) {
	url := fmt.Sprintf("http://%s/config_dump?resource=dynamic_listeners&name_regex=outbound_http", c.address)

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
