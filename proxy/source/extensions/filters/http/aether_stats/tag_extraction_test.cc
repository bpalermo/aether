#include <map>
#include <string>

#include "envoy/config/metrics/v3/stats.pb.h"

#include "source/common/protobuf/protobuf.h"
#include "source/common/runtime/runtime_features.h"
#include "source/common/stats/allocator.h"
#include "source/common/stats/stat_merger.h"
#include "source/common/stats/symbol_table.h"
#include "source/common/stats/tag_producer_impl.h"
#include "source/common/stats/thread_local_store.h"
#include "source/extensions/filters/http/aether_stats/aether_stats.h"

#include "test/mocks/server/server_factory_context.h"
#include "test/mocks/stream_info/mocks.h"
#include "test/mocks/upstream/cluster_info.h"
#include "test/test_common/utility.h"

#include "absl/strings/match.h"
#include "absl/strings/string_view.h"
#include "gmock/gmock.h"
#include "gtest/gtest.h"

// Guards the invariant that replaced the chart's stats_tags regex workaround
// (aether#695).
//
// History: aether_stats (proposal 012) records via counterFromStatNameWithTags,
// which inlines the tag VALUES into the full stat name
// (aether.requests_total.reporter.<v>.source_service.<v>....). The node proxy
// hot-restarts on every config change (talos-main hit epoch 53), and Envoy's
// StatMerger used to re-create every merged counter via counterFromStatName()
// with NO tags ("TODO(snowp): Propagate tag values during hot restarts"). After
// the first restart the programmatic tags were gone and their values collapsed
// into the metric name with empty labels. charts/aether papered over that with
// one stats_tags regex per programmatic key, re-deriving the tags from the name.
//
// envoyproxy/envoy#45674 (in the pinned Envoy snapshot) makes the hot restart
// parent transmit the tag metadata (hot_restart.proto TaggedMetric /
// counter_tags) and the child re-create the counter via
// counterFromMergedStatName, so the tags survive natively. This test asserts
// exactly that, with NO aether_stats stats_tags regexes configured: what the
// child gets back must be indistinguishable from the fresh, filter-written
// counter. The negative test pins the pre-fix behaviour so the difference the
// propagation makes stays explicit.
namespace Envoy {
namespace Extensions {
namespace HttpFilters {
namespace AetherStats {
namespace {

using testing::NiceMock;
using testing::ReturnRef;

// The child gates the tag metadata on this guard (hot_restarting_child.cc). It
// is a default-true RUNTIME_GUARD in the pinned snapshot. When upstream retires
// the guard, `runtimeFeatureEnabled` will ENVOY_BUG on the unknown name — at
// that point drop TagsLostWhenPropagationDisabled and the RuntimeGuard helper.
constexpr absl::string_view kPropagateTagsFlag =
    "envoy.reloadable_features.hot_restart_propagate_stat_tags";

// Flips a runtime guard for the duration of a test and restores it.
class RuntimeGuard {
public:
  RuntimeGuard(absl::string_view name, bool value)
      : name_(name), previous_(Runtime::runtimeFeatureEnabled(name)) {
    Runtime::maybeSetRuntimeGuard(name_, value);
  }
  ~RuntimeGuard() { Runtime::maybeSetRuntimeGuard(name_, previous_); }

private:
  const absl::string_view name_;
  const bool previous_;
};

// The stats_tags the chart keeps after aether#695: aether.cluster and
// aether.pod only, both extracted from Envoy's OWN stat names. Deliberately no
// regex for any aether_stats programmatic key — that is the invariant under
// test. Envoy tag semantics: the first capture group is removed from the stat
// name, the tag value is the second group.
Stats::TagProducerPtr productionTagProducer() {
  envoy::config::metrics::v3::StatsConfig config;
  config.mutable_use_all_default_tags()->set_value(false);
  const auto add = [&](absl::string_view name, absl::string_view regex) {
    auto* tag = config.add_stats_tags();
    tag->set_tag_name(std::string(name));
    tag->set_regex(std::string(regex));
  };
  add("aether.cluster", "^cluster\\.(([^.]+)\\.)");
  add("aether.pod", "^listener\\.(?:inbound|out_http)(_([^.]+))\\.");
  return Stats::TagProducerImpl::createTagProducer(config, {}).value();
}

// One Envoy process' stats plane: a real thread-local store carrying the
// production tag producer, plus the factory-context mock the filter reads its
// scope from.
struct StatsProcess {
  StatsProcess() {
    store_.setTagProducer(productionTagProducer());
    ON_CALL(context_, scope()).WillByDefault(ReturnRef(*store_.rootScope()));
  }

