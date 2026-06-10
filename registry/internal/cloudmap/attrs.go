// Package cloudmap implements the Registry interface using AWS Cloud Map as the backend.
// It maps service endpoints to Cloud Map instances within an HTTP namespace,
// inspired by the poll-based discovery approach of the AWS Cloud Map MCS Controller.
package cloudmap

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/aws/aws-sdk-go-v2/service/servicediscovery/types"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
)

const (
	// Standard Cloud Map attributes
	attrIPv4 = "AWS_INSTANCE_IPV4"
	attrPort = "AWS_INSTANCE_PORT"

	// Aether-specific attributes
	attrCluster         = "AETHER_CLUSTER"
	attrProtocol        = "AETHER_PROTOCOL"
	attrWeight          = "AETHER_WEIGHT"
	attrRegion          = "AETHER_REGION"
	attrZone            = "AETHER_ZONE"
	attrK8sNamespace    = "AETHER_K8S_NAMESPACE"
	attrK8sPod          = "AETHER_K8S_POD"
	attrK8sNode         = "AETHER_K8S_NODE"
	attrHealthCheckMode = "AETHER_HEALTH_CHECK_MODE"
	// attrDraining refines Cloud Map's binary instance health: a draining
	// endpoint is natively UNHEALTHY (excluded from selection) and this marker
	// upgrades it to EDS DRAINING (established connections finish gracefully).
	// The health verdict itself lives in Cloud Map's native per-instance
	// status, never in an attribute.
	attrDraining = "AETHER_DRAINING"
	// attrInitHealth is Cloud Map's reserved attribute seeding a brand-new
	// instance's custom health status at registration.
	attrInitHealth  = "AWS_INIT_HEALTH_STATUS"
	attrContainerID = "AETHER_CONTAINER_ID"
	attrNetworkNS   = "AETHER_NETWORK_NS"
	attrMetadata    = "AETHER_METADATA"
	attrController  = "AETHER_CONTROLLER"

	// controllerName identifies instances managed by Aether.
	controllerName = "aether"
)

// instanceID builds a deterministic Cloud Map instance ID from cluster name and IP.
// This makes register/deregister idempotent without requiring ID lookups.
func instanceID(clusterName, ip string) string {
	return fmt.Sprintf("%s/%s", clusterName, ip)
}

// marshalAttrs converts a ServiceEndpoint and protocol into Cloud Map instance attributes.
func marshalAttrs(protocol registryv1.Service_Protocol, ep *registryv1.ServiceEndpoint) map[string]string {
	attrs := map[string]string{
		attrIPv4:       ep.GetIp(),
		attrPort:       strconv.FormatUint(uint64(ep.GetPort()), 10),
		attrCluster:    ep.GetClusterName(),
		attrProtocol:   protocol.String(),
		attrWeight:     strconv.FormatUint(uint64(ep.GetWeight()), 10),
		attrController: controllerName,
	}

	if loc := ep.GetLocality(); loc != nil {
		if loc.GetRegion() != "" {
			attrs[attrRegion] = loc.GetRegion()
		}
		if loc.GetZone() != "" {
			attrs[attrZone] = loc.GetZone()
		}
	}

	if km := ep.GetKubernetesMetadata(); km != nil {
		if km.GetNamespace() != "" {
			attrs[attrK8sNamespace] = km.GetNamespace()
		}
		if km.GetPodName() != "" {
			attrs[attrK8sPod] = km.GetPodName()
		}
		if km.GetNodeName() != "" {
			attrs[attrK8sNode] = km.GetNodeName()
		}
	}

	if cm := ep.GetContainerMetadata(); cm != nil {
		if cm.GetContainerId() != "" {
			attrs[attrContainerID] = cm.GetContainerId()
		}
		if cm.GetNetworkNamespace() != "" {
			attrs[attrNetworkNS] = cm.GetNetworkNamespace()
		}
	}

	if md := ep.GetMetadata(); len(md) > 0 {
		if data, err := json.Marshal(md); err == nil {
			attrs[attrMetadata] = string(data)
		}
	}

	// Health is native: seed new instances via AWS_INIT_HEALTH_STATUS (updates
	// go through UpdateInstanceCustomHealthStatus). DRAINING maps to natively
	// UNHEALTHY plus the draining marker.
	attrs[attrInitHealth] = string(initHealthStatus(ep.GetHealth()))
	if ep.GetHealth() == registryv1.ServiceEndpoint_HEALTH_DRAINING {
		attrs[attrDraining] = "true"
	}

	// Only persist an explicit mode; absence means the default (active).
	if m := ep.GetHealthCheckMode(); m != registryv1.ServiceEndpoint_HEALTH_CHECK_MODE_UNSPECIFIED {
		attrs[attrHealthCheckMode] = m.String()
	}

	return attrs
}

