// Package prober is a synthetic mesh-availability prober (proposal 013). It runs
// as a per-node DaemonSet, mesh-managed like any client, and probes the mesh data
// plane from the client side — primarily a proxy local-reply liveness endpoint
// (direct_response, no upstream, no app) so a failure is unambiguously the mesh's
// fault. It emits its OWN pass/fail counter, so it records the connection-level
// failures the proxy-emitted aether_stats metric cannot (the source proxy can't
// report its own outage).
package prober

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.30.0"
)

const (
	telemetryServiceName = "aether-prober"
	otlpTimeout          = 10 * time.Second
	shutdownTimeout      = 5 * time.Second

	tierLiveness     = "liveness"
	tierReachability = "reachability"
	tierMeshDNS      = "mesh_dns"

	// defaultMeshDNSPort is appended to a mesh_dns target that omits a port so the
	// probe still dials the local mesh egress listener after resolving the name.
	defaultMeshDNSPort = "18081"

	// maxDrainBytes caps the response body drain (see drainBody). Every probe target
	// answers with a local reply or a small echo document, so the cap is only there so
	// a misrouted probe onto a streaming endpoint can't make the prober the thing that
	// hangs — the probe's own deadline still governs.
	maxDrainBytes = 64 << 10

	resultSuccess         = "success"
	resultHTTPError       = "http_error"
	resultConnectionError = "connection_error"
	resultTimeout         = "timeout"
	resultSaturated       = "saturated"
	// resultDNSError and its refinements are emitted only by the mesh_dns tier: a
	// name-resolution failure is an independently alertable signal, distinct from a
	// post-resolution connect failure (which stays connection_error).
	resultDNSError    = "dns_error"
	resultDNSNXDomain = "dns_nxdomain"
	resultDNSTimeout  = "dns_timeout"
)

// Config configures the prober.
type Config struct {
	Egress              string
	LivenessPath        string
	LivenessAuthority   string
	MeshDomain          string
	ReachabilityTargets []string
	MeshDNSTargets      []string
	Rate                float64
	Timeout             time.Duration
	MaxConcurrent       int
	OTLPEndpoint        string
}

// DefaultConfig returns the default prober configuration.
func DefaultConfig() Config {
	return Config{
		Egress:            "127.0.0.1:18081",
		LivenessPath:      "/-/-/live",
		LivenessAuthority: "liveness.aether.internal",
		MeshDomain:        "aether.internal",
		Rate:              5,
		Timeout:           2 * time.Second,
		MaxConcurrent:     16,
	}
}

type target struct {
	tier      string
	name      string // metric "target" label
	url       string
	authority string
	// client is the HTTP client this tier probes with. The liveness and reachability
	// tiers share the keep-alive client; the mesh_dns tier gets the no-keep-alive one
	// so it resolves and dials on every probe (see Prober.dnsClient).
	client *http.Client
}

// probeDurationBuckets are the explicit histogram boundaries, in SECONDS, for
// aether_probe_request_duration_seconds. They must be set explicitly: the OTel SDK's
// default boundaries (0, 5, 10, 25, ... 10000) are tuned for values expressed in
// MILLISECONDS, so against a seconds-valued duration the first bucket is "<= 5 s" and a
// healthy 2 ms probe is indistinguishable from a timed-out 2 s one — every observation
// lands in the first bucket and the derived quantiles are flat (#732).
//
// The ladder separates the four regimes that actually matter: healthy sub-10 ms probes,
// the 1 s resolver retransmit (#728), the 2 s probe budget (Config.Timeout) and the 5 s
// legacy resolver retransmit.
var probeDurationBuckets = []float64{
	0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 0.75, 1, 1.5, 2, 2.5, 5,
}

// newDurationHistogram builds the probe duration histogram. It is a named function so a
// test can assert the boundaries that actually reach the SDK.
func newDurationHistogram(meter metric.Meter) (metric.Float64Histogram, error) {
	return meter.Float64Histogram("aether_probe_request_duration_seconds",
		metric.WithDescription("Synthetic mesh probe duration."),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(probeDurationBuckets...))
}

