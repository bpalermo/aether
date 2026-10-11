# Gateway API conformance — rev22: GATEWAY-HTTP 46/46 and MESH-HTTP Core 7/7 at suite v1.6.3 (2026-10-11)

A twenty-second run of the upstream Kubernetes **Gateway API conformance suite**
(`sigs.k8s.io/gateway-api/conformance` @ **v1.6.3**) against **the reference cluster**,
now at aether chart **2.5.8**, commit **`e950d3c`**. It is the first real-cluster run
since rev21 (suite v1.5.1, aether 0.53.0) and the first one driven by the committed
runner in `test/conformance/` instead of an uncommitted one-off. Both profiles pass
with zero failures: GATEWAY-HTTP Core **36/36** and Extended **10/10**, MESH-HTTP Core
**7/7**. The counts are the ones the same suite version gives on kind, and the tests
that pass are the same tests.

Aether was not modified, rolled or reconfigured for this run: the suite ran against
the release that was already deployed, and removed everything it created.

## TL;DR — GATEWAY-HTTP Core 36/36 + Extended 10/10; MESH-HTTP Core 7/7; zero failures at suite v1.6.3

| Profile | Level | Passed | Failed | Skipped | rev21 (suite v1.5.1) | CI on kind (suite v1.6.3) |
|---|---|---|---|---|---|---|
| **GATEWAY-HTTP** | Core | **36** | **0** | 0 | 33 / 0 | 36 / 0 |
| **GATEWAY-HTTP** | Extended | **10** | **0** | 0 | 10 / 0 | 10 / 0 |
| **MESH-HTTP** | Core | **7** | **0** | 0 | 4 / 3 | 7 / 0 |

**Headline:** both profiles are clean on a real cluster at the suite version CI pins.
Test output: `--- PASS: TestAetherGatewayHTTP (92.49s)`, `"Core tests succeeded.
Extended tests succeeded."`; `--- PASS: TestAetherMeshHTTP (41.80s)`, `"Core tests
succeeded."`

> Do not read this doc as a claim that every Gateway API feature is implemented: the
> two profiles pass at the features aether advertises, and the 29 Extended features it
> does not advertise are skipped, not passed. They are listed below.

## GATEWAY-HTTP — Core 36/36, Extended 10/10

```
GATEWAY-HTTP  core:     Passed: 36  Failed: 0   (result: success)
GATEWAY-HTTP  extended: Passed: 10  Failed: 0   (result: success)
summary: Core tests succeeded. Extended tests succeeded.
--- PASS: TestAetherGatewayHTTP (92.49s)
```

Of the suite's 153 top-level tests, **46 passed, 0 failed and 107 were skipped** (they
belong to other profiles or need a feature that is not advertised).

The Core count moved from 33 (rev21) to 36 because the suite changed between v1.5.1 and
v1.6.3, not because of a different skip list: the runner has none. Its feature set is
**inferred** from `GatewayClass.status.supportedFeatures`, which read, on the day:

`Gateway`, `GatewayPort8080`, `HTTPRoute`, `HTTPRouteHostRewrite`,
`HTTPRouteMethodMatching`, `HTTPRoutePathRedirect`, `HTTPRoutePathRewrite`,
`HTTPRoutePortRedirect`, `HTTPRouteRequestTimeout`,
`HTTPRouteResponseHeaderModification`, `HTTPRouteSchemeRedirect`, `ReferenceGrant`.

That is the same twelve as rev21.

### Extended features not advertised (skipped)

The report lists 29 `unsupportedFeatures`. Their tests are skipped because the edge
GatewayClass does not advertise them:

