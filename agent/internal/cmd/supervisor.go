package cmd

import (
	"aethermesh.dev/agent/internal/supervisorcmd"
)

// `agent proxy-supervisor` is a DEPRECATED ALIAS of //agent/cmd/proxy-supervisor.
//
// #772 carved the supervisor out into its own binary and its own image: as a
// subcommand it made the aether-proxy pod stage and run the whole 65MiB agent
// binary — controller-runtime, client-go, go-control-plane, SPIRE, Gateway API,
// miekg/dns — as PID 1 to fork a child process, when its actual work needs 24
// modules and none of that.
//
// The chart no longer uses this path, and a forward rolling upgrade never needs
// it: the initContainer's image and the proxy container's command change
// atomically within one chart revision, and `supervisor-bin` is a per-pod
// emptyDir, so a surged-in successor stages its own binary and no old/new pair
// ever shares one. ROLLBACK is the case this exists for. `helm rollback` to a
// chart predating #772 re-issues `agent proxy-supervisor --install-path=...`
// against whatever agent image is deployed; without the alias that initContainer
// dies on "unknown command" and the rollback silently fails to take effect on
// the proxy DaemonSet — during an incident, which is when rollbacks happen.
//
// It costs nothing: hotrestart is agent-internal code the agent binary already
// linked, so keeping it does not move the agent's size or module count.
//
// Remove it — together with the /proxy-ready tars_layer on //agent/cmd/agent:image,
// which only this path reads — once no supported chart version still passes
// `proxy-supervisor` to the agent binary.
func init() {
	cmd := supervisorcmd.New(Version)
	cmd.Deprecated = "run the //agent/cmd/proxy-supervisor binary instead; the chart stages it from the proxy-supervisor image (#772)"
	rootCmd.AddCommand(cmd)
}