// Prober samples the mesh data plane and records results to OTel.
type Prober struct {
	cfg       Config
	log       logr.Logger
	client    *http.Client
	dnsClient *http.Client
	provider  *sdkmetric.MeterProvider // nil when telemetry is disabled
	counter   metric.Int64Counter
	duration  metric.Float64Histogram
	targets   []target
}

// newClient builds a probe client. Redirects are never followed: a probe measures the
// hop it dialled, not wherever that hop points.
//
// keepAlive=false disables connection reuse outright, which is how the mesh_dns tier
// guarantees the property it exists to measure: Go resolves a name only when it dials,
// so a reused connection would silently stop exercising mesh DNS. That guarantee used
// to be accidental — the tier's upstream answers with a body, and probe() closed the
// body without draining it, which is itself enough to make Go destroy the connection.
// It is now stated, so it survives an upstream that answers with no body (as the
// liveness route's direct_response does) and it no longer depends on the drain.
func newClient(keepAlive bool) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DisableKeepAlives = !keepAlive
	return &http.Client{
		// No client.Timeout: the per-probe deadline is a context so we can tell
		// timeout from connection error.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport:     tr,
	}
}

// New builds a Prober.
func New(ctx context.Context, cfg Config, log logr.Logger, version string) (*Prober, error) {
	meter, provider, err := newMeter(ctx, cfg.OTLPEndpoint, version)
	if err != nil {
		return nil, err
	}
	counter, err := meter.Int64Counter("aether_probe_requests_total",
		metric.WithDescription("Synthetic mesh probe results by tier/target/result."))
	if err != nil {
		return nil, fmt.Errorf("probe counter: %w", err)
	}
	duration, err := newDurationHistogram(meter)
	if err != nil {
		return nil, fmt.Errorf("probe histogram: %w", err)
	}

	p := &Prober{
		cfg: cfg, log: log, counter: counter, duration: duration, provider: provider,
		client:    newClient(true),
		dnsClient: newClient(false),
	}

	// Liveness tier (always): hit the proxy's egress local-reply route. No upstream
	// is involved, so this needs no config.aether.io/upstreams authorization.
	p.targets = append(p.targets, target{
		tier:      tierLiveness,
		name:      "egress",
		url:       "http://" + cfg.Egress + cfg.LivenessPath,
		authority: cfg.LivenessAuthority,
		client:    p.client,
	})
	// Reachability tier (optional): full round-trip to an echo upstream.
	for _, svc := range cfg.ReachabilityTargets {
		p.targets = append(p.targets, target{
			tier:      tierReachability,
			name:      svc,
			url:       "http://" + cfg.Egress + "/",
			authority: svc + "." + cfg.MeshDomain,
			client:    p.client,
		})
	}
	// Mesh-DNS tier (optional): unlike the tiers above, the URL is the REAL
	// namespace-qualified name and the authority is left empty, so the Go transport
	// resolves the FQDN through the system resolver (CNI :53 DNAT → agent mesh-DNS
	// resolver → ClusterIP) instead of dialing the fixed egress with a Host override.
	// This is the whole point: it exercises the mesh-DNS path the other tiers bypass,
	// closing the blind spot where a mesh-DNS outage is invisible.
	for _, fqdn := range cfg.MeshDNSTargets {
		p.targets = append(p.targets, target{
			tier: tierMeshDNS,
			name: fqdn,
			url:  "http://" + withDefaultPort(fqdn, defaultMeshDNSPort) + "/",
			// authority intentionally left empty: do NOT override the Host, so the
			// transport actually resolves and connects to the real name.
			//
			// dnsClient, not client: keep-alives are off so every probe dials, and
			// therefore every probe resolves. That IS the SLI (issue #574) — a tier
			// that reused a connection would keep reporting success straight through
			// a mesh-DNS outage.
			client: p.dnsClient,
		})
	}
	return p, nil
}