`BackendTLSPolicy`, `BackendTLSPolicySANValidation`, `GatewayAddressEmpty`,
`GatewayBackendClientCertificate`, `GatewayFrontendClientCertificateValidation`,
`GatewayFrontendClientCertificateValidationInsecureFallback`,
`GatewayHTTPListenerIsolation`, `GatewayHTTPSListenerDetectMisdirectedRequests`,
`GatewayInfrastructure`, `GatewayStaticAddresses`, `HTTPRoute303RedirectStatusCode`,
`HTTPRoute307RedirectStatusCode`, `HTTPRoute308RedirectStatusCode`,
`HTTPRouteBackendProtocolH2C`, `HTTPRouteBackendProtocolWebSocket`,
`HTTPRouteBackendRequestHeaderModification`, `HTTPRouteBackendTimeout`, `HTTPRouteCORS`,
`HTTPRouteDestinationPortMatching`, `HTTPRouteNamedRouteRule`, `HTTPRouteParentRefPort`,
`HTTPRouteQueryParamMatching`, `HTTPRouteRequestMirror`,
`HTTPRouteRequestMultipleMirrors`, `HTTPRouteRequestPercentageMirror`, `HTTPRouteRetry`,
`HTTPRouteRetryBackendTimeout`, `HTTPRouteRetryConnectionError`, `ListenerSet`.

`docs/conformance/gateway-api-features.md` says, feature by feature, which of these are
not implemented and which are implemented but not yet claimed.

### Why `GatewayInfrastructure` is not advertised

Since v1.6.3 the suite gates two tests on this one feature:
`GatewayInfrastructureMetadata` and `GatewayInvalidParametersRef`. The second was a
Core test through v1.6.2 and passed there, which is why CI's Core count went from 37 to
36 with the suite bump; the rejection it checks (`Accepted=False` /
`InvalidParameters`) is unchanged and covered by the edge reconciler's unit tests.

`GatewayInfrastructureMetadata` looks for a **generated ServiceAccount, Pod or Service
that carries the `gateway.networking.k8s.io/gateway-name` label in the Gateway's own
namespace**, and expects the Gateway's `spec.infrastructure` labels and annotations on
it. Aether does not generate per-Gateway workloads there: one shared edge Deployment
serves every Gateway of the class, and what it generates per Gateway is one
LoadBalancer Service **in the edge's namespace**, not in the Gateway's. The suite
therefore has no object to find. The decision of 2026-10-11 is to stay as is: the
feature remains unadvertised and both tests remain skipped.

## MESH-HTTP — Core 7/7

```
MESH-HTTP core: Passed: 7  Failed: 0  (result: success)
summary: Core tests succeeded.
--- PASS: TestAetherMeshHTTP (41.80s)
```

Of the 153 top-level tests, **8 passed, 0 failed and 145 were skipped**: `MeshBasic`,
`MeshFrontend`, `MeshHTTPRouteMatching`, `MeshHTTPRouteRedirectHostAndStatus`,
`MeshHTTPRouteRequestHeaderModifier`, `MeshHTTPRouteSimpleSameNamespace`,
`MeshHTTPRouteWeight` and `MeshTrafficSplit`. The suite counts seven of them in the
profile's Core result.

The three tests rev21 recorded as failing (`MeshHTTPRouteRedirectHostAndStatus`,
`MeshHTTPRouteRequestHeaderModifier`, `MeshHTTPRouteWeight`) pass. The cause recorded
then, the GAMMA transparent-capture path (proposal 022), has shipped since; so has the
runner's manifest overlay, which the one-off runner lacked.

Features advertised to the suite for this profile, the same list CI uses: `Mesh`,
`HTTPRoute`, `HTTPRouteResponseHeaderModification`, `HTTPRouteMethodMatching`.

## The reference cluster against kind

The numbers are the same; the environments are not. What differs, and what it did to
the run:

| | CI (kind) | Reference cluster |
|---|---|---|
| Kubernetes | the pinned kind node image | v1.36.1 |
| Gateway API CRDs | v1.6.3, experimental channel | v1.6.3, standard channel |
| Workload identity | SPIRE off: cleartext mesh hop | SPIRE on: every mesh hop is mTLS |
| Gateway addresses | cloud-provider-kind | the cluster's own LoadBalancer implementation, one address per Gateway |
| Other traffic | none | the long-running test workloads and the synthetic prober |
| Wall time, GATEWAY-HTTP | 29 s (last nightly) | 92.49 s |
| Wall time, MESH-HTTP | 11 s (last nightly) | 41.80 s |

