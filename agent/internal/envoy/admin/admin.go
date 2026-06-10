// Package admin provides a client for interacting with the Envoy proxy admin interface.
package admin

import (
	"net/http"
	"time"
)

// Client communicates with the Envoy admin interface. Admin requests are
// served on Envoy's main thread, so usage is kept to the minimum: the liveness
// loop's /clusters health scrape (until it moves to a worker-thread
// health_check listener). Listener apply/removal confirmation uses delta-xDS
// ACK tracking and the CNI plugin's in-netns readiness probe instead.
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
