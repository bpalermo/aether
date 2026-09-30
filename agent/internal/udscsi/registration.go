package udscsi

import (
	"context"
	"fmt"
	"log/slog"

	pluginregistrationv1 "aethermesh.dev/api/aether/kubelet/pluginregistration/v1"
)

const (
	// pluginTypeCSI is the kubelet plugin watcher's type for a CSI driver
	// (k8s.io/kubelet/pkg/apis/pluginregistration/v1.CSIPlugin).
	pluginTypeCSI = "CSIPlugin"
	// csiVersion is the CSI spec version the kubelet should speak to us.
	csiVersion = "1.0.0"
)

// RegistrationServer is the kubelet plugin-registration service, served by the
// node plugin itself on <kubelet-root>/plugins_registry/csi.aether.io-reg.sock.
// It replaces the csi-node-driver-registrar sidecar every other CSI driver
// ships: that sidecar's whole job is these two RPCs, and serving them here
// removes a third-party image, its supply chain and a second container whose
// liveness had to be kept in step with this one.
//
// The kubelet's plugin watcher sees the socket appear, calls GetInfo to learn
// the driver name and where its CSI endpoint is, validates it (by calling the
// CSI endpoint's NodeGetInfo), and reports the verdict through
// NotifyRegistrationStatus.
type RegistrationServer struct {
	pluginregistrationv1.UnimplementedRegistrationServer

	// Endpoint is the CSI socket path AS THE KUBELET SEES IT.
	Endpoint string
	Log      *slog.Logger
	// OnFailure is called when the kubelet reports that registration failed.
	// A plugin the kubelet refused serves nothing, so the caller exits
	// non-zero and lets the kubelet's restart backoff try again (with the
	// reason in the pod's last termination state) rather than sit Running
	// with the driver absent from the node's CSINode.
	OnFailure func(error)
}

// GetInfo tells the kubelet who we are and where the CSI endpoint is.
func (s *RegistrationServer) GetInfo(context.Context, *pluginregistrationv1.InfoRequest) (*pluginregistrationv1.PluginInfo, error) {
	s.Log.Info("kubelet requested plugin info", "endpoint", s.Endpoint)
	return &pluginregistrationv1.PluginInfo{
		Type:              pluginTypeCSI,
		Name:              DriverName,
		Endpoint:          s.Endpoint,
		SupportedVersions: []string{csiVersion},
	}, nil
}

// NotifyRegistrationStatus receives the kubelet's verdict.
func (s *RegistrationServer) NotifyRegistrationStatus(_ context.Context, st *pluginregistrationv1.RegistrationStatus) (*pluginregistrationv1.RegistrationStatusResponse, error) {
	if st.GetPluginRegistered() {
		s.Log.Info("kubelet registered the driver", "driver", DriverName)
		return &pluginregistrationv1.RegistrationStatusResponse{}, nil
	}
	err := fmt.Errorf("the kubelet refused to register %s: %s", DriverName, st.GetError())
	s.Log.Error("registration failed; exiting so the kubelet restarts the plugin", "error", err)
	if s.OnFailure != nil {
		s.OnFailure(err)
	}
	return &pluginregistrationv1.RegistrationStatusResponse{}, nil
}
