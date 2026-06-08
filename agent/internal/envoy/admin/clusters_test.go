package admin

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAppClusterHealth(t *testing.T) {
	// Two app clusters (one failing its HC, one passing) and a non-app cluster.
	const body = `{"cluster_statuses":[
		{"name":"app_pod-a","host_statuses":[{"health_status":{"failed_active_health_check":false}}]},
		{"name":"app_pod-b","host_statuses":[{"health_status":{"failed_active_health_check":true}}]},
		{"name":"echo","host_statuses":[{"health_status":{"failed_active_health_check":true}}]}
	]}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/clusters", r.URL.Path)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c := NewClient(srv.Listener.Addr().String())
	health, err := c.AppClusterHealth(t.Context())
	require.NoError(t, err)

	assert.True(t, health["app_pod-a"], "pod-a passes its HC")
	assert.False(t, health["app_pod-b"], "pod-b fails its HC")
	_, ok := health["echo"]
	assert.False(t, ok, "non-app clusters are excluded")
}