- **No test passed on one and failed on the other.** The lists of passing tests were
  compared name by name with the last nightly CI run on kind, which was still at suite
  v1.6.2. MESH-HTTP: the same eight. GATEWAY-HTTP: the same, less
  `GatewayInvalidParametersRef`, which v1.6.3 skips (see above). The kind counts at
  v1.6.3 in the table are the ones measured when the suite pin was bumped.
- **No timeout was raised and no test was skipped by hand.** The runner's timeouts are
  the committed ones (`GatewayMustHaveAddress=180s`, `MaxTimeToConsistency=60s`, 300 s
  for the mesh profile, `RequestTimeout=10s`).
- **No flake.** Both profiles were run a second time, back to back, with the same
  result: 36/0, 10/0 and 7/0 (in 75.73 s and 32.17 s).
- **The run takes about three times as long.** In the GATEWAY-HTTP run about 30 s went
  to the suite's own setup (its backend pods becoming Ready) before the first test.
  The rest was not broken down further.
- **The standard channel is enough.** Every manifest the two profiles apply was
  accepted by the standard-channel CRDs.
- **The report cannot name the CRD version.** `gatewayAPIVersion` and
  `gatewayAPIChannel` read `UNDEFINED` in both reports. The suite names a version only
  when every Gateway API CRD in the cluster carries the same bundle version and
  channel, and the reference cluster also holds three experimental
  `gateway.networking.x-k8s.io` CRDs from an older bundle. The runner tolerates the
  mismatch (`AllowCRDsMismatch`); the `gateway.networking.k8s.io` CRDs are all v1.6.3.
- **The report's `implementation.version` is a constant in the runner** (`0.53.0`),
  not what ran. What ran is chart 2.5.8 at `e950d3c`.

## What the run left behind

Nothing. After the last run every `gateway-conformance-*` namespace was gone, the
per-Gateway LoadBalancer Services the edge had generated were gone, and the list of
GatewayClasses, Gateways and routes in the cluster was the one recorded before the
first run, object for object.

The suite did not disturb the mesh. No aether pod and no test workload restarted or
was replaced. The synthetic prober's failure counters were read before the first run
and after the last and did not move, in any tier.

## Headline verdict

- **GATEWAY-HTTP: Core 36/36 and Extended 10/10, zero failures**, on a real cluster at
  suite v1.6.3. The same counts as on kind.
- **MESH-HTTP: Core 7/7, zero failures**, with SPIRE on, so over mTLS. This is the first
  clean real-cluster run of the mesh profile; rev21 had it at 4/3.
- **`GatewayInfrastructure` stays unadvertised**, by decision, for the reason above.
- **Nothing was tuned to get here**: no raised timeout, no skip list, no change to the
  deployed release.

## Reproduction

The committed runner, `test/conformance/conformance_test.go` with its
`mesh/manifests.yaml` overlay, at the commit the cluster runs, copied into a
`gateway-api` v1.6.3 source tree and run from its `conformance` directory with Go
1.27.1, as `test/conformance/README.md` describes:

```bash
AETHER_CONFORMANCE=1 go test . -tags conformance -run TestAetherGatewayHTTP -v -timeout 60m
AETHER_CONFORMANCE=1 go test . -tags conformance -run TestAetherMeshHTTP    -v -timeout 60m
```

- `GatewayClassName: "aether"`, controller `gateway.aether.io/edge`,
  `AllowCRDsMismatch: true`, `CleanupBaseResources: true`,
  `CleanupTestResources: true`.
- **GATEWAY-HTTP:** features inferred from `GatewayClass.status.supportedFeatures`.
- **MESH-HTTP:** features passed explicitly (above); the mesh namespace carries
  `aether.io/managed: "true"` and the two echo versions have their own ServiceAccounts.
- The profiles ran one after the other, the second once the first one's namespaces had
  finished terminating.

The suite's own reports for the run of record are committed next to this document:
[`reports/2026-10-11-rev22/gateway-http-report.yaml`](reports/2026-10-11-rev22/gateway-http-report.yaml)
and
[`reports/2026-10-11-rev22/mesh-http-report.yaml`](reports/2026-10-11-rev22/mesh-http-report.yaml).
