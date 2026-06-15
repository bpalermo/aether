//! Aether stats dynamic module (proposal 007). Analogous to Istio's istio_stats:
//! an HTTP filter that records a tagged request counter at the log phase. The
//! same module serves both reporters via the per-instance `filter_config` JSON:
//!
//! - **Source-reported** (`reporter="source"`), attached to each per-pod
//!   OUTBOUND HCM. The agent injects the local pod's identity
//!   (source_service/source_pod); the module derives the destination from the
//!   routed cluster name.
//! - **Destination-reported** (`reporter="destination"`), attached to each
//!   per-pod INBOUND HCM (Phase 2). The agent injects the local pod's identity
//!   (destination_service); the module derives the source by parsing the
//!   verified peer SVID's URI SAN (`ConnectionUriSanPeerCertificate`).
//!
//! Either way it increments a cumulative Envoy counter vector at request
//! completion — exported by the existing OTel stat sink (no per-request egress).

use envoy_proxy_dynamic_modules_rust_sdk::*;
use serde::Deserialize;
use std::time::Instant;

declare_init_functions!(init, new_http_filter_config_fn);

fn init() -> bool {
    true
}

#[derive(Deserialize)]
struct ConfigData {
    #[serde(default = "default_reporter")]
    reporter: String,
    #[serde(default)]
    source_service: String,
    #[serde(default)]
    source_pod: String,
    /// Local pod's service for the inbound (destination-reported) side. The
    /// outbound side leaves this empty and derives the destination per request
    /// from the routed cluster name instead.
    #[serde(default)]
    destination_service: String,
    #[serde(default = "default_mesh_domain")]
    mesh_domain: String,
    /// When false, source_pod is reported as "" to bound cardinality.
    #[serde(default)]
    emit_pod: bool,
}

fn default_reporter() -> String {
    "source".to_string()
}
fn default_mesh_domain() -> String {
    "aether.internal".to_string()
}

pub struct FilterConfig {
    reporter: String,
    source_service: String,
    source_pod: String,
    destination_service: String,
    mesh_domain: String,
    requests_total: EnvoyCounterVecId,
    request_duration_ms: EnvoyHistogramVecId,
}

impl FilterConfig {
    pub fn new<EC: EnvoyHttpFilterConfig>(filter_config: &str, ec: &mut EC) -> Option<Self> {
        let c: ConfigData = match serde_json::from_str(filter_config) {
            Ok(c) => c,
            Err(e) => {
                eprintln!("aether_stats: bad filter_config: {e}");
                return None;
            }
        };
        // Cumulative counter vector; labels lifted to OTLP attributes by the sink.
        // Phase 1 (source-reported) label set. response_flags is intentionally
        // omitted here — it is only reliably available at log time, which the
        // access-logger variant (Phase 1b) provides; adding it later is an
        // additive Prometheus label.
        let requests_total = ec
            .define_counter_vec(
                "aether_requests_total",
                &[
                    "reporter",
                    "source_service",
                    "source_pod",
                    "destination_service",
                    "response_code",
                    "response_flags",
                ],
            )
            .ok()?;
        // Request-duration histogram. Deliberately a LEANER label set than the
        // counter — a histogram multiplies series by its bucket count, so it
        // omits source_pod/response_code/response_flags and keeps only the edge
        // identity. Duration is measured in-module (no Envoy duration attribute
        // is exposed to HTTP filters); the unit lives in the metric name.
        let request_duration_ms = ec
            .define_histogram_vec(
                "aether_request_duration_milliseconds",
                &["reporter", "source_service", "destination_service"],
            )
            .ok()?;
        Some(Self {
            reporter: c.reporter,
            source_service: c.source_service,
            source_pod: if c.emit_pod {
                c.source_pod
            } else {
                String::new()
            },
            destination_service: c.destination_service,
            mesh_domain: c.mesh_domain,
            requests_total,
            request_duration_ms,
        })
    }
}

impl<EHF: EnvoyHttpFilter> HttpFilterConfig<EHF> for FilterConfig {
    fn new_http_filter(&self, _envoy: &mut EHF) -> Box<dyn HttpFilter<EHF>> {
        Box::new(Filter {
            reporter: self.reporter.clone(),
            source_service: self.source_service.clone(),
            source_pod: self.source_pod.clone(),
            destination_service: self.destination_service.clone(),
            mesh_domain: self.mesh_domain.clone(),
            requests_total: self.requests_total,
            request_duration_ms: self.request_duration_ms,
            // Filters are created per stream at request start, so this is the
            // earliest observable point for the request-duration measurement.
            start: Instant::now(),
            dest_cluster: String::new(),
            local_reply_details: String::new(),
            recorded: false,
        })
    }
}