// withDefaultPort returns authority unchanged when it already carries a port,
// otherwise it appends ":port". It handles the no-port case only (mesh-DNS targets
// are hostnames, never bracketed IPv6 literals).
func withDefaultPort(authority, port string) string {
	if _, _, err := net.SplitHostPort(authority); err == nil {
		return authority
	}
	return authority + ":" + port
}

func newMeter(ctx context.Context, endpoint, version string) (metric.Meter, *sdkmetric.MeterProvider, error) {
	if endpoint == "" {
		return noop.NewMeterProvider().Meter(telemetryServiceName), nil, nil
	}
	res, err := resource.New(
		ctx,
		resource.WithAttributes(
			semconv.ServiceName(telemetryServiceName),
			semconv.ServiceVersion(version),
		),
		resource.WithFromEnv(), // picks up OTEL_RESOURCE_ATTRIBUTES (k8s.node.name)
		resource.WithTelemetrySDK(),
		resource.WithProcess(),
		resource.WithHost(),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("create resource: %w", err)
	}
	exporter, err := otlpmetricgrpc.New(
		ctx,
		otlpmetricgrpc.WithEndpoint(endpoint),
		otlpmetricgrpc.WithInsecure(),
		otlpmetricgrpc.WithTimeout(otlpTimeout),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("create OTLP exporter: %w", err)
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter)),
	)
	return mp.Meter(telemetryServiceName), mp, nil
}

// Run samples all targets until ctx is cancelled, then flushes telemetry.
func (p *Prober) Run(ctx context.Context) error {
	p.log.Info("prober starting", "targets", len(p.targets), "rate", p.cfg.Rate, "egress", p.cfg.Egress)
	var wg sync.WaitGroup
	for _, t := range p.targets {
		wg.Add(1)
		go func(t target) {
			defer wg.Done()
			p.runTarget(ctx, t)
		}(t)
	}
	wg.Wait()
	if p.provider != nil {
		sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := p.provider.Shutdown(sctx); err != nil {
			p.log.Error(err, "telemetry shutdown")
		}
	}
	return nil
}

func (p *Prober) runTarget(ctx context.Context, t target) {
	interval := time.Second
	if p.cfg.Rate > 0 {
		interval = time.Duration(float64(time.Second) / p.cfg.Rate)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	// Bound in-flight probes so a hung hop never blocks the open-loop ticker.
	sem := make(chan struct{}, max(1, p.cfg.MaxConcurrent))
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			select {
			case sem <- struct{}{}:
				go func() {
					defer func() { <-sem }()
					p.probe(ctx, t)
				}()
			default:
				p.record(t, resultSaturated, 0)
			}
		}
	}
}

func (p *Prober) probe(ctx context.Context, t target) {
	rctx, cancel := context.WithTimeout(ctx, p.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, t.url, nil)
	if err != nil {
		p.record(t, resultConnectionError, 0)
		return
	}
	// mesh_dns targets carry no authority: leaving req.Host unset preserves the
	// FQDN so the transport resolves and dials the real name (the point of the tier).
	if t.authority != "" {
		req.Host = t.authority
	}
	// Mark the probe not-sampled so the mesh proxy's HCM doesn't create and export
	// a span per probe. The proxy's tracing is parent-based: a request that already
	// carries a trace context honors its sampled decision instead of rolling
	// random_sampling, so a cleared flag suppresses the span. Without this, at the
	// probe rate with proxy tracing enabled every probe becomes a synthetic
	// liveness-probe trace in Tempo (the access-log health-check exclusion has no
	// tracing equivalent for the direct_response liveness route).
	req.Header.Set("traceparent", notSampledTraceparent())
	start := time.Now()
	resp, err := t.client.Do(req)
	elapsed := time.Since(start).Seconds()
	if err != nil {
		p.record(t, classifyErr(rctx, err), elapsed)
		return
	}
	drainBody(resp.Body)
	if resp.StatusCode == http.StatusOK {
		p.record(t, resultSuccess, elapsed)
		return
	}
	p.record(t, resultHTTPError, elapsed)
}

