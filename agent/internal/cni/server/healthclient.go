package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/bpalermo/aether/agent/internal/xds/proxy"
)

// healthGatewayClient probes the proxy's health gateway listener (a Unix
// domain socket programmed via LDS) for per-pod app health. Requests are
// answered by health_check filters on Envoy worker threads from the same
// host-health state the load balancer uses — the admin interface (main
// thread) is not involved.
type healthGatewayClient struct {
	client *http.Client
}

// newHealthGatewayClient builds a client for the gateway at socketPath.
// Connections are kept alive across probes and ticks.
func newHealthGatewayClient(socketPath string) *healthGatewayClient {
	return &healthGatewayClient{
		client: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", socketPath)
				},
			},
			Timeout: 2 * time.Second,
		},
	}
}

// appHealth reports the active-HC health of one health-probe cluster:
//
//	200 → (healthy, known)     the pod's host passes its health check
//	503 → (unhealthy, known)   it fails (includes HC warm-up; caller grace)
//	404 → (_, not known)       the pod's gateway filter is not programmed yet
//
// Any other status or a transport error is returned as an error (the gateway
// itself is unreachable or misbehaving — the caller aborts the tick).
func (c *healthGatewayClient) appHealth(ctx context.Context, probeCluster string) (healthy, known bool, err error) {
	// The authority is irrelevant for a UDS dial; "health-gateway" only labels it.
	url := "http://health-gateway" + proxy.HealthGatewayPath(probeCluster)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, false, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return false, false, fmt.Errorf("failed to query health gateway: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		return true, true, nil
	case http.StatusServiceUnavailable:
		return false, true, nil
	case http.StatusNotFound:
		return false, false, nil
	default:
		return false, false, fmt.Errorf("health gateway returned status %d for %s", resp.StatusCode, probeCluster)
	}
}