  Stats::SymbolTableImpl symbol_table_;
  Stats::Allocator alloc_{symbol_table_};
  Stats::ThreadLocalStoreImpl store_{alloc_};
  NiceMock<Server::Configuration::MockServerFactoryContext> context_;
};

class HotRestartTagPropagationTest : public testing::Test {
protected:
  HotRestartTagPropagationTest() {
    stream_info_.response_code_ = 200;
    auto cluster = std::make_shared<NiceMock<Upstream::MockClusterInfo>>();
    cluster->name_ = "payments.aether.internal:8080";
    stream_info_.upstream_cluster_info_ = cluster;
  }

  static ProtoConfig sourceConfig() {
    ProtoConfig proto;
    proto.set_reporter("source");
    proto.set_source_service("checkout");
    proto.set_source_pod("checkout-abc123");
    proto.set_emit_pod(true);
    proto.set_mesh_domain("aether.internal");
    return proto;
  }

  // Records one request through the real filter into `process` and returns the
  // resulting counter. The store starts empty and every call here uses the same
  // tag set, so there is exactly one aether.requests_total counter.
  Stats::CounterSharedPtr record(StatsProcess& process) {
    FilterConfig config(sourceConfig(), process.context_);
    config.record(stream_info_);
    Stats::CounterSharedPtr found;
    for (const auto& counter : process.store_.counters()) {
      if (absl::StrContains(counter->name(), "aether.requests_total")) {
        EXPECT_EQ(found, nullptr) << "more than one aether.requests_total counter";
        found = counter;
      }
    }
    return found;
  }

  static std::map<std::string, std::string> tagsOf(const Stats::Metric& metric) {
    std::map<std::string, std::string> tags;
    for (const auto& tag : metric.tags()) {
      tags[tag.name_] = tag.value_;
    }
    return tags;
  }

  // Builds the hot restart payload the parent sends for `counter` and merges it
  // into the child store, exactly as HotRestartingParent::exportStatsToChild and
  // HotRestartingChild::mergeParentStats do — the flat name and its delta, the
  // dynamic spans, and (gated on the runtime guard) the tag metadata.
  //
  // The dynamic spans matter here: the filter interns tag VALUES through a
  // StatNameDynamicPool, so the counter's StatName is a mix of symbolic and
  // dynamic segments. Merging the flat name without the spans would build an
  // all-symbolic StatName, which is a different stat from the one the filter
  // writes — the child would end up with two counters.
  void mergeIntoChild(Stats::StatMerger& merger, const Stats::Counter& counter,
                      const std::string& full_name) {
    Protobuf::Map<std::string, uint64_t> counter_deltas;
    counter_deltas[full_name] = counter.value();

    Stats::StatMerger::DynamicsMap dynamics;
    dynamics[full_name] = parent_.store_.symbolTable().getDynamicSpans(counter.statName());

    Stats::StatMerger::TagsMap counter_tags;
    if (Runtime::runtimeFeatureEnabled(kPropagateTagsFlag)) {
      Stats::StatMerger::ParentTags& parent_tags = counter_tags[full_name];
      parent_tags.base_name_ = counter.tagExtractedName();
      for (const auto& tag : counter.tags()) {
        parent_tags.tags_.emplace_back(tag.name_, tag.value_);
      }
    }

    merger.mergeStats(counter_deltas, Protobuf::Map<std::string, uint64_t>(), dynamics,
                      counter_tags, Stats::StatMerger::TagsMap());
  }

