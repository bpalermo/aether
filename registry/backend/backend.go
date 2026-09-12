// Package backend is the registry backend factory: it owns the mapping from a
// --registry-backend name ("kubernetes" or "etcd") to a concrete registry.Registry
// implementation. It is the ONLY package that links every backend; consumers of
// the registry.Registry interface (and the agent, which speaks only to the
// in-cluster registrar via registry/registrarclient) do not transitively pull in
// the etcd client.
package backend

import (
	"context"
	"fmt"
	"log/slog"

	"aethermesh.dev/registry"
	"aethermesh.dev/registry/internal/etcd"
	"aethermesh.dev/registry/internal/k8s"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Config carries the union of per-backend settings for New. Only the fields the
// selected backend needs are read; the rest are ignored.
type Config struct {
	// ClusterName is the Kubernetes cluster name (kubernetes + etcd backends).
	ClusterName string
	// Reader reads pods/nodes for the kubernetes backend. It should be a direct
	// API reader (e.g., manager.GetAPIReader()) to avoid cache timing issues.
	Reader client.Reader
	// EtcdEndpoints is the etcd client endpoint list (etcd backend).
	EtcdEndpoints []string
	// Region is the region owning this etcd partition (etcd backend, proposal 006).
	Region string
}

// New constructs the registry.Registry implementation selected by name. The
// caller owns the returned registry's lifecycle (Initialize/Close).
func New(_ context.Context, log *slog.Logger, name string, cfg Config) (registry.Registry, error) {
	switch name {
	case "kubernetes":
		return k8s.NewKubernetesRegistry(log, cfg.Reader, k8s.Config{
			ClusterName: cfg.ClusterName,
		}), nil
	case "etcd":
		return etcd.NewEtcdRegistry(log, etcd.Config{
			Endpoints: cfg.EtcdEndpoints,
			Region:    cfg.Region,
			Cluster:   cfg.ClusterName,
		}), nil
	default:
		return nil, fmt.Errorf("unsupported registry backend: %s (supported: kubernetes, etcd)", name)
	}
}
