package envoy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseConfigDumpForNetns(t *testing.T) {
	tests := []struct {
		name    string
		data    string
		netns   string
		want    bool
		wantErr bool
	}{
		{
			name:  "finds matching netns",
			netns: "/proc/100/ns/net",
			data: `{
				"configs": [{
					"dynamic_listeners": [{
						"active_state": {
							"listener": {
								"address": {
									"socket_address": {
										"network_namespace_filepath": "/proc/100/ns/net"
									}
								}
							}
						}
					}]
				}]
			}`,
			want: true,
		},
		{
			name:  "no matching netns",
			netns: "/proc/999/ns/net",
			data: `{
				"configs": [{
					"dynamic_listeners": [{
						"active_state": {
							"listener": {
								"address": {
									"socket_address": {
										"network_namespace_filepath": "/proc/100/ns/net"
									}
								}
							}
						}
					}]
				}]
			}`,
			want: false,
		},
		{
			name:  "empty dynamic listeners",
			netns: "/proc/100/ns/net",
			data:  `{"configs": [{"dynamic_listeners": []}]}`,
			want:  false,
		},
		{
			name:  "nil active state is skipped",
			netns: "/proc/100/ns/net",
			data:  `{"configs": [{"dynamic_listeners": [{}]}]}`,
			want:  false,
		},
		{
			name:    "invalid JSON returns error",
			netns:   "/proc/100/ns/net",
			data:    `not json`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseConfigDumpForNetns([]byte(tt.data), tt.netns)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestWaitForListenerPresent(t *testing.T) {
	t.Run("returns immediately when listener present", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{
				"configs": [{
					"dynamic_listeners": [{
						"active_state": {
							"listener": {
								"address": {
									"socket_address": {
										"network_namespace_filepath": "/proc/100/ns/net"
									}
								}
							}
						}
					}]
				}]
			}`))
		}))
		defer srv.Close()

		client := NewAdminClient(srv.Listener.Addr().String())
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		err := client.WaitForListenerPresent(ctx, "/proc/100/ns/net")
		assert.NoError(t, err)
	})

	t.Run("times out when listener never appears", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"configs": [{"dynamic_listeners": []}]}`))
		}))
		defer srv.Close()

		client := NewAdminClient(srv.Listener.Addr().String())
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()

		err := client.WaitForListenerPresent(ctx, "/proc/100/ns/net")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "timed out")
	})
}

func TestWaitForListenerRemoval(t *testing.T) {
	t.Run("returns immediately when listener absent", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"configs": [{"dynamic_listeners": []}]}`))
		}))
		defer srv.Close()

		client := NewAdminClient(srv.Listener.Addr().String())
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		err := client.WaitForListenerRemoval(ctx, "/proc/100/ns/net")
		assert.NoError(t, err)
	})

	t.Run("times out when listener never removed", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{
				"configs": [{
					"dynamic_listeners": [{
						"active_state": {
							"listener": {
								"address": {
									"socket_address": {
										"network_namespace_filepath": "/proc/100/ns/net"
									}
								}
							}
						}
					}]
				}]
			}`))
		}))
		defer srv.Close()

		client := NewAdminClient(srv.Listener.Addr().String())
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()

		err := client.WaitForListenerRemoval(ctx, "/proc/100/ns/net")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "timed out")
	})
}