// drainBody reads the rest of the response body before closing it. Closing a body that
// has not reached EOF makes Go's transport DESTROY the connection instead of returning
// it to the idle pool, so a bare Close() silently converts every keep-alive tier into a
// connect-per-probe tier: the reachability tier is documented as reusing the fixed
// egress, and on this mesh a new connection costs a full upstream mTLS handshake
// (the node proxy's clusters set connection_pool_per_downstream_connection, by design).
// The liveness tier only escaped because its direct_response carries no body at all.
//
// The drain deliberately happens AFTER the caller has stopped the clock: the probe
// measures time-to-response-headers, which is what Client.Do returns on, and reading a
// small already-buffered body must not be folded into the SLI.
func drainBody(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, maxDrainBytes))
	_ = body.Close()
}

// notSampledTraceparent builds a W3C traceparent with the sampled flag clear
// ("-00"). Because the proxy honors a propagated decision over its own
// random_sampling, this keeps every probe out of the proxy's exported spans. The
// trace-id and parent-id are random: an all-zero trace-id is invalid per W3C and
// would be rejected, re-enabling sampling and defeating the suppression.
func notSampledTraceparent() string {
	var buf [24]byte // 16-byte trace-id followed by 8-byte parent-id
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand.Read does not fail on supported platforms; fall back to a
		// fixed nonzero id so the cleared flag is still honored.
		return "00-0000000000000000000000000000ace0-00000000000000a1-00"
	}
	return "00-" + hex.EncodeToString(buf[0:16]) + "-" + hex.EncodeToString(buf[16:24]) + "-00"
}

// classifyErr maps a request error to a result. connection_error is the class the
// proxy-emitted metric is blind to (no response produced / listener down). A
// name-resolution failure is split out into the dns_* classes so a mesh-DNS
// regression is an unambiguous, independently alertable signal — never folded into
// connection_error. A connect failure AFTER successful resolution stays
// connection_error.
//
// Resolution is classified FIRST, ahead of the probe's own deadline. This ordering
// is load-bearing, not stylistic (issue #726). The per-probe budget (Config.Timeout,
// 2s) is SHORTER than the stock resolv.conf retransmit timeout (5s), so a lookup that
// loses a datagram always blows the probe deadline before the resolver retries. With
// the deadline tested first, ctx.Err() was ALWAYS context.DeadlineExceeded by the time
// the error came back and dns_timeout was structurally unreachable for exactly the
// failure it exists to name: three separate investigations read "dns_* is zero" as
// "DNS is healthy" when it only ever meant "DNS never failed FAST".
//
// The reorder is safe because *net.DNSError is produced only by resolution — Go
// returns one (with IsTimeout set) when the context deadline cancels a lookup in
// flight, and a plain OpError with no DNSError inside when the deadline lands on a
// post-resolution connect. So the DNS branch catches resolution stalls and nothing
// else; connect stalls still fall through to resultTimeout below.
func classifyErr(ctx context.Context, err error) string {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		switch {
		case dnsErr.IsNotFound:
			return resultDNSNXDomain
		case dnsErr.IsTimeout:
			return resultDNSTimeout
		default:
			return resultDNSError
		}
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return resultTimeout
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return resultTimeout
	}
	return resultConnectionError
}

func (p *Prober) record(t target, result string, elapsed float64) {
	attrs := metric.WithAttributes(
		attribute.String("tier", t.tier),
		attribute.String("target", t.name),
		attribute.String("result", result),
	)
	p.counter.Add(context.Background(), 1, attrs)
	if elapsed > 0 {
		p.duration.Record(context.Background(), elapsed, attrs)
	}
}
