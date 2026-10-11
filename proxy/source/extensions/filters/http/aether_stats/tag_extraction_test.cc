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
// hot-restarts on every config change (the reference cluster hit epoch 53), and Envoy's
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
//
// The file also pins the *other* half of the hot restart contract, which the
// tag fix does not change and which aether#708 mistook for a regression:
// counters transfer as DELTAS (`counter.latch()`), so a child's counter value
// is per-generation by design. See CountersTransferAsDeltasNotAbsoluteValues.
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

// The stats_tags the chart ships (stats_config.stats_tags in
// charts/aether/templates/agent-proxy-configmap.yaml): aether.cluster,
// aether.namespace, aether.pod and aether.transport_socket_match, all extracted
// from Envoy's OWN stat names. Deliberately no regex for any aether_stats
// programmatic key — that is the invariant under test. Envoy tag semantics: the
// first capture group is removed from the stat name, the tag value is the
// second group.
//
// The four add() lines below are compared with that template, text for text, by
// //scripts:proxy_stats_tags_test in the root workspace: change them together.
Stats::TagProducerPtr productionTagProducer() {
  envoy::config::metrics::v3::StatsConfig config;
  config.mutable_use_all_default_tags()->set_value(false);
  const auto add = [&](absl::string_view name, absl::string_view regex) {
    auto* tag = config.add_stats_tags();
    tag->set_tag_name(std::string(name));
    tag->set_regex(std::string(regex));
  };
  add("aether.cluster", "^cluster\\.(([^.]+)\\.)");
  // The listener stat prefixes are inbound_<namespace>_<pod> and
  // out_http_<namespace>_<pod> since aether#1584. Both extractors REQUIRE the
  // namespace part since chart 2.6.0; chart 2.5.x made it optional in the pod
  // extractor, for an agent older than aether#1584.
  add("aether.namespace", "^listener\\.(?:inbound|out_http)(_([^_.]+))_[^.]+\\.");
  add("aether.pod", "^listener\\.(?:inbound|out_http)_[^_.]+(_([^.]+))\\.");
  add("aether.transport_socket_match", "\\.((spiffe://.+)\\.)total_match_count$");
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
  // The delta is `counter.latch()`, not `counter.value()`: the parent ships the
  // increment pending since its last stats flush, and StatMerger .add()s it.
  // (The real parent additionally skips counters whose latched value is 0.)
  //
  // The dynamic spans matter here: the filter interns tag VALUES through a
  // StatNameDynamicPool, so the counter's StatName is a mix of symbolic and
  // dynamic segments. Merging the flat name without the spans would build an
  // all-symbolic StatName, which is a different stat from the one the filter
  // writes — the child would end up with two counters.
  void mergeIntoChild(Stats::StatMerger& merger, Stats::Counter& counter,
                      const std::string& full_name) {
    Protobuf::Map<std::string, uint64_t> counter_deltas;
    counter_deltas[full_name] = counter.latch();

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

// The other half of the hot restart contract, and the one that surprises
// readers of `aether_requests_total` (aether#708): counters transfer as
// DELTAS, never as absolute values.
//
// HotRestartingParent::exportStatsToChild sends `counter.latch()` — the
// increment pending since the parent's last stats flush — and
// StatMerger::mergeCounters `.add()`s it onto the child's counter. The parent
// has been latching that pending increment away every `stats_flush_interval`
// for its whole life (5s default; charts/aether sets none), so the child
// inherits only the residual of the last flush window, not the parent's
// lifetime total. Combined with an OTLP sink that exports `counter.value()` as
// CUMULATIVE from *this process's* start, every aether_stats series restarts
// near zero on each hot restart. That is Envoy behaving correctly, so the rule
// for consumers is: never read a raw counter value across a roll —
// rate()/increase() only.
//
// If this ever fails because the child inherited the parent's full value,
// something started shipping absolute counters and the delta-consuming sinks
// (statsd, metrics_service) are being double-counted.
TEST_F(HotRestartTagPropagationTest, CountersTransferAsDeltasNotAbsoluteValues) {
  // The parent serves a long life of traffic (the filter's own .inc(), just
  // more of it).
  Stats::CounterSharedPtr fresh = record(parent_);
  ASSERT_NE(fresh, nullptr);
  fresh->add(2);
  ASSERT_EQ(fresh->value(), 3UL);

  // The parent's periodic flush (MetricSnapshotImpl, every
  // stats_flush_interval) latches the pending increment away. flushMetricsToSinks
  // even notes that hot restart depends on this latching.
  EXPECT_EQ(fresh->latch(), 3UL);

  // One more request accrues *after* that flush: this is all the parent still
  // has pending when the child asks for its stats.
  fresh->inc();
  ASSERT_EQ(fresh->value(), 4UL);

  const std::string full_name = fresh->name();
  Stats::StatMerger merger(child_.store_);
  mergeIntoChild(merger, *fresh, full_name);

  Stats::CounterSharedPtr merged = TestUtility::findCounter(child_.store_, full_name);
  ASSERT_NE(merged, nullptr);
  // The post-latch residual only — NOT the parent's 4.
  EXPECT_EQ(merged->value(), 1UL);
  EXPECT_LT(merged->value(), fresh->value());

  // Delta semantics are orthogonal to tag propagation: the labels still survive.
  EXPECT_EQ(merged->tagExtractedName(), "aether.requests_total");
  EXPECT_EQ(tagsOf(*merged), tagsOf(*fresh));

  // And the child's own traffic keeps accumulating on that same counter, from
  // the inherited residual — which is why rate()/increase() over the child's
  // series is correct even though its absolute value is meaningless.
  Stats::CounterSharedPtr after_restart = record(child_);
  ASSERT_NE(after_restart, nullptr);
  EXPECT_EQ(after_restart.get(), merged.get());
  EXPECT_EQ(after_restart->value(), 2);
}

// What the chart regexes used to compensate for: with the propagation disabled
// the parent sends no tag metadata, the merged counter has no tags at all, and
// every tag value stays welded into the metric name (the reference-cluster symptom —
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

// The stats_tags the chart ships are unrelated to aether_stats: they lift
// Envoy's own cluster/listener name segments out of the stat name. They must
// keep working, and they must NOT touch aether.requests_total.
TEST_F(HotRestartTagPropagationTest, RetainedChartRegexesStillExtract) {
  Stats::TagProducerPtr producer = productionTagProducer();

  // What the producer makes of one stat name: the tag-extracted name and the
  // tags, by name.
  using Tags = std::map<std::string, std::string>;
  struct Extracted {
    std::string name_;
    Tags tags_;
  };
  const auto extract = [&producer](const std::string& stat_name) {
    Stats::TagVector tags;
    Extracted extracted;
    extracted.name_ = producer->produceTags(stat_name, tags);
    for (const Stats::Tag& tag : tags) {
      EXPECT_TRUE(extracted.tags_.emplace(tag.name_, tag.value_).second)
          << "tag " << tag.name_ << " extracted twice from " << stat_name;
    }
    return extracted;
  };

  const Extracted cluster = extract("cluster.svc-1.upstream_rq_total");
  EXPECT_EQ(cluster.name_, "cluster.upstream_rq_total");
  EXPECT_EQ(cluster.tags_, (Tags{{"aether.cluster", "svc-1"}}));

  // A SPIFFE-ID-named transport socket match under a cluster: both extractors
  // take their own segment and its trailing dot, and the name keeps no double
  // dot.
  const Extracted socket_match =
      extract("cluster.svc-1.spiffe://aether.internal/ns/shop/sa/web.total_match_count");
  EXPECT_EQ(socket_match.name_, "cluster.total_match_count");
  EXPECT_EQ(socket_match.tags_,
            (Tags{{"aether.cluster", "svc-1"},
                  {"aether.transport_socket_match", "spiffe://aether.internal/ns/shop/sa/web"}}));

  // Since aether#1584 the agent writes inbound_<namespace>_<pod> and
  // out_http_<namespace>_<pod>: two tags, and one collapsed name per family.
  const Extracted inbound = extract("listener.inbound_shop_svc-1-abc123.downstream_cx_total");
  EXPECT_EQ(inbound.name_, "listener.inbound.downstream_cx_total");
  EXPECT_EQ(inbound.tags_, (Tags{{"aether.namespace", "shop"}, {"aether.pod", "svc-1-abc123"}}));

  const Extracted outbound = extract("listener.out_http_shop_svc-1-abc123.downstream_cx_total");
  EXPECT_EQ(outbound.name_, "listener.out_http.downstream_cx_total");
  EXPECT_EQ(outbound.tags_, (Tags{{"aether.namespace", "shop"}, {"aether.pod", "svc-1-abc123"}}));

  // The pod's QUIC inbound listener, inbound_<namespace>_<pod>_h3: the "_h3"
  // stays in the pod tag, which is all that tells a pod's two inbound listeners
  // apart once the name no longer carries the pod.
  const Extracted quic = extract("listener.inbound_shop_svc-1-abc123_h3.downstream_cx_total");
  EXPECT_EQ(quic.name_, "listener.inbound.downstream_cx_total");
  EXPECT_EQ(quic.tags_, (Tags{{"aether.namespace", "shop"}, {"aether.pod", "svc-1-abc123_h3"}}));

  // A pod name may contain dots, and a dot is the stat name separator: the
  // agent writes each one as '~' in a stat name (aether#1636), so the whole pod
  // name is one segment and comes out as the tag value, tildes included.
  const Extracted dotted = extract("listener.inbound_team-a_web~external-7d9f.downstream_cx_total");
  EXPECT_EQ(dotted.name_, "listener.inbound.downstream_cx_total");
  EXPECT_EQ(dotted.tags_,
            (Tags{{"aether.namespace", "team-a"}, {"aether.pod", "web~external-7d9f"}}));

  // The prefix an agent older than aether#1584 writes, inbound_<pod>, has no
  // namespace part. Chart 2.5.x still read it as aether.pod; since chart 2.6.0
  // neither extractor matches it, so the name is left as it is, pod and all,
  // and carries no tag. (That agent is outside the compatibility window.)
  const std::string unnamespaced = "listener.inbound_svc-1-abc123.downstream_cx_total";
  const Extracted old_agent = extract(unnamespaced);
  EXPECT_EQ(old_agent.name_, unnamespaced);
  EXPECT_TRUE(old_agent.tags_.empty());

  // The same agent's QUIC listener, inbound_<pod>_h3, does have an underscore
  // after the prefix, and reads as namespace "<pod>", pod "h3": the documented
  // misreading of an unsupported agent, pinned so a change to it is seen.
  const Extracted old_agent_quic = extract("listener.inbound_svc-1-abc123_h3.downstream_cx_total");
  EXPECT_EQ(old_agent_quic.name_, "listener.inbound.downstream_cx_total");
  EXPECT_EQ(old_agent_quic.tags_,
            (Tags{{"aether.namespace", "svc-1-abc123"}, {"aether.pod", "h3"}}));

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
