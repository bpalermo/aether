package constants

const (
	aetherLabelPrefix = "aether.io"

	// LabelAetherService is the Kubernetes label used to mark pods as managed by Aether
	// and to specify which service they belong to. The value should be the service name.
	LabelAetherService = aetherLabelPrefix + "/service"
)
