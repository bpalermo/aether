#include "source/extensions/filters/http/aether_stats/aether_stats.h"

#include <string>
#include <vector>

#include "envoy/upstream/upstream.h"

#include "source/common/stream_info/utility.h"

#include "absl/strings/match.h"
#include "absl/strings/str_cat.h"
#include "absl/strings/str_split.h"

namespace Envoy {
namespace Extensions {
namespace HttpFilters {
namespace AetherStats {

namespace {

// "spiffe://<trust-domain>/ns/<ns>/sa/<sa>" -> "<sa>". Empty for any URI that is
// not a SPIFFE SVID or carries no non-empty `sa` segment. Untrusted input.
std::string spiffeService(absl::string_view uri_san) {
  constexpr absl::string_view kScheme = "spiffe://";
  if (!absl::StartsWith(uri_san, kScheme)) {
    return "";
  }
  absl::string_view rest = uri_san.substr(kScheme.size());
  const auto slash = rest.find('/'); // drop the authority (trust domain)
  if (slash == absl::string_view::npos) {
    return "";
  }
  const std::vector<absl::string_view> segs = absl::StrSplit(rest.substr(slash + 1), '/');
  for (size_t i = 0; i + 1 < segs.size(); ++i) {
    if (segs[i] == "sa" && !segs[i + 1].empty()) {
      return std::string(segs[i + 1]);
    }
  }
  return "";
}

} // namespace

FilterConfig::FilterConfig(const ProtoConfig& proto,
                           Server::Configuration::ServerFactoryContext& context)
    : is_destination_(proto.reporter() == "destination"),
      source_service_(proto.source_service()),
      source_pod_(proto.emit_pod() ? proto.source_pod() : ""),
      destination_service_(proto.destination_service()),
      mesh_domain_(proto.mesh_domain().empty() ? "aether.internal" : proto.mesh_domain()),
      scope_(context.scope()), pool_(scope_.symbolTable()),
      requests_total_(pool_.add("aether.requests_total")), tag_reporter_(pool_.add("reporter")),
      tag_source_service_(pool_.add("source_service")), tag_source_pod_(pool_.add("source_pod")),
      tag_destination_service_(pool_.add("destination_service")),
      tag_response_code_(pool_.add("response_code")),
      tag_response_flags_(pool_.add("response_flags")) {}

// "<svc>.<mesh_domain>[:port]" -> "<svc>"; empty/foreign -> "unknown".
std::string FilterConfig::destFromCluster(absl::string_view cluster) const {
  if (cluster.empty()) {
    return "unknown";
  }
  absl::string_view no_port = cluster.substr(0, cluster.find(':'));
  const std::string suffix = absl::StrCat(".", mesh_domain_);
  absl::string_view bare = no_port;
  if (absl::EndsWith(no_port, suffix)) {
    bare = no_port.substr(0, no_port.size() - suffix.size());
  }
  return bare.empty() ? "unknown" : std::string(bare);
}

void FilterConfig::record(const StreamInfo::StreamInfo& info) const {
  std::string source_service;
  std::string source_pod;
  std::string destination_service;
  absl::string_view reporter;

  if (is_destination_) {
    // Inbound: destination from config, source parsed from the verified peer SVID.
    reporter = "destination";
    source_service = "unknown";
    const auto ssl = info.downstreamAddressProvider().sslConnection();
    if (ssl != nullptr) {
      const auto sans = ssl->uriSanPeerCertificate();
      if (!sans.empty()) {
        std::string s = spiffeService(sans[0]);
        if (!s.empty()) {
          source_service = std::move(s);
        }
      }
    }
    destination_service = destination_service_.empty() ? "unknown" : destination_service_;
  } else {
    // Outbound: source from config, destination from the routed cluster name.
    reporter = "source";
    source_service = source_service_;
    source_pod = source_pod_;
    absl::string_view cluster;
    const auto cluster_info = info.upstreamClusterInfo();
    if (cluster_info.has_value()) {
      cluster = cluster_info->name();
    }
    destination_service = destFromCluster(cluster);
  }

  const std::string response_code =
      info.responseCode().has_value() ? absl::StrCat(info.responseCode().value()) : "0";
  // Full access: the real StreamInfo response flags, in the same UF/UH/NC/...
  // vocabulary the dynamic module had to reproduce from on_local_reply details.
  const std::string response_flags = StreamInfo::ResponseFlagUtils::toShortString(info);

  Stats::StatNameDynamicPool dyn(scope_.symbolTable());
  const Stats::StatNameTagVector tags{
      {tag_reporter_, dyn.add(reporter)},
      {tag_source_service_, dyn.add(source_service)},
      {tag_source_pod_, dyn.add(source_pod)},
      {tag_destination_service_, dyn.add(destination_service)},
      {tag_response_code_, dyn.add(response_code)},
      {tag_response_flags_, dyn.add(response_flags)},
  };
  scope_.counterFromStatNameWithTags(requests_total_, tags).inc();
}

} // namespace AetherStats
} // namespace HttpFilters
} // namespace Extensions
} // namespace Envoy
