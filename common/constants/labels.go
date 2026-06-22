package constants

const (
	aetherLabelPrefix = "aether.io"

	// LabelAetherManaged is the Kubernetes label used to opt pods into the Aether
	// service mesh. The value must be "true" for the pod to be managed.
	LabelAetherManaged = aetherLabelPrefix + "/managed"

	// TaintAgentNotReady is the startup taint that keeps workload pods off a node
	// until the aether agent's CNI is serving. The operator registers nodes with
	// it (kubelet --register-with-taints); the agent tolerates it and removes it
	// once its CNI socket is up, so a pod's CNI ADD can be handled (issue #261).
	TaintAgentNotReady = aetherLabelPrefix + "/agent-not-ready"
)
