package meshconfig

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GVK is the GroupVersionKind of the MeshConfig custom resource.
func GVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: Group, Version: Version, Kind: Kind}
}

// NewUnstructured returns an empty unstructured object stamped with the
// MeshConfig GVK, ready for client Get/Watch.
func NewUnstructured() *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(GVK())
	return u
}
