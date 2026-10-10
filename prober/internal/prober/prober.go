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
	"log/slog"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"sync"
	"time"

	"aethermesh.dev/common/telemetry/serviceresource"
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
	// post-resolution connect failure (which stays connection_error). dns_timeout
	// also covers a probe whose deadline fired while the lookup was still in flight
	// (classifyFailure, #1252).
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
	log       *slog.Logger
	client    *http.Client
	dnsClient *http.Client
	provider  *sdkmetric.MeterProvider // nil when telemetry is disabled
	counter   metric.Int64Counter
	duration  metric.Float64Histogram
	targets   []target
	// pod and node are this prober's identity, read from the OTel resource
	// (OTEL_RESOURCE_ATTRIBUTES k8s.pod.name / k8s.node.name, set by the chart from
	// the downward API). pod rides every datapoint as the `pod` attribute; both are
	// stamped on every AETHER_PROBE_FAIL line (#1041, #1040).
	pod  string
	node string
	// fails emits the bounded per-failure AETHER_PROBE_FAIL lines (#1040).
	fails *failLog
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
func New(ctx context.Context, cfg Config, log *slog.Logger, version string) (*Prober, error) {
	res, err := newResource(ctx, version)
	if err != nil {
		return nil, err
	}
	meter, provider, err := newMeter(ctx, cfg.OTLPEndpoint, res)
	if err != nil {
		return nil, err
	}
	return newProber(cfg, log, res, meter, provider, os.Stdout)
}

// newProber wires a Prober around an already-built resource and meter. It is split out
// of New so a test can hand it a ManualReader-backed meter and a buffer for the
// AETHER_PROBE_FAIL lines, and assert on what actually leaves the process.
func newProber(cfg Config, log *slog.Logger, res *resource.Resource, meter metric.Meter,
	provider *sdkmetric.MeterProvider, failOut io.Writer,
) (*Prober, error) {
	// The labels: tier, target, result and pod here, and `node` from the resource's
	// k8s.node.name. That node is the one this prober runs on: the SOURCE of the
	// probe, never where it went. The metric has no destination, on purpose (a
	// destination label would be a series per endpoint, per pod, per result). A
	// failed probe's destination is found through its AETHER_PROBE_FAIL line: see
	// phaseTimings.
	counter, err := meter.Int64Counter("aether_probe_requests_total",
		metric.WithDescription("Synthetic mesh probe results by tier/target/result. The node is the prober's own (the source); the destination of a failed probe is on its AETHER_PROBE_FAIL log line."))
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
		pod:       resourceString(res, semconv.K8SPodNameKey),
		node:      resourceString(res, semconv.K8SNodeNameKey),
	}
	p.fails = newFailLog(failOut, p.pod, p.node, failLogCap, failLogWindow)

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

// newResource builds the prober's OTel resource. It is built even when telemetry is
// disabled, because the prober's own identity (k8s.pod.name, k8s.node.name) is read
// from it for the `pod` datapoint attribute and the AETHER_PROBE_FAIL lines.
//
// There is deliberately NO host.name (serviceresource.WithoutHost, #1041). The prober is not hostNetwork, so
// host.name is its POD name, and the reference-cluster collector's transform/promote set the metric
// `node` label from host.name ahead of k8s.node.name: every series said
// node="prober-h2mzs" instead of the Kubernetes node, which left #1040's burst
// unplaceable once that pod was rolled away. Node identity comes only from k8s.node.name
// (OTEL_RESOURCE_ATTRIBUTES, downward API spec.nodeName); the pod rides its own `pod`
// datapoint attribute.
func newResource(ctx context.Context, version string) (*resource.Resource, error) {
	// OTEL_RESOURCE_ATTRIBUTES carries k8s.node.name, k8s.pod.name, k8s.namespace.name.
	res, err := serviceresource.New(ctx, telemetryServiceName, version, serviceresource.WithoutHost())
	if err != nil {
		return nil, fmt.Errorf("create resource: %w", err)
	}
	return res, nil
}

// resourceString returns the string value of key on res, or "" when it is absent.
func resourceString(res *resource.Resource, key attribute.Key) string {
	if res == nil {
		return ""
	}
	v, ok := res.Set().Value(key)
	if !ok {
		return ""
	}
	return v.AsString()
}

func newMeter(ctx context.Context, endpoint string, res *resource.Resource) (metric.Meter, *sdkmetric.MeterProvider, error) {
	if endpoint == "" {
		return noop.NewMeterProvider().Meter(telemetryServiceName), nil, nil
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
	// Emit the per-window suppression summaries even once failures stop, so the tail
	// of a burst is counted rather than left pending until the next failure.
	wg.Go(func() {
		tick := time.NewTicker(failLogWindow)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-tick.C:
				p.fails.flush(now)
			}
		}
	})
	wg.Wait()
	// Final summary for anything still suppressed when the prober stops: every open
	// window is closed, whatever its age, and stamped with the time it is (#1463).
	p.fails.flushAll(time.Now())
	if p.provider != nil {
		sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := p.provider.Shutdown(sctx); err != nil {
			p.log.Error("telemetry shutdown", "error", err)
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
	// Return only once every in-flight probe has finished (#1209), so Run's final
	// fail-log flush and telemetry shutdown come after the last record. The
	// cancelled ctx ends them promptly; Timeout bounds them regardless.
	var inflight sync.WaitGroup
	defer inflight.Wait()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			select {
			case sem <- struct{}{}:
				inflight.Go(func() {
					defer func() { <-sem }()
					p.probe(ctx, t)
				})
			default:
				p.record(t, resultSaturated, 0, errSaturated, noPhase)
			}
		}
	}
}