pub struct Filter {
    reporter: String,
    source_service: String,
    source_pod: String,
    destination_service: String,
    mesh_domain: String,
    requests_total: EnvoyCounterVecId,
    request_duration_ms: EnvoyHistogramVecId,
    start: Instant,
    dest_cluster: String,
    local_reply_details: String,
    recorded: bool,
}

impl Filter {
    fn is_destination(&self) -> bool {
        self.reporter == "destination"
    }

    fn record<EHF: EnvoyHttpFilter>(&mut self, envoy: &mut EHF) {
        if self.recorded {
            return;
        }
        self.recorded = true;

        // The two reporters fill the same counter vector but source each
        // half differently. Outbound (source-reported): source from the
        // agent-injected config, destination from the routed cluster name.
        // Inbound (destination-reported): destination from the agent-injected
        // local identity, source parsed from the verified peer SVID's URI SAN.
        let (source_service, source_pod, destination_service) = if self.is_destination() {
            (
                self.peer_source_service(envoy),
                // The SPIFFE SVID carries no pod, so the inbound source pod is
                // always unknown; reported as "" like the outbound default.
                String::new(),
                if self.destination_service.is_empty() {
                    "unknown".to_string()
                } else {
                    self.destination_service.clone()
                },
            )
        } else {
            (
                self.source_service.clone(),
                self.source_pod.clone(),
                self.dest_service_from_cluster(&self.dest_cluster.clone()),
            )
        };

        let response_code = envoy
            .get_attribute_int(abi::envoy_dynamic_module_type_attribute_id::ResponseCode)
            .map(|c| c.to_string())
            .or_else(|| {
                envoy
                    .get_response_header_value(":status")
                    .map(|b| String::from_utf8_lossy(b.as_slice()).into_owned())
            })
            .unwrap_or_else(|| "0".to_string());

        // Cause flag, mapped from the proxy's local-reply details (empty means
        // the response came from upstream, or success).
        let response_flags = classify_flag(&self.local_reply_details);

        let _ = envoy.increment_counter_vec(
            self.requests_total,
            &[
                &self.reporter,
                &source_service,
                &source_pod,
                &destination_service,
                &response_code,
                response_flags,
            ],
            1,
        );

        // Request duration on the lean {reporter, source, destination} label set.
        let elapsed_ms = self.start.elapsed().as_millis().min(u64::MAX as u128) as u64;
        let _ = envoy.record_histogram_value_vec(
            self.request_duration_ms,
            &[&self.reporter, &source_service, &destination_service],
            elapsed_ms,
        );
    }

    /// Reads the verified peer (caller) SVID's URI SAN from the downstream mTLS
    /// connection and parses its service-account segment. Returns "unknown" when
    /// no peer cert / URI SAN is present or the SAN is not a SPIFFE SVID — this
    /// is the only untrusted input the module parses.
    fn peer_source_service<EHF: EnvoyHttpFilter>(&self, envoy: &mut EHF) -> String {
        envoy
            .get_attribute_string(
                abi::envoy_dynamic_module_type_attribute_id::ConnectionUriSanPeerCertificate,
            )
            .and_then(|b| spiffe_service(&String::from_utf8_lossy(b.as_slice())).map(str::to_owned))
            .unwrap_or_else(|| "unknown".to_string())
    }

    /// "<svc>.<mesh_domain>[:port]" -> "<svc>"; empty/foreign -> "unknown".
    fn dest_service_from_cluster(&self, cluster: &str) -> String {
        if cluster.is_empty() {
            return "unknown".to_string();
        }
        let no_port = cluster.split(':').next().unwrap_or(cluster);
        let suffix = format!(".{}", self.mesh_domain);
        let bare = no_port.strip_suffix(&suffix).unwrap_or(no_port);
        if bare.is_empty() {
            "unknown".to_string()
        } else {
            bare.to_string()
        }
    }
}

