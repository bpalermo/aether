package constants

const (
	aetherLabelPrefix       = "aether.io"
	aetherLabelSubsetPrefix = "subset." + aetherLabelPrefix

	LabelAetherService = aetherLabelPrefix + "/service"

	LabelKubernetesTopologyRegion = "topology.kubernetes.io/region"
	LabelKubernetesTopologyZone   = "topology.kubernetes.io/zone"
)
