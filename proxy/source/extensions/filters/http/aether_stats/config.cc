#include "source/extensions/filters/http/aether_stats/config.h"

#include <memory>

#include "envoy/registry/registry.h"

#include "source/extensions/filters/http/aether_stats/aether_stats.h"

namespace Envoy {
namespace Extensions {
namespace HttpFilters {
namespace AetherStats {

absl::StatusOr<Http::FilterFactoryCb> AetherStatsFilterFactory::createFilterFactoryFromProto(
    const Protobuf::Message& proto_config, const std::string&,
    Server::Configuration::FactoryContext& context) {
  const auto& proto = dynamic_cast<const ProtoConfig&>(proto_config);
  auto config = std::make_shared<FilterConfig>(proto, context.serverFactoryContext());
  return [config](Http::FilterChainFactoryCallbacks& callbacks) -> void {
    callbacks.addStreamFilter(std::make_shared<Filter>(config));
  };
}

ProtobufTypes::MessagePtr AetherStatsFilterFactory::createEmptyConfigProto() {
  return std::make_unique<ProtoConfig>();
}

// Compiled into the custom Envoy binary; the agent attaches the filter by the
// config proto type. Registers under factory.name() only —
// LEGACY_REGISTER_FACTORY adds a *second* (deprecated) name, which aborts at
// static init with "Double registration" when that name equals name() (the
// Envoy binary won't boot).
REGISTER_FACTORY(AetherStatsFilterFactory, Server::Configuration::NamedHttpFilterConfigFactory);

} // namespace AetherStats
} // namespace HttpFilters
} // namespace Extensions
} // namespace Envoy