impl<EHF: EnvoyHttpFilter> HttpFilter<EHF> for Filter {
    fn on_request_headers(
        &mut self,
        envoy: &mut EHF,
        _end_of_stream: bool,
    ) -> abi::envoy_dynamic_module_type_on_http_filter_request_headers_status {
        // Only the source-reported (outbound) side derives the destination from
        // the routed cluster; the inbound side takes its destination from config
        // and reads the source from the peer SAN at record time.
        if !self.is_destination() {
            // Cluster is selected by the router after request headers; capture it
            // here via the dedicated getter (the XdsClusterName CEL attribute is
            // not populated in the HTTP-filter context).
            if let Some(b) = envoy.get_cluster_name() {
                self.dest_cluster = String::from_utf8_lossy(b.as_slice()).into_owned();
            }
        }
        abi::envoy_dynamic_module_type_on_http_filter_request_headers_status::Continue
    }

    // Proxy-generated replies (connect failure, no healthy host, no cluster,
    // timeouts) carry the cause in `details`; capture it for the flag label.
    fn on_local_reply(
        &mut self,
        _envoy: &mut EHF,
        _response_code: u32,
        details: EnvoyBuffer,
        _reset_imminent: bool,
    ) -> abi::envoy_dynamic_module_type_on_http_filter_local_reply_status {
        self.local_reply_details = String::from_utf8_lossy(details.as_slice()).into_owned();
        abi::envoy_dynamic_module_type_on_http_filter_local_reply_status::Continue
    }

    // Record once at stream completion (the log phase): response_code is final
    // here, and on_stream_complete fires for every request including local
    // replies (e.g. a 503/UF connect failure).
    fn on_stream_complete(&mut self, envoy: &mut EHF) {
        self.record(envoy);
    }
}

/// Maps an Envoy local-reply `details` string to a bounded cause flag, so the
/// `response_flags` label stays low-cardinality. Empty details (the response
/// came from upstream, or success) -> "-". The ResponseFlags attribute is not
/// exposed to dynamic-module HTTP filters at v1.38, so the cause is derived from
/// the local-reply details captured in on_local_reply.
fn classify_flag(details: &str) -> &'static str {
    if details.is_empty() {
        return "-";
    }
    // Order matters: check the most specific causes first.
    if details.contains("connection_failure")
        || details.contains("connect_error")
        || details.contains("connection refused")
        || details.contains("connection_termination")
    {
        "UF" // upstream connection failure
    } else if details.contains("no_healthy_upstream") {
        "UH" // no healthy upstream host
    } else if details.contains("cluster_not_found") || details.contains("no_cluster") {
        "NC" // no cluster (e.g. ODCDS cold-path miss)
    } else if details.contains("route_not_found") || details.contains("no_route") {
        "NR" // no route configured
    } else if details.contains("overflow") {
        "UO" // upstream/connection-pool overflow (circuit breaker)
    } else if details.contains("timeout") {
        "UT" // upstream/stream timeout
    } else if details.contains("upstream_reset")
        || details.contains("remote_reset")
        || details.contains("local_reset")
    {
        "UR" // upstream reset (post-connect)
    } else {
        "LR" // some other proxy local reply
    }
}

/// Extracts the service-account segment from a SPIFFE SVID URI SAN of the form
/// `spiffe://<trust-domain>/ns/<namespace>/sa/<service-account>` (the SVID the
/// agent issues, see the agent's listener SVID template). Returns the value
/// following the first `sa` path segment; `None` for any URI that is not a
/// SPIFFE SVID or carries no non-empty `sa` segment. This is the only untrusted
/// input the module parses, so it never panics and never allocates.
fn spiffe_service(uri_san: &str) -> Option<&str> {
    // Drop the scheme + authority (trust domain); keep the path segments.
    let rest = uri_san.strip_prefix("spiffe://")?;
    let path = rest.split_once('/').map(|(_, p)| p).unwrap_or("");
    let mut segs = path.split('/');
    while let Some(seg) = segs.next() {
        if seg == "sa" {
            return segs.next().filter(|s| !s.is_empty());
        }
    }
    None
}

