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
	attrCluster      = "AETHER_CLUSTER"
	attrProtocol     = "AETHER_PROTOCOL"
	attrWeight       = "AETHER_WEIGHT"
	attrRegion       = "AETHER_REGION"
	attrZone         = "AETHER_ZONE"
	attrK8sNamespace = "AETHER_K8S_NAMESPACE"
	attrK8sPod       = "AETHER_K8S_POD"
	attrK8sNode      = "AETHER_K8S_NODE"
	attrContainerID  = "AETHER_CONTAINER_ID"
	attrNetworkNS    = "AETHER_NETWORK_NS"
	attrMetadata     = "AETHER_METADATA"
	attrController   = "AETHER_CONTROLLER"

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

	return ep, nil
}

// unmarshalEndpointFromSummary converts an HttpInstanceSummary into a ServiceEndpoint.
func unmarshalEndpointFromSummary(summary types.HttpInstanceSummary) (*registryv1.ServiceEndpoint, error) {
	return unmarshalEndpoint(summary.Attributes)
}

// protocolFromAttrs extracts the protocol from instance attributes.
func protocolFromAttrs(attrs map[string]string) registryv1.Service_Protocol {
	if val, ok := registryv1.Service_Protocol_value[attrs[attrProtocol]]; ok {
		return registryv1.Service_Protocol(val)
	}
	return registryv1.Service_PROTOCOL_UNSPECIFIED
}
