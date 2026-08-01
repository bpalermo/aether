package telemetry

import (
	"strings"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc/stats"
)

// notHealthCheck filters gRPC health-check RPCs out of telemetry so liveness
// probes don't drown traces and metrics in noise.
func notHealthCheck(info *stats.RPCTagInfo) bool {
	return !strings.HasPrefix(info.FullMethodName, "/grpc.health.v1.Health/")
}

// ServerStatsHandler returns a gRPC server stats handler that records OTel
// spans and RPC metrics through the global providers. It is a no-op until
// setup.Setup/setup.SetupTracing (common/telemetry/setup) register real
// providers, so it is safe (and intended) to install unconditionally on
// every gRPC server.
func ServerStatsHandler() stats.Handler {
	return otelgrpc.NewServerHandler(otelgrpc.WithFilter(notHealthCheck))
}

// ClientStatsHandler returns the client-side counterpart of ServerStatsHandler.
// Installing it on a gRPC connection propagates trace context to the server,
// linking client and server spans across processes.
func ClientStatsHandler() stats.Handler {
	return otelgrpc.NewClientHandler(otelgrpc.WithFilter(notHealthCheck))
}
