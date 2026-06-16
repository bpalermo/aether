#pragma once

#include <string>

#include "envoy/server/filter_config.h"

namespace Envoy {
namespace Extensions {
namespace HttpFilters {
namespace AetherStats {

// Config factory for envoy.filters.http.aether_stats. Implements
// NamedHttpFilterConfigFactory directly (rather than Common::FactoryBase) so the
// config proto needs no protoc-gen-validate sidecar.
class AetherStatsFilterFactory : public Server::Configuration::NamedHttpFilterConfigFactory {
public:
  absl::StatusOr<Http::FilterFactoryCb>
  createFilterFactoryFromProto(const Protobuf::Message& proto_config,
                               const std::string& stats_prefix,
                               Server::Configuration::FactoryContext& context) override;

  ProtobufTypes::MessagePtr createEmptyConfigProto() override;

  std::string name() const override { return "aether.filters.http.aether_stats"; }
};

} // namespace AetherStats
} // namespace HttpFilters
} // namespace Extensions
} // namespace Envoy
