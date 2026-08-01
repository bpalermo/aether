package v1

import (
	configv1 "github.com/bpalermo/aether/api/aether/config/v1"
	"google.golang.org/protobuf/proto"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// DeepCopyInto copies the receiver into out. The proto spec is cloned via
// proto.Clone (never shallow-copied — proto messages hold internal state).
func (in *EndpointPolicy) DeepCopyInto(out *EndpointPolicy) {
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	if in.Spec != nil {
		out.Spec = proto.Clone(in.Spec).(*configv1.EndpointPolicySpec)
	} else {
		out.Spec = nil
	}
	in.Status.DeepCopyInto(&out.Status)
}

// DeepCopy returns a deep copy of the EndpointPolicy.
func (in *EndpointPolicy) DeepCopy() *EndpointPolicy {
	if in == nil {
		return nil
	}
	out := new(EndpointPolicy)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject implements runtime.Object.
func (in *EndpointPolicy) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopyInto copies the status into out.
func (in *EndpointPolicyStatus) DeepCopyInto(out *EndpointPolicyStatus) {
	*out = *in
	if in.Conditions != nil {
		out.Conditions = make([]metav1.Condition, len(in.Conditions))
		for i := range in.Conditions {
			in.Conditions[i].DeepCopyInto(&out.Conditions[i])
		}
	}
}

// DeepCopyInto copies the list into out.
func (in *EndpointPolicyList) DeepCopyInto(out *EndpointPolicyList) {
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]EndpointPolicy, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

// DeepCopy returns a deep copy of the EndpointPolicyList.
func (in *EndpointPolicyList) DeepCopy() *EndpointPolicyList {
	if in == nil {
		return nil
	}
	out := new(EndpointPolicyList)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject implements runtime.Object.
func (in *EndpointPolicyList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}
