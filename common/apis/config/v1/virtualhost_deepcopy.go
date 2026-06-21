package v1

import (
	configv1 "github.com/bpalermo/aether/api/aether/config/v1"
	"google.golang.org/protobuf/proto"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// DeepCopyInto copies the receiver into out. The proto spec is cloned via
// proto.Clone (never shallow-copied — proto messages hold internal state).
func (in *VirtualHost) DeepCopyInto(out *VirtualHost) {
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	if in.Spec != nil {
		out.Spec = proto.Clone(in.Spec).(*configv1.VirtualHostSpec)
	} else {
		out.Spec = nil
	}
	in.Status.DeepCopyInto(&out.Status)
}

// DeepCopy returns a deep copy of the VirtualHost.
func (in *VirtualHost) DeepCopy() *VirtualHost {
	if in == nil {
		return nil
	}
	out := new(VirtualHost)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject implements runtime.Object.
func (in *VirtualHost) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopyInto copies the status into out.
func (in *VirtualHostStatus) DeepCopyInto(out *VirtualHostStatus) {
	*out = *in
	if in.Conditions != nil {
		out.Conditions = make([]metav1.Condition, len(in.Conditions))
		for i := range in.Conditions {
			in.Conditions[i].DeepCopyInto(&out.Conditions[i])
		}
	}
}

// DeepCopyInto copies the list into out.
func (in *VirtualHostList) DeepCopyInto(out *VirtualHostList) {
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]VirtualHost, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

// DeepCopy returns a deep copy of the VirtualHostList.
func (in *VirtualHostList) DeepCopy() *VirtualHostList {
	if in == nil {
		return nil
	}
	out := new(VirtualHostList)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject implements runtime.Object.
func (in *VirtualHostList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}
