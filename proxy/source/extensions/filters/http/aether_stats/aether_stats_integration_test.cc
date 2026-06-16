#include <string>

#include "test/integration/http_integration.h"
#include "test/test_common/utility.h"

#include "gtest/gtest.h"

namespace Envoy {
namespace Extensions {
namespace HttpFilters {
namespace AetherStats {
namespace {

// Boots a real Envoy with the aether_stats filter wired exactly as the agent
// emits it (an xds.type.v3.TypedStruct typed_config whose inner type is the
// native extension's Config proto), sends a request through it, and asserts the
// native counter increments.
//
// This is the guard that the unit test and the tar-driver image_test cannot
// provide: it actually starts the binary (a "Double registration" static-init
// abort fails here — the bug that broke the first in-vivo roll) and resolves
// the filter factory by config type from the TypedStruct (a missing/renamed
// factory or a config-field mismatch fails here too).
class AetherStatsIntegrationTest : public testing::TestWithParam<Network::Address::IpVersion>,
                                   public HttpIntegrationTest {
public:
  AetherStatsIntegrationTest() : HttpIntegrationTest(Http::CodecType::HTTP1, GetParam()) {}

  void initializeFilter() {
    // Mirrors agent/internal/xds/proxy/stats_filter.go: source reporter,
    // identity in the per-instance config, destination derived from the routed
    // cluster.
    config_helper_.prependFilter(R"EOF(
name: aether.filters.http.aether_stats
typed_config:
  "@type": type.googleapis.com/xds.type.v3.TypedStruct
  type_url: type.googleapis.com/aether.filters.http.aether_stats.v3.Config
  value:
    reporter: source
    source_service: checkout
    mesh_domain: aether.internal
)EOF");
    initialize();
  }
};

INSTANTIATE_TEST_SUITE_P(IpVersions, AetherStatsIntegrationTest,
                         testing::ValuesIn(TestEnvironment::getIpVersionsForTest()));

TEST_P(AetherStatsIntegrationTest, RecordsRequestCounter) {
  initializeFilter();

  codec_client_ = makeHttpConnection(lookupPort("http"));
  auto response = codec_client_->makeHeaderOnlyRequest(Http::TestRequestHeaderMapImpl{
      {":method", "GET"}, {":path", "/"}, {":scheme", "http"}, {":authority", "host"}});

  waitForNextUpstreamRequest();
  upstream_request_->encodeHeaders(Http::TestResponseHeaderMapImpl{{":status", "200"}}, true);

  ASSERT_TRUE(response->waitForEndStream());
  EXPECT_TRUE(upstream_request_->complete());
  EXPECT_EQ("200", response->headers().getStatusValue());

  // The filter records at stream completion. Find the lone
  // aether.requests_total counter (tagged, so look it up by tag-extracted name)
  // and assert it fired.
  Stats::CounterSharedPtr counter;
  for (const auto& c : test_server_->counters()) {
    if (c->tagExtractedName() == "aether.requests_total") {
      counter = c;
      break;
    }
  }
  ASSERT_NE(counter, nullptr) << "aether.requests_total counter was not created";
  EXPECT_GE(counter->value(), 1);
}

} // namespace
} // namespace AetherStats
} // namespace HttpFilters
} // namespace Extensions
} // namespace Envoy
