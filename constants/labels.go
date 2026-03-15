package constants

const (
	aetherLabelPrefix = "aether.io"

	// LabelAetherManaged is the Kubernetes label used to opt pods into the Aether
	// service mesh. The value must be "true" for the pod to be managed.
	LabelAetherManaged = aetherLabelPrefix + "/managed"
)