  StatsProcess parent_;
  StatsProcess child_;
  NiceMock<StreamInfo::MockStreamInfo> stream_info_;
};

// The counter the filter writes is clean to begin with: the tags are labels, the
// metric name is the bare aether.requests_total, and the values are only
// inlined into the full (flat) name.
TEST_F(HotRestartTagPropagationTest, FreshCounterIsTagged) {
  Stats::CounterSharedPtr fresh = record(parent_);
  ASSERT_NE(fresh, nullptr);
  EXPECT_EQ(fresh->tagExtractedName(), "aether.requests_total");

  const auto tags = tagsOf(*fresh);
  EXPECT_EQ(tags.size(), 7);
  EXPECT_EQ(tags.at("reporter"), "source");
  EXPECT_EQ(tags.at("source_service"), "checkout");
  EXPECT_EQ(tags.at("source_pod"), "checkout-abc123");
  EXPECT_EQ(tags.at("destination_service"), "payments");
  EXPECT_EQ(tags.at("destination_pod"), "");
  EXPECT_EQ(tags.at("response_code"), "200");
  EXPECT_EQ(tags.count("response_flags"), 1);

  // The flat name still carries the values — that inlining is what used to be
  // all that was left after a merge.
  EXPECT_TRUE(absl::StrContains(fresh->name(), ".reporter.source"));
}

// THE invariant: with the tag metadata propagated (guard on by default) and no
// aether_stats stats_tags regexes anywhere, a counter merged across a hot
// restart is indistinguishable from the fresh one — same tag-extracted name,
// same tags — and the child's own write lands on that same counter instead of
// creating a mangled twin.
TEST_F(HotRestartTagPropagationTest, TagsSurviveHotRestartWithoutRegexes) {
  // The pinned snapshot ships the propagation on by default; a re-pin that
  // flipped it would silently reinstate the collapse.
  EXPECT_TRUE(Runtime::runtimeFeatureEnabled(kPropagateTagsFlag));

  Stats::CounterSharedPtr fresh = record(parent_);
  ASSERT_NE(fresh, nullptr);
  const std::string full_name = fresh->name();

  // The merger owns the merged stat until the child touches it, so it has to
  // outlive the assertions below.
  Stats::StatMerger merger(child_.store_);
  mergeIntoChild(merger, *fresh, full_name);

  Stats::CounterSharedPtr merged = TestUtility::findCounter(child_.store_, full_name);
  ASSERT_NE(merged, nullptr);
  EXPECT_EQ(merged->value(), fresh->value());
  EXPECT_EQ(merged->tagExtractedName(), fresh->tagExtractedName());
  EXPECT_EQ(tagsOf(*merged), tagsOf(*fresh));

  // The post-restart write from the child's own filter resolves to the merged
  // counter (no poisoned central-cache slot) and keeps the labels.
  Stats::CounterSharedPtr after_restart = record(child_);
  ASSERT_NE(after_restart, nullptr);
  EXPECT_EQ(after_restart.get(), merged.get());
  EXPECT_EQ(after_restart->tagExtractedName(), "aether.requests_total");
  EXPECT_EQ(tagsOf(*after_restart), tagsOf(*fresh));
  EXPECT_EQ(after_restart->value(), 2); // 1 merged from the parent + 1 local
}

// What the chart regexes used to compensate for: with the propagation disabled
// the parent sends no tag metadata, the merged counter has no tags at all, and
// every tag value stays welded into the metric name (the talos-main symptom —
// series named ...aether.requests_total.reporter.source....).
TEST_F(HotRestartTagPropagationTest, TagsLostWhenPropagationDisabled) {
  RuntimeGuard disabled(kPropagateTagsFlag, false);

  Stats::CounterSharedPtr fresh = record(parent_);
  ASSERT_NE(fresh, nullptr);
  const std::string full_name = fresh->name();

  Stats::StatMerger merger(child_.store_);
  mergeIntoChild(merger, *fresh, full_name);

  Stats::CounterSharedPtr merged = TestUtility::findCounter(child_.store_, full_name);
  ASSERT_NE(merged, nullptr);
  EXPECT_TRUE(merged->tags().empty());
  EXPECT_EQ(merged->tagExtractedName(), full_name);
  EXPECT_TRUE(absl::StrContains(merged->tagExtractedName(), ".reporter."));
}

// The two stats_tags the chart keeps are unrelated to aether_stats: they lift
// Envoy's own cluster/listener name segments out of the stat name. They must
// keep working, and they must NOT touch aether.requests_total.
TEST_F(HotRestartTagPropagationTest, RetainedChartRegexesStillExtract) {
  Stats::TagProducerPtr producer = productionTagProducer();

  Stats::TagVector cluster_tags;
  const std::string cluster_extracted =
      producer->produceTags("cluster.svc-1.upstream_rq_total", cluster_tags);
  EXPECT_EQ(cluster_extracted, "cluster.upstream_rq_total");
  ASSERT_EQ(cluster_tags.size(), 1);
  EXPECT_EQ(cluster_tags[0].name_, "aether.cluster");
  EXPECT_EQ(cluster_tags[0].value_, "svc-1");

  Stats::TagVector listener_tags;
  const std::string listener_extracted =
      producer->produceTags("listener.inbound_svc-1-abc123.downstream_cx_total", listener_tags);
  EXPECT_EQ(listener_extracted, "listener.inbound.downstream_cx_total");
  ASSERT_EQ(listener_tags.size(), 1);
  EXPECT_EQ(listener_tags[0].name_, "aether.pod");
  EXPECT_EQ(listener_tags[0].value_, "svc-1-abc123");

  // No regex re-derives the programmatic keys any more: extraction alone leaves
  // the mangled name untouched. Propagation, not regex, is what recovers them.
  Stats::TagVector request_tags;
  const std::string mangled = "aether.requests_total.reporter.source.source_service.checkout."
                              "source_pod.checkout-abc123.destination_service.payments."
                              "destination_pod..response_code.200.response_flags.-";
  EXPECT_EQ(producer->produceTags(mangled, request_tags), mangled);
  EXPECT_TRUE(request_tags.empty());
}

} // namespace
} // namespace AetherStats
} // namespace HttpFilters
} // namespace Extensions
} // namespace Envoy
