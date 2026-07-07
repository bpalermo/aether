package v1

import (
	configv1 "github.com/bpalermo/aether/api/aether/config/v1"
	"google.golang.org/protobuf/proto"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// DeepCopyInto copies the receiver into out. The proto spec is cloned via proto.Clone
// (never shallow-copied — proto messages, and the opaque typedConfig Any, hold
// internal state).
func (in *EdgeConfig) DeepCopyInto(out *EdgeConfig) {
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	if in.Spec != nil {
		out.Spec = proto.Clone(in.Spec).(*configv1.EdgeConfigSpec)
	} else {
		out.Spec = nil
	}
	in.Status.DeepCopyInto(&out.Status)
}

// DeepCopy returns a deep copy of the EdgeConfig.
func (in *EdgeConfig) DeepCopy() *EdgeConfig {
	if in == nil {
		return nil
	}
	out := new(EdgeConfig)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject implements runtime.Object.
func (in *EdgeConfig) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopyInto copies the status into out.
func (in *EdgeConfigStatus) DeepCopyInto(out *EdgeConfigStatus) {
	*out = *in
	if in.Conditions != nil {
		out.Conditions = make([]metav1.Condition, len(in.Conditions))
		for i := range in.Conditions {
			in.Conditions[i].DeepCopyInto(&out.Conditions[i])
		}
	}
}

// DeepCopyInto copies the list into out.
func (in *EdgeConfigList) DeepCopyInto(out *EdgeConfigList) {
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]EdgeConfig, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

// DeepCopy returns a deep copy of the EdgeConfigList.
func (in *EdgeConfigList) DeepCopy() *EdgeConfigList {
	if in == nil {
		return nil
	}
	out := new(EdgeConfigList)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject implements runtime.Object.
func (in *EdgeConfigList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}