// unmarshalEndpoint converts Cloud Map instance attributes into a ServiceEndpoint.
func unmarshalEndpoint(attrs map[string]string) (*registryv1.ServiceEndpoint, error) {
	ip := attrs[attrIPv4]
	if ip == "" {
		return nil, fmt.Errorf("instance missing %s attribute", attrIPv4)
	}

	port, _ := strconv.ParseUint(attrs[attrPort], 10, 32)
	weight, _ := strconv.ParseUint(attrs[attrWeight], 10, 32)

	ep := &registryv1.ServiceEndpoint{
		Ip:          ip,
		ClusterName: attrs[attrCluster],
		Port:        uint32(port),
		Weight:      uint32(weight),
	}

	region, zone := attrs[attrRegion], attrs[attrZone]
	if region != "" || zone != "" {
		ep.Locality = &registryv1.ServiceEndpoint_Locality{
			Region: region,
			Zone:   zone,
		}
	}

	k8sNs, k8sPod, k8sNode := attrs[attrK8sNamespace], attrs[attrK8sPod], attrs[attrK8sNode]
	if k8sNs != "" || k8sPod != "" || k8sNode != "" {
		ep.KubernetesMetadata = &registryv1.ServiceEndpoint_KubernetesMetadata{
			Namespace: k8sNs,
			PodName:   k8sPod,
			NodeName:  k8sNode,
		}
	}

	containerID, netNS := attrs[attrContainerID], attrs[attrNetworkNS]
	if containerID != "" || netNS != "" {
		ep.ContainerMetadata = &registryv1.ServiceEndpoint_ContainerMetadata{
			ContainerId:      containerID,
			NetworkNamespace: netNS,
		}
	}

	if mdStr := attrs[attrMetadata]; mdStr != "" {
		var md map[string]string
		if err := json.Unmarshal([]byte(mdStr), &md); err == nil {
			ep.Metadata = md
		}
	}

	if m, ok := registryv1.ServiceEndpoint_HealthCheckMode_value[attrs[attrHealthCheckMode]]; ok {
		ep.HealthCheckMode = registryv1.ServiceEndpoint_HealthCheckMode(m)
	}

	return ep, nil
}

// unmarshalEndpointFromSummary converts an HttpInstanceSummary into a
// ServiceEndpoint. Health comes from Cloud Map's native per-instance status
// (set via custom health checks), refined to DRAINING by the draining marker;
// an unknown status maps to UNSPECIFIED (treated healthy, matching older
// instances).
func unmarshalEndpointFromSummary(summary types.HttpInstanceSummary) (*registryv1.ServiceEndpoint, error) {
	ep, err := unmarshalEndpoint(summary.Attributes)
	if err != nil {
		return nil, err
	}
	switch summary.HealthStatus {
	case types.HealthStatusHealthy:
		ep.Health = registryv1.ServiceEndpoint_HEALTH_HEALTHY
	case types.HealthStatusUnhealthy:
		ep.Health = registryv1.ServiceEndpoint_HEALTH_UNHEALTHY
		if summary.Attributes[attrDraining] == "true" {
			ep.Health = registryv1.ServiceEndpoint_HEALTH_DRAINING
		}
	default:
		ep.Health = registryv1.ServiceEndpoint_HEALTH_UNSPECIFIED
	}
	return ep, nil
}

// initHealthStatus maps a registry health to the AWS_INIT_HEALTH_STATUS seed.
func initHealthStatus(h registryv1.ServiceEndpoint_Health) types.CustomHealthStatus {
	if h == registryv1.ServiceEndpoint_HEALTH_UNHEALTHY || h == registryv1.ServiceEndpoint_HEALTH_DRAINING {
		return types.CustomHealthStatusUnhealthy
	}
	return types.CustomHealthStatusHealthy
}

// customHealthStatus maps a registry health to the native custom health update.
func customHealthStatus(h registryv1.ServiceEndpoint_Health) types.CustomHealthStatus {
	return initHealthStatus(h)
}

// protocolFromAttrs extracts the protocol from instance attributes.
func protocolFromAttrs(attrs map[string]string) registryv1.Service_Protocol {
	if val, ok := registryv1.Service_Protocol_value[attrs[attrProtocol]]; ok {
		return registryv1.Service_Protocol(val)
	}
	return registryv1.Service_PROTOCOL_UNSPECIFIED
}
