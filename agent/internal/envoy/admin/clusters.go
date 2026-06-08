package admin

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	adminv3 "github.com/envoyproxy/go-control-plane/envoy/admin/v3"
	"google.golang.org/protobuf/encoding/protojson"
)

// appClusterPrefix marks the per-pod application clusters whose active health
// check reflects the pod's app readiness (delegated liveness).
const appClusterPrefix = "app_"

// AppClusterHealth queries the Envoy admin /clusters endpoint and returns, for
// each per-pod application cluster (app_<pod>), whether its host currently passes
// the active health check. The map is keyed by the app cluster name. A cluster
// whose host has not yet been health-checked is reported healthy (optimistic) so
// freshly added pods are not transiently marked down.
func (c *Client) AppClusterHealth(ctx context.Context) (map[string]bool, error) {
	url := fmt.Sprintf("http://%s/clusters?format=json", c.address)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to query envoy admin clusters: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("envoy admin returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	var clusters adminv3.Clusters
	if err := protojson.Unmarshal(body, &clusters); err != nil {
		return nil, fmt.Errorf("failed to parse clusters: %w", err)
	}

	health := make(map[string]bool)
	for _, cs := range clusters.GetClusterStatuses() {
		if !strings.HasPrefix(cs.GetName(), appClusterPrefix) {
			continue
		}
		health[cs.GetName()] = appHostsHealthy(cs)
	}
	return health, nil
}

// appHostsHealthy reports whether the app cluster's host passes the active health
// check. With no hosts reported yet, it is optimistically healthy.
func appHostsHealthy(cs *adminv3.ClusterStatus) bool {
	for _, h := range cs.GetHostStatuses() {
		if h.GetHealthStatus().GetFailedActiveHealthCheck() {
			return false
		}
	}
	return true
}
