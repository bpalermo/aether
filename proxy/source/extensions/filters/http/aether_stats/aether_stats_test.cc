#include <string>
#include <vector>

#include "source/common/stats/isolated_store_impl.h"
#include "source/extensions/filters/http/aether_stats/aether_stats.h"

#include "test/mocks/server/server_factory_context.h"
#include "test/mocks/ssl/mocks.h"
#include "test/mocks/stream_info/mocks.h"
#include "test/mocks/upstream/cluster_info.h"

#include "absl/types/span.h"
#include "gmock/gmock.h"
#include "gtest/gtest.h"

namespace Envoy {
namespace Extensions {
namespace HttpFilters {
namespace AetherStats {
namespace {

using testing::NiceMock;
using testing::Return;
using testing::ReturnRef;

class AetherStatsTest : public testing::Test {
protected:
  AetherStatsTest() {
    ON_CALL(context_, scope()).WillByDefault(ReturnRef(*scope_));
    stream_info_.response_code_ = 200;
  }

  // "source" reporter: identity is config-supplied, destination derives from the
  // routed cluster name.
  ProtoConfig sourceConfig() {
    ProtoConfig p;
    p.set_reporter("source");
    p.set_source_service("checkout");
    p.set_source_pod("checkout-abc123");
    p.set_emit_pod(true);
    p.set_mesh_domain("aether.internal");
    return p;
  }

  // "destination" reporter: destination is config-supplied, source derives from
  // the verified peer SVID.
  ProtoConfig destConfig() {
    ProtoConfig p;
    p.set_reporter("destination");
    p.set_destination_service("payments");
    p.set_mesh_domain("aether.internal");
    return p;
  }

  void setUpstreamCluster(const std::string& name) {
    auto cluster = std::make_shared<NiceMock<Upstream::MockClusterInfo>>();
    cluster->name_ = name;
    stream_info_.upstream_cluster_info_ = cluster;
  }

  void setPeerUriSan(const std::string& uri) {
    peer_uris_ = {uri};
    auto ssl = std::make_shared<NiceMock<Ssl::MockConnectionInfo>>();
    ON_CALL(*ssl, uriSanPeerCertificate())
        .WillByDefault(Return(absl::Span<const std::string>(peer_uris_)));
    stream_info_.downstream_connection_info_provider_->setSslConnection(ssl);
  }

  // Runs record() once and returns the lone aether.requests_total counter. The
  // store starts empty and each distinct tag set produces a distinct counter, so
  // a single record() call leaves exactly one.
  Stats::CounterSharedPtr recordOnce(const ProtoConfig& proto) {
    FilterConfig config(proto, context_);
    config.record(stream_info_);

    Stats::CounterSharedPtr found;
    for (const auto& c : store_.counters()) {
      if (c->tagExtractedName() == "aether.requests_total") {
        EXPECT_EQ(found, nullptr) << "more than one aether.requests_total counter";
        found = c;
      }
    }
    return found;
  }

  static std::map<std::string, std::string> tagsOf(const Stats::Counter& c) {
    std::map<std::string, std::string> m;
    for (const auto& t : c.tags()) {
      m[t.name_] = t.value_;
    }
    return m;
  }

  Stats::IsolatedStoreImpl store_;
  Stats::ScopeSharedPtr scope_{store_.rootScope()};
  NiceMock<Server::Configuration::MockServerFactoryContext> context_;
  NiceMock<StreamInfo::MockStreamInfo> stream_info_;
  std::vector<std::string> peer_uris_;
};

// Outbound: destination is stripped of the mesh-domain suffix and :port, identity
// (including pod) comes from config.
TEST_F(AetherStatsTest, SourceReporterDerivesDestinationFromCluster) {
  setUpstreamCluster("payments.aether.internal:8080");

  auto counter = recordOnce(sourceConfig());
  ASSERT_NE(counter, nullptr);
  EXPECT_EQ(counter->value(), 1);

  const auto tags = tagsOf(*counter);
  EXPECT_EQ(tags.at("reporter"), "source");
  EXPECT_EQ(tags.at("source_service"), "checkout");
  EXPECT_EQ(tags.at("source_pod"), "checkout-abc123");
  EXPECT_EQ(tags.at("destination_service"), "payments");
  EXPECT_EQ(tags.at("response_code"), "200");
  // No flags set on a clean stream.
  EXPECT_EQ(tags.at("response_flags"),
            StreamInfo::ResponseFlagUtils::toShortString(stream_info_));
}

// emit_pod=false blanks the pod tag even when source_pod is populated.
TEST_F(AetherStatsTest, SourceReporterOmitsPodWhenDisabled) {
  setUpstreamCluster("payments.aether.internal:8080");
  auto proto = sourceConfig();
  proto.set_emit_pod(false);

  auto counter = recordOnce(proto);
  ASSERT_NE(counter, nullptr);
  EXPECT_EQ(tagsOf(*counter).at("source_pod"), "");
}

// No routed cluster -> destination is "unknown".
TEST_F(AetherStatsTest, SourceReporterUnknownDestinationWithoutCluster) {
  auto counter = recordOnce(sourceConfig());
  ASSERT_NE(counter, nullptr);
  EXPECT_EQ(tagsOf(*counter).at("destination_service"), "unknown");
}

// A non-mesh cluster name is kept verbatim (no suffix to strip).
TEST_F(AetherStatsTest, SourceReporterKeepsForeignClusterName) {
  setUpstreamCluster("BlackHoleCluster");
  auto counter = recordOnce(sourceConfig());
  ASSERT_NE(counter, nullptr);
  EXPECT_EQ(tagsOf(*counter).at("destination_service"), "BlackHoleCluster");
}

// Inbound: source service is parsed from the verified peer SPIFFE SVID; the
// destination is config-supplied.
TEST_F(AetherStatsTest, DestinationReporterParsesPeerSvid) {
  setPeerUriSan("spiffe://aether.internal/ns/shop/sa/checkout");

  auto counter = recordOnce(destConfig());
  ASSERT_NE(counter, nullptr);

  const auto tags = tagsOf(*counter);
  EXPECT_EQ(tags.at("reporter"), "destination");
  EXPECT_EQ(tags.at("source_service"), "checkout");
  EXPECT_EQ(tags.at("destination_service"), "payments");
  // Destination reporter never emits a source pod.
  EXPECT_EQ(tags.at("source_pod"), "");
}

// No client certificate -> source service is "unknown".
TEST_F(AetherStatsTest, DestinationReporterUnknownSourceWithoutSvid) {
  auto counter = recordOnce(destConfig());
  ASSERT_NE(counter, nullptr);
  EXPECT_EQ(tagsOf(*counter).at("source_service"), "unknown");
}

// A non-SPIFFE URI SAN does not yield a source service.
TEST_F(AetherStatsTest, DestinationReporterIgnoresNonSpiffeSan) {
  setPeerUriSan("https://example.com/foo");
  auto counter = recordOnce(destConfig());
  ASSERT_NE(counter, nullptr);
  EXPECT_EQ(tagsOf(*counter).at("source_service"), "unknown");
}

} // namespace
} // namespace AetherStats
} // namespace HttpFilters
} // namespace Extensions
} // namespace Envoy