func (p *Prober) probe(ctx context.Context, t target) {
	rctx, cancel := context.WithTimeout(ctx, p.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, t.url, nil)
	if err != nil {
		p.record(t, resultConnectionError, 0, err, noPhase)
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
	// The header's trace id is also the probe's name in the proxies' access logs: a
	// failed probe prints it, and the rows that carry it say where the probe went
	// (#1391; see phaseTimings for why the trace id and not the whole header).
	traceparent := notSampledTraceparent()
	req.Header.Set("traceparent", traceparent)
	start := time.Now()
	// The phase trace (#1252): which step of the request the time went to. The
	// transport's dial goroutine keeps the request context's values, so the DNS and
	// connect hooks fire even for a dial the deadline abandons.
	rec := newPhaseRecorder()
	req = req.WithContext(httptrace.WithClientTrace(rctx, rec.trace()))
	resp, err := t.client.Do(req)
	end := time.Now()
	elapsed := end.Sub(start).Seconds()
	if err != nil {
		// The prober itself is stopping (SIGTERM cancels ctx): the probe was cut
		// short by us, not failed by the data plane. Record nothing, not even an
		// attempt, or every prober roll writes connection_error into the SLI
		// (#1209). The probe's own deadline is DeadlineExceeded and still lands
		// in timeout below; a cancel while ctx is live stays classified as before.
		if ctx.Err() != nil && errors.Is(err, context.Canceled) {
			return
		}
		snap := rec.snapshot(end)
		pt := snap.timings()
		pt.TraceID = traceID(traceparent)
		p.record(t, classifyFailure(rctx, err, pt.Phase), elapsed, err, pt)
		return
	}
	drainBody(resp.Body)
	if resp.StatusCode == http.StatusOK {
		p.record(t, resultSuccess, elapsed, nil, noPhase)
		return
	}
	snap := rec.snapshot(end)
	pt := snap.timings()
	pt.TraceID = traceID(traceparent)
	pt.Phase = phaseResponse // the request completed; the status is the failure
	p.record(t, resultHTTPError, elapsed, fmt.Errorf("HTTP %d", resp.StatusCode), pt)
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

// traceID is the trace-id of a W3C traceparent ("00-<32 hex>-<16 hex>-<flags>"), or ""
// when tp is not one.
func traceID(tp string) string {
	if len(tp) != 55 || tp[2] != '-' || tp[35] != '-' || tp[52] != '-' {
		return ""
	}
	return tp[3:35]
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
// The reorder is safe because *net.DNSError is produced only by resolution, so the DNS
// branch catches resolution failures and nothing else; connect failures still fall
// through below.
//
// What the error alone CANNOT show is a resolution stall that outlives the probe
// (#1252). On the net/http path a *net.DNSError arrives only when the lookup itself
// fails first (NXDOMAIN, SERVFAIL, the resolver's own retransmit timeout). The dial runs
// on a context detached from the request's cancellation, so when the probe's deadline
// fires while the lookup is still in flight, Transport.getConn returns the REQUEST
// context's cause, a bare context.DeadlineExceeded, and this function can only say
// `timeout` (TestTransportHidesResolutionStall). classifyFailure adds the phase the
// probe's trace saw.
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

// classifyFailure is classifyErr plus the phase the probe's trace was in when it ended
// (#1252). A deadline that interrupted name resolution is dns_timeout: Go's transport
// returns it as a bare context deadline (see classifyErr), so without the phase it
// was always counted `timeout` and "dns_* is zero" never meant DNS was healthy.
//
// Deadlines in every later phase stay `timeout`. The result label set is deliberately
// NOT widened with connect_timeout / first_byte_timeout: aether_probe_requests_total is
// the soak SLI and the alert input, every new value is a new series per tier, target
// and pod, and splitting `timeout` would silently change what an existing
// result="timeout" rule matches. The phase, with per-phase milliseconds, is on the
// AETHER_PROBE_FAIL line instead.
func classifyFailure(ctx context.Context, err error, phase string) string {
	result := classifyErr(ctx, err)
	if result == resultTimeout && phase == phaseDNS {
		return resultDNSTimeout
	}
	return result
}

// record counts one probe outcome and, for anything but success, emits the bounded
// AETHER_PROBE_FAIL line. err is the failure's cause (nil on success); pt is the
// probe's phase timings (noPhase when it never reached the transport).
func (p *Prober) record(t target, result string, elapsed float64, err error, pt phaseTimings) {
	kvs := []attribute.KeyValue{
		attribute.String("tier", t.tier),
		attribute.String("target", t.name),
		attribute.String("result", result),
	}
	// pod (#1041): the metric `node` label is the Kubernetes node, so the pod is its
	// own attribute. Per-pod anomalies stay distinguishable, and two prober
	// generations on one node never write the same series.
	if p.pod != "" {
		kvs = append(kvs, attribute.String("pod", p.pod))
	}
	attrs := metric.WithAttributes(kvs...)
	p.counter.Add(context.Background(), 1, attrs)
	if elapsed > 0 {
		p.duration.Record(context.Background(), elapsed, attrs)
	}
	if result != resultSuccess {
		p.fails.log(time.Now(), t, result, elapsed, err, pt)
	}
}
