#include <map>
#include <string>

#include "envoy/config/metrics/v3/stats.pb.h"

#include "source/common/stats/tag_producer_impl.h"

#include "gtest/gtest.h"

namespace Envoy {
namespace Stats {
namespace {

// Builds the production stats tag producer (mirrors the stats_tags in
// charts/agent/templates/configmap.yaml) and asserts the aether_stats tags are
// recovered by regex from the full counter name.
//
// Why this matters: the aether_stats filter records via
// counterFromStatNameWithTags, which inlines the tag VALUES into the full stat
// name. Across a hot restart (the node proxy hot-restarts on every config
// change — talos-main was at epoch 53), Envoy's StatMerger re-materialises the
// counter via counterFromStatName(full_name) with NO tags
// ("TODO(snowp): Propagate tag values during hot restarts" in stat_merger.cc).
// The only thing that then recovers reporter/source_service/... as real tags is
// regex extraction — exactly how aether.cluster already survives. Without these
// regexes the tags collapse into the metric name with empty labels, which is the
// talos-main symptom.
TagProducerPtr prodTagProducer() {
  envoy::config::metrics::v3::StatsConfig config;
  config.mutable_use_all_default_tags()->set_value(false);
  const auto add = [&](absl::string_view name, absl::string_view re) {
    auto* t = config.add_stats_tags();
    t->set_tag_name(std::string(name));
    t->set_regex(std::string(re));
  };
  // Pre-existing tags.
  add("aether.cluster", "^cluster\\.(([^.]+)\\.)");
  add("aether.pod", "^listener\\.(?:inbound|out_http)(_([^.]+))\\.");
  // aether_stats tags — names MUST match the programmatic tag keys in
  // aether_stats.cc so a fresh (epoch-0) counter and a hot-restart-merged
  // counter export identical labels.
  add("reporter", "(\\.reporter\\.([^.]*))");
  add("source_service", "(\\.source_service\\.([^.]*))");
  add("source_pod", "(\\.source_pod\\.([^.]*))");
  add("destination_service", "(\\.destination_service\\.([^.]*))");
  add("response_code", "(\\.response_code\\.([^.]*))");
  add("response_flags", "(\\.response_flags\\.([^.]*))");
  return TagProducerImpl::createTagProducer(config, {}).value();
}

std::map<std::string, std::string> extract(const std::string& name, std::string& tag_extracted) {
  TagVector tags;
  tag_extracted = prodTagProducer()->produceTags(name, tags);
  std::map<std::string, std::string> m;
  for (const auto& t : tags) {
    m[t.name_] = t.value_;
  }
  return m;
}

// Inbound (destination reporter): empty source_pod, response_flags "-".
TEST(AetherStatsTagExtraction, RecoversDestinationTags) {
  std::string extracted;
  auto m = extract("aether.requests_total.reporter.destination.source_service.loadgen.source_pod."
                   ".destination_service.svc-1.response_code.200.response_flags.-",
                   extracted);
  EXPECT_EQ(extracted, "aether.requests_total");
  EXPECT_EQ(m["reporter"], "destination");
  EXPECT_EQ(m["source_service"], "loadgen");
  EXPECT_EQ(m["source_pod"], "");
  EXPECT_EQ(m["destination_service"], "svc-1");
  EXPECT_EQ(m["response_code"], "200");
  EXPECT_EQ(m["response_flags"], "-");
}

// Outbound (source reporter): populated source_pod.
TEST(AetherStatsTagExtraction, RecoversSourceTags) {
  std::string extracted;
  auto m = extract("aether.requests_total.reporter.source.source_service.checkout.source_pod."
                   "checkout-abc123.destination_service.payments.response_code.503.response_flags.UF",
                   extracted);
  EXPECT_EQ(extracted, "aether.requests_total");
  EXPECT_EQ(m["reporter"], "source");
  EXPECT_EQ(m["source_service"], "checkout");
  EXPECT_EQ(m["source_pod"], "checkout-abc123");
  EXPECT_EQ(m["destination_service"], "payments");
  EXPECT_EQ(m["response_code"], "503");
  EXPECT_EQ(m["response_flags"], "UF");
}

// The new regexes must not perturb unrelated stats (e.g. the cluster path that
// already works) — aether.cluster still extracts and nothing else matches.
TEST(AetherStatsTagExtraction, LeavesClusterStatsIntact) {
  std::string extracted;
  auto m = extract("cluster.svc-1.upstream_rq_total", extracted);
  EXPECT_EQ(extracted, "cluster.upstream_rq_total");
  EXPECT_EQ(m["aether.cluster"], "svc-1");
  EXPECT_EQ(m.count("reporter"), 0);
}

} // namespace
} // namespace Stats
} // namespace Envoy
