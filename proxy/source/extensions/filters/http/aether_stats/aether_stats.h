#pragma once

#include <string>

#include "envoy/server/factory_context.h"
#include "envoy/stats/scope.h"
#include "envoy/stats/stats.h"
#include "envoy/stream_info/stream_info.h"

#include "source/common/stats/symbol_table.h"
#include "source/extensions/filters/http/common/pass_through_filter.h"

#include "source/extensions/filters/http/aether_stats/aether_stats.pb.h"

namespace Envoy {
namespace Extensions {
namespace HttpFilters {
namespace AetherStats {

using ProtoConfig = aether::filters::http::aether_stats::v3::Config;

// Shared, per-filter-chain config. Holds the agent-injected identity and the
// interned stat names; records one tagged counter per request at log time.
//
// Native C++ replacement for the aether_stats dynamic module (proposal 012):
// full StreamInfo access means response flags come from
// StreamInfo::ResponseFlagUtils::toShortString (no on_local_reply workaround) and
// the counter lands in the root scope as `aether.requests_total` (not the
// dynamic-modules `dynamicmodulescustom.*` scope).
class FilterConfig {
public:
  FilterConfig(const ProtoConfig& proto, Server::Configuration::ServerFactoryContext& context);

  // Records the request into the counter vector. Called once at stream completion.
  void record(const StreamInfo::StreamInfo& info) const;

private:
  std::string destFromCluster(absl::string_view cluster) const;

  const bool is_destination_;
  const std::string source_service_;
  const std::string source_pod_; // already "" when emit_pod is false
  const std::string destination_service_;
  const std::string destination_pod_; // already "" when emit_pod is false
  const std::string mesh_domain_;

  Stats::Scope& scope_;
  Stats::StatNamePool pool_;
  const Stats::StatName requests_total_;
  // Tag keys (values are interned per-request via a StatNameDynamicPool).
  const Stats::StatName tag_reporter_;
  const Stats::StatName tag_source_service_;
  const Stats::StatName tag_source_pod_;
  const Stats::StatName tag_destination_service_;
  const Stats::StatName tag_destination_pod_;
  const Stats::StatName tag_response_code_;
  const Stats::StatName tag_response_flags_;
};

using FilterConfigSharedPtr = std::shared_ptr<FilterConfig>;

class Filter : public Http::PassThroughFilter {
public:
  explicit Filter(FilterConfigSharedPtr config) : config_(std::move(config)) {}

  // Http::StreamFilterBase
  void onStreamComplete() override { config_->record(decoder_callbacks_->streamInfo()); }

private:
  const FilterConfigSharedPtr config_;
};

} // namespace AetherStats
} // namespace HttpFilters
} // namespace Extensions
} // namespace Envoy
