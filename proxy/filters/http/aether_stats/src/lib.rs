//! Aether stats dynamic module (proposal 007, Phase 1: source-reported).
//! Analogous to Istio's istio_stats: an HTTP filter that records a tagged
//! request counter at the log phase.
//!
//! Attached to each per-pod OUTBOUND HCM. The agent passes the local pod's
//! identity (reporter/source_service/source_pod/mesh_domain) as the per-instance
//! `filter_config` JSON. The module derives the destination from the routed
//! cluster name and increments a cumulative Envoy counter vector at request
//! completion — exported by the existing OTel stat sink (no per-request egress).

use envoy_proxy_dynamic_modules_rust_sdk::*;
use serde::Deserialize;

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
    mesh_domain: String,
    requests_total: EnvoyCounterVecId,
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
        Some(Self {
            reporter: c.reporter,
            source_service: c.source_service,
            source_pod: if c.emit_pod {
                c.source_pod
            } else {
                String::new()
            },
            mesh_domain: c.mesh_domain,
            requests_total,
        })
    }
}

impl<EHF: EnvoyHttpFilter> HttpFilterConfig<EHF> for FilterConfig {
    fn new_http_filter(&self, _envoy: &mut EHF) -> Box<dyn HttpFilter<EHF>> {
        Box::new(Filter {
            reporter: self.reporter.clone(),
            source_service: self.source_service.clone(),
            source_pod: self.source_pod.clone(),
            mesh_domain: self.mesh_domain.clone(),
            requests_total: self.requests_total,
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
    mesh_domain: String,
    requests_total: EnvoyCounterVecId,
    dest_cluster: String,
    local_reply_details: String,
    recorded: bool,
}

impl Filter {
    fn record<EHF: EnvoyHttpFilter>(&mut self, envoy: &mut EHF) {
        if self.recorded {
            return;
        }
        self.recorded = true;

        let destination_service = self.dest_service_from_cluster(&self.dest_cluster.clone());

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
                &self.source_service,
                &self.source_pod,
                &destination_service,
                &response_code,
                response_flags,
            ],
            1,
        );
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
        // Cluster is selected by the router after request headers; capture it
        // here via the dedicated getter (the XdsClusterName CEL attribute is
        // not populated in the HTTP-filter context).
        if let Some(b) = envoy.get_cluster_name() {
            self.dest_cluster = String::from_utf8_lossy(b.as_slice()).into_owned();
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
            mesh_domain: "aether.internal".to_string(),
            requests_total: EnvoyCounterVecId(0),
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

    // ---- ConfigData deserialization -------------------------------------

    #[test]
    fn config_defaults_when_empty() {
        let c: ConfigData = serde_json::from_str("{}").unwrap();
        assert_eq!(c.reporter, "source");
        assert_eq!(c.source_service, "");
        assert_eq!(c.source_pod, "");
        assert_eq!(c.mesh_domain, "aether.internal");
        assert!(!c.emit_pod);
    }

    #[test]
    fn config_parses_all_fields() {
        let json = r#"{
            "reporter": "destination",
            "source_service": "cart",
            "source_pod": "cart-7d9",
            "mesh_domain": "example.mesh",
            "emit_pod": true
        }"#;
        let c: ConfigData = serde_json::from_str(json).unwrap();
        assert_eq!(c.reporter, "destination");
        assert_eq!(c.source_service, "cart");
        assert_eq!(c.source_pod, "cart-7d9");
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
    // response_code, response_flags) tuple recorded.
    fn expect_record(mock: &mut MockEnvoyHttpFilter) -> Arc<Mutex<Vec<String>>> {
        let labels = Arc::new(Mutex::new(Vec::new()));
        let sink = labels.clone();
        mock.expect_increment_counter_vec()
            .times(1)
            .returning(move |_id, ls, _value| {
                *sink.lock().unwrap() = ls.iter().map(|s| s.to_string()).collect();
                Ok(())
            });
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
}
