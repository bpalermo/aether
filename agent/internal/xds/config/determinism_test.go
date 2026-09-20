package config

import (
	"fmt"
	"testing"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// configSourceMarshalRuns is how many independent constructions are compared.
// Go randomises map iteration per range, not per process, so one extra build
// would not reliably expose a two-entry map; twelve makes an accidental
// agreement vanishingly unlikely.
const configSourceMarshalRuns = 12

// hashedConfigSources are every ConfigSource constructor in this package whose
// output ends up inside a message Envoy HASHES to decide whether two config
// consumers may share one subscription (issue #852). The map value is a
// constructor rather than a value because the whole point is that two
// INDEPENDENT constructions must agree.
//
// The arguments are the ones aether actually passes: 15s is
// proxy.OutboundRouteInitialFetchTimeout (not importable here — proxy depends on
// config, not the other way round) and "spire_agent" is the edge bootstrap
// cluster name.
func hashedConfigSources() map[string]func() *corev3.ConfigSource {
	return map[string]func() *corev3.ConfigSource{
		"XDSConfigSourceADS": XDSConfigSourceADS,
		"XDSConfigSourceADSWithInitialFetch": func() *corev3.ConfigSource {
			return XDSConfigSourceADSWithInitialFetch(15 * time.Second)
		},
		"SDSConfigSourceFromCluster": func() *corev3.ConfigSource {
			return SDSConfigSourceFromCluster("spire_agent")
		},
	}
}

// TestConfigSourceBytesIdenticalAcrossConstructions is the #852 guard: every
// ConfigSource this package hands out must serialise to the SAME bytes on every
// construction, because Envoy keys subscription sharing on a hash of the message
// carrying it, not on the resource name.
//
// Concretely, for RDS: RouteConfigProviderManager::addDynamicProvider keys
// provider reuse on a hash of the entire Rds message and matches on that hash
// alone. Every meshed pod's egress listener emits its own copy of that Rds; they
// collapse onto one shared provider only because all those copies hash alike. A
// config_source that varied per construction would give each listener its own
// provider, subscription and route table, with no NACK and no warning. See
// XDSConfigSourceADS in common.go for the full contract, the other hash-keyed
// sites, and what a split actually costs.
//
// Envoy's hash is a value hash, not a byte hash, so what this test catches that
// Envoy would also catch is a REPEATED field whose order varies —
// ConfigSource.authorities, or api_config_source's grpc_services /
// initial_metadata / config_validators, any of which a future caller could
// assemble by ranging a Go map. That is #135's shape exactly, and it is the
// hazard that reaches the data plane.
//
// Why a PLAIN proto.Marshal and not proto.MarshalOptions{Deterministic: true},
// which is what the cache's TestSnapshotDeterministic_ShuffledInputOrder uses:
// Deterministic canonicalises map fields, so a map-typed field added to one of
// these messages would still compare equal here and this test would go quiet on
// half of what it is for. The deterministic marshal is the right tool when the
// input order is the variable; here the value is supposed to be a CONSTANT, so
// the strictest available check is the correct one.
func TestConfigSourceBytesIdenticalAcrossConstructions(t *testing.T) {
	for name, build := range hashedConfigSources() {
		t.Run(name, func(t *testing.T) {
			var want []byte
			for run := range configSourceMarshalRuns {
				got, err := proto.Marshal(build())
				require.NoError(t, err)
				require.NotEmptyf(t, got, "%s serialised to nothing — the fixture proves nothing", name)
				if run == 0 {
					want = got
					continue
				}
				require.Equalf(t, want, got,
					"%s produced different bytes on construction %d: Envoy hashes this message to "+
						"decide subscription reuse, so a non-constant ConfigSource silently splits "+
						"one shared provider into one per consumer (issue #852)",
					name, run)
			}
		})
	}
}

// TestConfigSourceCarriesNoMapField closes the gap the byte comparison above
// cannot: a map with a single entry serialises identically every time, so a
// newly added one-entry map field would sail straight through it. This walks the
// POPULATED message tree instead and fails on any set map-typed field whatever
// its size.
//
// Be honest about what this defends. Envoy itself is immune — MessageUtil::hash
// folds map entries with addition precisely so order cannot matter. The exposure
// is one layer out, at go-control-plane, which versions a snapshot resource by
// hashing its marshalled bytes; a map serialised in Go's randomised order there
// makes an unchanged resource look changed on every push, which is incident
// #135's mechanism. Both marshals that currently carry a ConfigSource
// (config.TypedConfig, cache MarshalResource) set Deterministic and canonicalise
// maps, so today nothing leaks. This keeps the invariant from depending on that
// remaining true for whatever emits a ConfigSource next.
//
// Populated, not declared, is deliberate: corev3.ConfigSource's descriptor tree
// reaches google_grpc.channel_args.args and other maps Envoy allows and aether
// never sets. The contract is about the values these constructors emit, not
// about what the proto permits.
func TestConfigSourceCarriesNoMapField(t *testing.T) {
	for name, build := range hashedConfigSources() {
		t.Run(name, func(t *testing.T) {
			requireNoPopulatedMapField(t, build().ProtoReflect(), name)
		})
	}
}

// requireNoPopulatedMapField fails if any set field reachable from m is a proto
// map, reporting the full field path so the offender is obvious.
func requireNoPopulatedMapField(t *testing.T, m protoreflect.Message, path string) {
	t.Helper()
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		child := fmt.Sprintf("%s.%s", path, fd.Name())
		switch {
		// IsMap must be tested first: a map field is also a repeated message
		// field as far as IsList is concerned.
		case fd.IsMap():
			t.Errorf("%s is a populated map field: proto maps have no stable serialisation order, "+
				"so any marshal of this ConfigSource that does not set Deterministic re-versions "+
				"the enclosing resource on every push (issue #852, incident #135). "+
				"A ConfigSource must stay a deterministic constant.", child)
		case fd.IsList():
			if fd.Message() == nil {
				return true
			}
			list := v.List()
			for i := range list.Len() {
				requireNoPopulatedMapField(t, list.Get(i).Message(), fmt.Sprintf("%s[%d]", child, i))
			}
		case fd.Message() != nil:
			requireNoPopulatedMapField(t, v.Message(), child)
		}
		return true
	})
}
