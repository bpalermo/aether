package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	adminv3 "github.com/envoyproxy/go-control-plane/envoy/admin/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/anypb"
)

// marshalConfigDump builds a ConfigDump with the given listeners and returns its JSON representation.
func marshalConfigDump(t *testing.T, listeners ...*listenerv3.Listener) []byte {
	t.Helper()

	var dynamicListeners []*adminv3.ListenersConfigDump_DynamicListener
	for _, l := range listeners {
		anyListener, err := anypb.New(l)
		require.NoError(t, err)
		dynamicListeners = append(dynamicListeners, &adminv3.ListenersConfigDump_DynamicListener{
			Name: l.GetName(),
			ActiveState: &adminv3.ListenersConfigDump_DynamicListenerState{
				Listener: anyListener,
			},
		})
	}

	listenersDump := &adminv3.ListenersConfigDump{
		DynamicListeners: dynamicListeners,
	}
	anyListenersDump, err := anypb.New(listenersDump)
	require.NoError(t, err)

	dump := &adminv3.ConfigDump{
		Configs: []*anypb.Any{anyListenersDump},
	}
	data, err := protojson.Marshal(dump)
	require.NoError(t, err)
	return data
}

// listenerWithNetns builds a minimal Listener with the given network namespace.
func listenerWithNetns(name, netns string) *listenerv3.Listener {
	return &listenerv3.Listener{
		Name: name,
		Address: &corev3.Address{
			Address: &corev3.Address_SocketAddress{
				SocketAddress: &corev3.SocketAddress{
					NetworkNamespaceFilepath: netns,
				},
			},
		},
	}
}

func TestParseConfigDumpForNetns(t *testing.T) {
	tests := []struct {
		name    string
		data    func(t *testing.T) []byte
		netns   string
		want    bool
		wantErr bool
	}{
		{
			name:  "finds matching netns",
			netns: "/proc/100/ns/net",
			data: func(t *testing.T) []byte {
				return marshalConfigDump(t, listenerWithNetns("outbound_http", "/proc/100/ns/net"))
			},
			want: true,
		},
		{
			name:  "no matching netns",
			netns: "/proc/999/ns/net",
			data: func(t *testing.T) []byte {
				return marshalConfigDump(t, listenerWithNetns("outbound_http", "/proc/100/ns/net"))
			},
			want: false,
		},
		{
			name:  "empty dynamic listeners",
			netns: "/proc/100/ns/net",
			data: func(t *testing.T) []byte {
				return marshalConfigDump(t)
			},
			want: false,
		},
		{
			name:    "invalid JSON returns error",
			netns:   "/proc/100/ns/net",
			data:    func(_ *testing.T) []byte { return []byte(`not json`) },
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseConfigDumpForNetns(tt.data(t), tt.netns)
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
		body := marshalConfigDump(t, listenerWithNetns("outbound_http", "/proc/100/ns/net"))
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
		}))
		defer srv.Close()

		client := NewClient(srv.Listener.Addr().String())
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		err := client.WaitForListenerPresent(ctx, "/proc/100/ns/net")
		assert.NoError(t, err)
	})

	t.Run("times out when listener never appears", func(t *testing.T) {
		body := marshalConfigDump(t)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
		}))
		defer srv.Close()

		client := NewClient(srv.Listener.Addr().String())
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()

		err := client.WaitForListenerPresent(ctx, "/proc/100/ns/net")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "timed out")
	})
}

func TestWaitForListenerRemoval(t *testing.T) {
	t.Run("returns immediately when listener absent", func(t *testing.T) {
		body := marshalConfigDump(t)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
		}))
		defer srv.Close()

		client := NewClient(srv.Listener.Addr().String())
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		err := client.WaitForListenerRemoval(ctx, "/proc/100/ns/net")
		assert.NoError(t, err)
	})

	t.Run("times out when listener never removed", func(t *testing.T) {
		body := marshalConfigDump(t, listenerWithNetns("outbound_http", "/proc/100/ns/net"))
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
		}))
		defer srv.Close()

		client := NewClient(srv.Listener.Addr().String())
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()

		err := client.WaitForListenerRemoval(ctx, "/proc/100/ns/net")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "timed out")
	})
}
