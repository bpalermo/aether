//! Aether edge-telemetry dynamic module (proposal 007, Phase 1: source-reported).
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
                eprintln!("aether_telemetry: bad filter_config: {e}");
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
                ],
            )
            .ok()?;
        Some(Self {
            reporter: c.reporter,
            source_service: c.source_service,
            source_pod: if c.emit_pod { c.source_pod } else { String::new() },
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

        let _ = envoy.increment_counter_vec(
            self.requests_total,
            &[
                &self.reporter,
                &self.source_service,
                &self.source_pod,
                &destination_service,
                &response_code,
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

    fn on_response_headers(
        &mut self,
        envoy: &mut EHF,
        end_of_stream: bool,
    ) -> abi::envoy_dynamic_module_type_on_http_filter_response_headers_status {
        if end_of_stream {
            self.record(envoy);
        }
        abi::envoy_dynamic_module_type_on_http_filter_response_headers_status::Continue
    }

    fn on_response_body(
        &mut self,
        envoy: &mut EHF,
        end_of_stream: bool,
    ) -> abi::envoy_dynamic_module_type_on_http_filter_response_body_status {
        if end_of_stream {
            self.record(envoy);
        }
        abi::envoy_dynamic_module_type_on_http_filter_response_body_status::Continue
    }
}

fn new_http_filter_config_fn<EC: EnvoyHttpFilterConfig, EHF: EnvoyHttpFilter>(
    envoy_filter_config: &mut EC,
    filter_name: &str,
    filter_config: &[u8],
) -> Option<Box<dyn HttpFilterConfig<EHF>>> {
    let cfg = std::str::from_utf8(filter_config).unwrap_or("{}");
    match filter_name {
        "edge" => FilterConfig::new(cfg, envoy_filter_config)
            .map(|c| Box::new(c) as Box<dyn HttpFilterConfig<EHF>>),
        other => {
            eprintln!("aether_telemetry: unknown filter_name {other}");
            None
        }
    }
}
