package admin

import (
	"encoding/json"
	"fmt"
)

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
