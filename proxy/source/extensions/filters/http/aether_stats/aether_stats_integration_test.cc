#include <string>
#include <utility>
#include <vector>

#include "envoy/config/bootstrap/v3/bootstrap.pb.h"

#include "source/common/common/logger.h"

#include "test/integration/http_integration.h"
#include "test/test_common/utility.h"

#include "absl/strings/match.h"
#include "absl/strings/str_cat.h"
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

// Reproduction: boot Envoy with the SAME bootstrap stats_config the agent ships
// in production (charts/agent/templates/configmap.yaml): use_all_default_tags
// off + custom stats_tags (aether.cluster/aether.pod) + a stats_matcher
// exclusion_list. On talos-main the aether.requests_total counter exports with
// the tags baked into the name and empty labels; this test asserts the tags
// survive as real Stats tags under that config, to localize whether the break is
// at the Envoy counter or downstream in the OTLP->Prometheus pipeline.
TEST_P(AetherStatsIntegrationTest, RecordsRequestCounterUnderProdStatsConfig) {
  config_helper_.addConfigModifier([](envoy::config::bootstrap::v3::Bootstrap& bootstrap) {
    auto* sc = bootstrap.mutable_stats_config();
    sc->mutable_use_all_default_tags()->set_value(false);
    auto* t1 = sc->add_stats_tags();
    t1->set_tag_name("aether.cluster");
    t1->set_regex("^cluster\\.(([^.]+)\\.)");
    auto* t2 = sc->add_stats_tags();
    t2->set_tag_name("aether.pod");
    t2->set_regex("^listener\\.(?:inbound|out_http)(_([^.]+))\\.");
    // The aether_stats hot-restart fallback regexes (see configmap.yaml). On the
    // fresh write path the programmatic tags win (these run only on the no-tags
    // name), so the counter must still carry exactly the 7 real tags.
    for (const auto& [name, re] : std::vector<std::pair<std::string, std::string>>{
             {"reporter", "(\\.reporter\\.([^.]*))"},
             {"source_service", "(\\.source_service\\.([^.]*))"},
             {"source_pod", "(\\.source_pod\\.([^.]*))"},
             {"destination_service", "(\\.destination_service\\.([^.]*))"},
             {"destination_pod", "(\\.destination_pod\\.([^.]*))"},
             {"response_code", "(\\.response_code\\.([^.]*))"},
             {"response_flags", "(\\.response_flags\\.([^.]*))"}}) {
      auto* t = sc->add_stats_tags();
      t->set_tag_name(name);
      t->set_regex(re);
    }
    auto* excl = sc->mutable_stats_matcher()->mutable_exclusion_list();
    excl->add_patterns()->set_contains("ssl.certificate.");
    excl->add_patterns()->mutable_safe_regex()->set_regex("^cluster\\..*\\.health_check\\..*");
  });
  initializeFilter();

  codec_client_ = makeHttpConnection(lookupPort("http"));
  auto response = codec_client_->makeHeaderOnlyRequest(Http::TestRequestHeaderMapImpl{
      {":method", "GET"}, {":path", "/"}, {":scheme", "http"}, {":authority", "host"}});
  waitForNextUpstreamRequest();
  upstream_request_->encodeHeaders(Http::TestResponseHeaderMapImpl{{":status", "200"}}, true);
  ASSERT_TRUE(response->waitForEndStream());

  // Dump every counter that mentions "aether_requests"/"requests_total" so the
  // test log shows exactly what name/tag_extracted_name/tags the counter carries.
  Stats::CounterSharedPtr counter;
  for (const auto& c : test_server_->counters()) {
    if (absl::StrContains(c->name(), "requests_total")) {
      std::string tagstr;
      for (const auto& t : c->tags()) {
        absl::StrAppend(&tagstr, t.name_, "=", t.value_, " ");
      }
      ENVOY_LOG_MISC(critical, "AETHER_REPRO name='{}' tag_extracted='{}' tags=[{}]", c->name(),
                     c->tagExtractedName(), tagstr);
      if (c->tagExtractedName() == "aether.requests_total") {
        counter = c;
      }
    }
  }
  ASSERT_NE(counter, nullptr)
      << "no counter with clean tag_extracted_name 'aether.requests_total' — tags were baked into "
         "the name (see AETHER_REPRO log lines above)";
  EXPECT_EQ(counter->tags().size(), 7) << "expected 7 stats tags on the counter";
}

} // namespace
} // namespace AetherStats
} // namespace HttpFilters
} // namespace Extensions
} // namespace Envoy