fn new_http_filter_config_fn<EC: EnvoyHttpFilterConfig, EHF: EnvoyHttpFilter>(
    envoy_filter_config: &mut EC,
    filter_name: &str,
    filter_config: &[u8],
) -> Option<Box<dyn HttpFilterConfig<EHF>>> {
    let cfg = std::str::from_utf8(filter_config).unwrap_or("{}");
    match filter_name {
        "stats" => FilterConfig::new(cfg, envoy_filter_config)
            .map(|c| Box::new(c) as Box<dyn HttpFilterConfig<EHF>>),
        other => {
            eprintln!("aether_stats: unknown filter_name {other}");
            None
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::{Arc, Mutex};

    // ---- classify_flag ---------------------------------------------------

    #[test]
    fn classify_flag_empty_is_dash() {
        // No local reply (upstream answered, or success) -> "-".
        assert_eq!(classify_flag(""), "-");
    }

    #[test]
    fn classify_flag_maps_each_cause() {
        // One representative `details` token per documented Envoy cause.
        assert_eq!(classify_flag("upstream_connection_failure"), "UF");
        assert_eq!(
            classify_flag("upstream_reset_before_response_started{connect_error}"),
            "UF"
        );
        assert_eq!(
            classify_flag("delayed_connect_error: connection refused"),
            "UF"
        );
        assert_eq!(classify_flag("upstream_connection_termination"), "UF");
        assert_eq!(classify_flag("no_healthy_upstream"), "UH");
        assert_eq!(classify_flag("cluster_not_found"), "NC");
        assert_eq!(classify_flag("no_cluster"), "NC");
        assert_eq!(classify_flag("route_not_found"), "NR");
        assert_eq!(classify_flag("no_route"), "NR");
        assert_eq!(classify_flag("upstream_overflow"), "UO");
        assert_eq!(classify_flag("upstream_response_timeout"), "UT");
        assert_eq!(classify_flag("upstream_reset_after_response_started"), "UR");
        assert_eq!(classify_flag("remote_reset"), "UR");
        assert_eq!(classify_flag("local_reset"), "UR");
    }

    #[test]
    fn classify_flag_unknown_cause_is_lr() {
        // Any other proxy-generated local reply falls back to a generic flag.
        assert_eq!(classify_flag("ext_authz_denied"), "LR");
        assert_eq!(classify_flag("filter_removed"), "LR");
    }

    #[test]
    fn classify_flag_ordering_prefers_specific_cause() {
        // A connection failure that also mentions "reset" must classify as the
        // more specific UF, not UR (UF is checked first).
        assert_eq!(
            classify_flag("upstream_connection_failure_after_reset"),
            "UF"
        );
    }

    // ---- dest_service_from_cluster --------------------------------------

    fn test_filter() -> Filter {
        Filter {
            reporter: "source".to_string(),
            source_service: "cart".to_string(),
            source_pod: "cart-abc".to_string(),
            destination_service: String::new(),
            mesh_domain: "aether.internal".to_string(),
            requests_total: EnvoyCounterVecId(0),
            request_duration_ms: EnvoyHistogramVecId(0),
            start: Instant::now(),
            dest_cluster: String::new(),
            local_reply_details: String::new(),
            recorded: false,
        }
    }

    // Destination-reported (inbound) filter: destination is the agent-injected
    // local identity; source is parsed per request from the peer SVID URI SAN.
    fn test_filter_dest() -> Filter {
        Filter {
            reporter: "destination".to_string(),
            source_service: String::new(),
            source_pod: String::new(),
            destination_service: "checkout".to_string(),
            mesh_domain: "aether.internal".to_string(),
            requests_total: EnvoyCounterVecId(0),
            request_duration_ms: EnvoyHistogramVecId(0),
            start: Instant::now(),
            dest_cluster: String::new(),
            local_reply_details: String::new(),
            recorded: false,
        }
    }

    #[test]
    fn dest_service_strips_mesh_suffix_and_port() {
        let f = test_filter();
        assert_eq!(
            f.dest_service_from_cluster("checkout.aether.internal"),
            "checkout"
        );
        assert_eq!(
            f.dest_service_from_cluster("checkout.aether.internal:8080"),
            "checkout"
        );
    }

    #[test]
    fn dest_service_keeps_name_without_mesh_suffix() {
        // Foreign / non-mesh cluster names are reported verbatim (port stripped).
        let f = test_filter();
        assert_eq!(f.dest_service_from_cluster("external"), "external");
        assert_eq!(f.dest_service_from_cluster("external:443"), "external");
    }

    #[test]
    fn dest_service_empty_or_bare_suffix_is_unknown() {
        let f = test_filter();
        assert_eq!(f.dest_service_from_cluster(""), "unknown");
        // Only the suffix, nothing left after stripping.
        assert_eq!(f.dest_service_from_cluster(".aether.internal"), "unknown");
        // Only a port, no name.
        assert_eq!(f.dest_service_from_cluster(":8080"), "unknown");
    }

    // ---- spiffe_service --------------------------------------------------

    #[test]
    fn spiffe_service_extracts_sa_segment() {
        assert_eq!(
            spiffe_service("spiffe://aether.internal/ns/default/sa/cart"),
            Some("cart")
        );
        // Extra trailing segments after the service are ignored.
        assert_eq!(
            spiffe_service("spiffe://aether.internal/ns/default/sa/cart/extra"),
            Some("cart")
        );
        // The `ns` segment is not required; only `sa/<svc>` is.
        assert_eq!(spiffe_service("spiffe://td/sa/payments"), Some("payments"));
    }

    #[test]
    fn spiffe_service_rejects_non_spiffe() {
        assert_eq!(spiffe_service(""), None);
        assert_eq!(spiffe_service("https://example.com/sa/cart"), None);
        assert_eq!(spiffe_service("cart"), None);
    }

    #[test]
    fn spiffe_service_rejects_missing_or_empty_sa() {
        // No sa segment at all.
        assert_eq!(spiffe_service("spiffe://td/ns/default"), None);
        // Authority only, no path.
        assert_eq!(spiffe_service("spiffe://td"), None);
        // sa segment present but the service value is empty.
        assert_eq!(spiffe_service("spiffe://td/ns/default/sa/"), None);
        assert_eq!(spiffe_service("spiffe://td/ns/default/sa"), None);
    }

    // ---- ConfigData deserialization -------------------------------------

    #[test]
    fn config_defaults_when_empty() {
        let c: ConfigData = serde_json::from_str("{}").unwrap();
        assert_eq!(c.reporter, "source");
        assert_eq!(c.source_service, "");
        assert_eq!(c.source_pod, "");
        assert_eq!(c.destination_service, "");
        assert_eq!(c.mesh_domain, "aether.internal");
        assert!(!c.emit_pod);
    }

    #[test]
    fn config_parses_all_fields() {
        let json = r#"{
            "reporter": "destination",
            "source_service": "cart",
            "source_pod": "cart-7d9",
            "destination_service": "checkout",
            "mesh_domain": "example.mesh",
            "emit_pod": true
        }"#;
        let c: ConfigData = serde_json::from_str(json).unwrap();
        assert_eq!(c.reporter, "destination");
        assert_eq!(c.source_service, "cart");
        assert_eq!(c.source_pod, "cart-7d9");
        assert_eq!(c.destination_service, "checkout");
        assert_eq!(c.mesh_domain, "example.mesh");
        assert!(c.emit_pod);
    }

    #[test]
    fn config_rejects_malformed_json() {
        assert!(serde_json::from_str::<ConfigData>("not json").is_err());
    }

    // ---- Filter HTTP hooks (mocked Envoy) -------------------------------

    // Collects the label vector passed to increment_counter_vec so tests can
    // assert the exact (reporter, source_service, source_pod, destination,
    // response_code, response_flags) tuple recorded. Also stubs the duration
    // histogram record() always emits alongside the counter, so the strict mock
    // does not fail on the unexpected call.
    fn expect_record(mock: &mut MockEnvoyHttpFilter) -> Arc<Mutex<Vec<String>>> {
        let labels = Arc::new(Mutex::new(Vec::new()));
        let sink = labels.clone();
        mock.expect_increment_counter_vec()
            .times(1)
            .returning(move |_id, ls, _value| {
                *sink.lock().unwrap() = ls.iter().map(|s| s.to_string()).collect();
                Ok(())
            });
        mock.expect_record_histogram_value_vec()
            .times(1)
            .returning(|_id, _ls, _value| Ok(()));
        labels
    }

    #[test]
    fn on_request_headers_captures_cluster_name() {
        let mut mock = MockEnvoyHttpFilter::default();
        mock.expect_get_cluster_name()
            .times(1)
            .returning(|| Some(EnvoyBuffer::new(b"checkout.aether.internal:8080")));

        let mut f = test_filter();
        f.on_request_headers(&mut mock, false);
        assert_eq!(f.dest_cluster, "checkout.aether.internal:8080");
    }

    #[test]
    fn on_request_headers_no_cluster_leaves_empty() {
        let mut mock = MockEnvoyHttpFilter::default();
        mock.expect_get_cluster_name().times(1).returning(|| None);

        let mut f = test_filter();
        f.on_request_headers(&mut mock, false);
        assert_eq!(f.dest_cluster, "");
    }

    #[test]
    fn on_local_reply_captures_details() {
        let mut mock = MockEnvoyHttpFilter::default();
        let mut f = test_filter();
        f.on_local_reply(
            &mut mock,
            503,
            EnvoyBuffer::new(b"no_healthy_upstream"),
            false,
        );
        assert_eq!(f.local_reply_details, "no_healthy_upstream");
    }

    #[test]
    fn record_upstream_success_uses_attribute_code_and_dash_flag() {
        let mut mock = MockEnvoyHttpFilter::default();
        mock.expect_get_attribute_int()
            .times(1)
            .returning(|_| Some(200));
        let labels = expect_record(&mut mock);

        let mut f = test_filter();
        f.dest_cluster = "checkout.aether.internal:8080".to_string();
        f.record(&mut mock);

        assert!(f.recorded);
        assert_eq!(
            *labels.lock().unwrap(),
            vec!["source", "cart", "cart-abc", "checkout", "200", "-"]
        );
    }

    #[test]
    fn record_falls_back_to_status_header_when_no_attribute() {
        let mut mock = MockEnvoyHttpFilter::default();
        mock.expect_get_attribute_int().times(1).returning(|_| None);
        mock.expect_get_response_header_value()
            .times(1)
            .returning(|_| Some(EnvoyBuffer::new(b"503")));
        let labels = expect_record(&mut mock);

        let mut f = test_filter();
        f.dest_cluster = "checkout.aether.internal".to_string();
        f.local_reply_details = "upstream_connection_failure".to_string();
        f.record(&mut mock);

        assert_eq!(
            *labels.lock().unwrap(),
            vec!["source", "cart", "cart-abc", "checkout", "503", "UF"]
        );
    }

    #[test]
    fn record_defaults_code_to_zero_when_unavailable() {
        let mut mock = MockEnvoyHttpFilter::default();
        mock.expect_get_attribute_int().times(1).returning(|_| None);
        mock.expect_get_response_header_value()
            .times(1)
            .returning(|_| None);
        let labels = expect_record(&mut mock);

        let mut f = test_filter();
        // Empty cluster -> "unknown" destination.
        f.record(&mut mock);

        let l = labels.lock().unwrap();
        assert_eq!(l[3], "unknown");
        assert_eq!(l[4], "0");
        assert_eq!(l[5], "-");
    }

    #[test]
    fn record_destination_uses_config_dest_and_peer_san_source() {
        // Inbound: destination from config, source parsed from the peer SVID.
        let mut mock = MockEnvoyHttpFilter::default();
        mock.expect_get_attribute_string().returning(|id| {
            assert_eq!(
                id,
                abi::envoy_dynamic_module_type_attribute_id::ConnectionUriSanPeerCertificate
            );
            Some(EnvoyBuffer::new(
                b"spiffe://aether.internal/ns/default/sa/cart",
            ))
        });
        mock.expect_get_attribute_int()
            .times(1)
            .returning(|_| Some(200));
        let labels = expect_record(&mut mock);

        let mut f = test_filter_dest();
        f.record(&mut mock);

        assert_eq!(
            *labels.lock().unwrap(),
            // reporter, source_service (from SAN), source_pod (always ""),
            // destination_service (from config), code, flag.
            vec!["destination", "cart", "", "checkout", "200", "-"]
        );
    }

    #[test]
    fn record_destination_missing_peer_san_is_unknown_source() {
        // No client cert / no URI SAN -> source_service "unknown", never panics.
        let mut mock = MockEnvoyHttpFilter::default();
        mock.expect_get_attribute_string().returning(|_| None);
        mock.expect_get_attribute_int()
            .times(1)
            .returning(|_| Some(200));
        let labels = expect_record(&mut mock);

        let mut f = test_filter_dest();
        f.record(&mut mock);

        let l = labels.lock().unwrap();
        assert_eq!(l[0], "destination");
        assert_eq!(l[1], "unknown");
        assert_eq!(l[3], "checkout");
    }

    #[test]
    fn record_is_idempotent() {
        let mut mock = MockEnvoyHttpFilter::default();
        // increment must happen exactly once even though record() runs twice.
        mock.expect_get_attribute_int()
            .times(1)
            .returning(|_| Some(200));
        expect_record(&mut mock);

        let mut f = test_filter();
        f.dest_cluster = "checkout.aether.internal".to_string();
        f.record(&mut mock);
        f.record(&mut mock);
    }

    #[test]
    fn on_stream_complete_records_full_flow() {
        let mut mock = MockEnvoyHttpFilter::default();
        mock.expect_get_cluster_name()
            .times(1)
            .returning(|| Some(EnvoyBuffer::new(b"checkout.aether.internal:8080")));
        mock.expect_get_attribute_int().times(1).returning(|_| None);
        mock.expect_get_response_header_value()
            .times(1)
            .returning(|_| Some(EnvoyBuffer::new(b"503")));
        let labels = expect_record(&mut mock);

        let mut f = test_filter();
        // Router selects the cluster, then a connect failure produces a local reply.
        f.on_request_headers(&mut mock, false);
        f.on_local_reply(
            &mut mock,
            503,
            EnvoyBuffer::new(b"upstream_connection_failure"),
            false,
        );
        f.on_stream_complete(&mut mock);

        assert_eq!(
            *labels.lock().unwrap(),
            vec!["source", "cart", "cart-abc", "checkout", "503", "UF"]
        );
    }

    // ---- duration histogram ---------------------------------------------

    #[test]
    fn record_emits_duration_histogram_on_lean_labels() {
        // The histogram carries only {reporter, source_service, destination_service}
        // — not source_pod/response_code/response_flags — to bound bucket×label
        // cardinality.
        let mut mock = MockEnvoyHttpFilter::default();
        mock.expect_get_attribute_int()
            .times(1)
            .returning(|_| Some(200));
        mock.expect_increment_counter_vec()
            .times(1)
            .returning(|_, _, _| Ok(()));
        let hist_labels = Arc::new(Mutex::new(Vec::new()));
        let sink = hist_labels.clone();
        mock.expect_record_histogram_value_vec()
            .times(1)
            .returning(move |_id, ls, _value| {
                *sink.lock().unwrap() = ls.iter().map(|s| s.to_string()).collect();
                Ok(())
            });

        let mut f = test_filter();
        f.dest_cluster = "checkout.aether.internal".to_string();
        f.record(&mut mock);

        assert_eq!(
            *hist_labels.lock().unwrap(),
            vec!["source", "cart", "checkout"]
        );
    }

    // ---- spiffe_service fuzz/torture -------------------------------------

    #[test]
    fn spiffe_service_never_panics_on_adversarial_input() {
        // The peer SAN is the only untrusted input. The parser must be total:
        // never panic, always terminate, and only ever return a slice that is a
        // substring of its input. Exercise pathological shapes plus a sweep of
        // every single byte and some large/repetitive inputs.
        let mut cases: Vec<String> = vec![
            String::new(),
            "spiffe://".to_string(),
            "spiffe:///".to_string(),
            "spiffe://td/sa".to_string(),
            "spiffe://td/sa/".to_string(),
            "sa/x".to_string(),
            "/////".to_string(),
            "spiffe://td/sa/svc/sa/other".to_string(),
            "spiffe://td/ns//sa//".to_string(),
            "\0\0\0".to_string(),
            "spiffe://td/\u{0}/sa/\u{0}svc".to_string(),
            "spiffe://td/ns/默认/sa/服务".to_string(),
        ];
        // Single-byte inputs (control chars, high bytes via lossy decode).
        for b in 0u8..=255 {
            cases.push(String::from_utf8_lossy(&[b]).into_owned());
        }
        // Large and repetitive shapes.
        cases.push("a".repeat(1 << 20));
        cases.push("/".repeat(100_000));
        cases.push(format!("spiffe://td/{}/sa/svc", "x/".repeat(50_000)));
        cases.push(format!("spiffe://td/ns/d/sa/{}", "s".repeat(1 << 20)));

        for c in &cases {
            if let Some(svc) = spiffe_service(c) {
                // The result is always a non-empty substring of the input.
                assert!(!svc.is_empty());
                assert!(c.contains(svc));
            }
        }
    }
}
