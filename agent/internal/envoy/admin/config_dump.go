package admin

import (
	"fmt"

	adminv3 "github.com/envoyproxy/go-control-plane/envoy/admin/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	"google.golang.org/protobuf/encoding/protojson"
)

func parseConfigDumpForNetns(data []byte, netns string) (bool, error) {
	var dump adminv3.ConfigDump
	if err := protojson.Unmarshal(data, &dump); err != nil {
		return false, fmt.Errorf("failed to parse config dump: %w", err)
	}

	for _, anyConfig := range dump.GetConfigs() {
		msg, err := anyConfig.UnmarshalNew()
		if err != nil {
			continue
		}
		listenersDump, ok := msg.(*adminv3.ListenersConfigDump)
		if !ok {
			continue
		}
		for _, dl := range listenersDump.GetDynamicListeners() {
			if dl.GetActiveState() == nil || dl.GetActiveState().GetListener() == nil {
				continue
			}
			listenerMsg, err := dl.GetActiveState().GetListener().UnmarshalNew()
			if err != nil {
				continue
			}
			listener, ok := listenerMsg.(*listenerv3.Listener)
			if !ok {
				continue
			}
			sa := listener.GetAddress().GetSocketAddress()
			if sa != nil && sa.GetNetworkNamespaceFilepath() == netns {
				return true, nil
			}
		}
	}

	return false, nil
}
