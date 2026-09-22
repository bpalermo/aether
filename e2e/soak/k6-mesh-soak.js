import http from 'k6/http';
import { check } from 'k6';
import { Counter } from 'k6/metrics';

export const options = {
  scenarios: {
    mesh_soak: {
      executor: 'constant-arrival-rate',
      rate: 60, timeUnit: '1s',
      duration: '8h30m',
      preAllocatedVUs: 60, maxVUs: 180,
    },
  },
  thresholds: {
    http_req_failed: ['rate<0.01'],
    checks: ['rate>0.99'],
  },
};

const targets = [
  'http://svc-1.aether-test.aether.internal:18081/',
  'http://svc-2.aether-test.aether.internal:18081/',
  'http://svc-3.aether-test.aether.internal:18081/',
  'http://svc-4.aether-test.aether.internal:18081/',
  'http://echo.aether-test.aether.internal:18081/',
  // A MULTI-PROTOCOL service (proposal 037): HTTP primary on :8080, raw TCP on
  // :9000, one pod, one ServiceAccount. It is here to hold the HTTP half of that
  // shape under load while e2e/soak/multiprotocol.yaml's dialer drives the TCP
  // half, because the interesting failure is not either protocol on its own --
  // it is the destination_port chain for :9000 shadowing the VIP's HTTP traffic.
  // Envoy evaluates destination_port BEFORE prefix_ranges, server_names and
  // application_protocols, so a mistake there does not degrade the TCP port; it
  // silently swallows every HTTP request to this authority. This target is what
  // would notice.
  //
  // Adding a sixth target leaves the TOTAL arrival rate untouched -- the
  // executor is constant-arrival-rate -- so fleet CPU stays comparable with the
  // rev231 run. Only the per-target share moves, 20% to 16.7%.
  'http://mixed-svc.aether-test.aether.internal:18081/',
];

// Per-failure-class counters (#846).
//
// k6's text summary aggregates across tags, so `error_code` is invisible in it
// and every transport failure looks identical. That is why the rev226 soak
// ended with ~510 k6 failures that had no access-log line and no way to tell a
// refused dial from a DNS failure from a 4xx. Each class below is a DIFFERENT
// defect with a different owner, so they have to be counted separately.
//
// These MUST be Counters, not a plain object tally. Each VU runs its own JS
// context, so module-level `{}` state is per-VU and never aggregated, and
// handleSummary() runs in yet another context that would see none of it. k6
// aggregates metrics by NAME across VUs; ordinary variables it does not.
const cDNS = new Counter('aether_fail_dns');
const cConn = new Counter('aether_fail_conn');
const cTLS = new Counter('aether_fail_tls');
const cTimeout = new Counter('aether_fail_timeout');
const cProto = new Counter('aether_fail_proto');
const cOther = new Counter('aether_fail_other');
const c4xx = new Counter('aether_fail_http_4xx');
const c5xx = new Counter('aether_fail_http_5xx');

// classify returns the counter a failed response belongs to, or null if the
// response is a success.
//
// Deliberately conservative about k6's error_code numbering: it keys on the
// documented *hundreds* ranges rather than on individual codes, because a
// wrong constant here would silently file failures under the wrong defect --
// which is worse than filing them under `other`. Two deviations, both
// intentional:
//
//   * timeouts are matched on the error STRING first, because a timeout can
//     surface under more than one numeric range;
//   * 1211 is dial-time DNS resolution and belongs with DNS, not with TCP.
//
// Anything unrecognised lands in `other` rather than being forced into a
// neighbouring class. A non-zero `other` is a prompt to extend this function,
// not a failure to explain.
function classify(res) {
  const ec = res.error_code || 0;

  if (ec === 0) {
    if (res.status >= 500) return c5xx;
    if (res.status >= 400) return c4xx;
    if (res.status === 200) return null;
    return cOther;
  }

  const err = String(res.error || '');
  if (/timeout|deadline exceeded/i.test(err)) return cTimeout;

  if (ec === 1211) return cDNS;
  if (ec >= 1100 && ec < 1200) return cDNS;
  if (ec >= 1200 && ec < 1300) return cConn;
  if (ec >= 1300 && ec < 1400) return cTLS;
  if (ec >= 1400 && ec < 1500) return cProto;
  return cOther;
}

export default function () {
  const url = targets[Math.floor(Math.random() * targets.length)];
  const res = http.get(url, { tags: { endpoint: url } });

  const bucket = classify(res);
  if (bucket !== null) {
    // The exact code/status rides along as a tag so the raw output can still
    // resolve a class down to a specific error when one needs chasing.
    bucket.add(1, {
      endpoint: url,
      code: String(res.error_code || 0),
      status: String(res.status),
    });
  }

  check(res, { 'status is 200': (r) => r.status === 200 });
}

// handleSummary prints the class split in a form that survives the run.
//
// Written by hand rather than via the k6-summary jslib: that is a remote
// import from jslib.k6.io, and the soak runner has no reason to have egress to
// it. A summary that fails to render is a soak with no data.
export function handleSummary(data) {
  const classes = [
    ['dns', 'aether_fail_dns'],
    ['conn', 'aether_fail_conn'],
    ['tls', 'aether_fail_tls'],
    ['timeout', 'aether_fail_timeout'],
    ['proto', 'aether_fail_proto'],
    ['http_4xx', 'aether_fail_http_4xx'],
    ['http_5xx', 'aether_fail_http_5xx'],
    ['other', 'aether_fail_other'],
  ];

  const counts = {};
  let total = 0;
  for (const [label, metric] of classes) {
    // A class that never fired has NO metric object at all -- k6 omits it, the
    // same way Envoy omits a never-incremented counter. Report it as an
    // explicit 0 so a reader can tell "did not happen" from "not measured".
    const m = data.metrics[metric];
    const n = (m && m.values && m.values.count) || 0;
    counts[label] = n;
    total += n;
  }

  const reqs = (data.metrics.http_reqs && data.metrics.http_reqs.values.count) || 0;
  const failRate = (data.metrics.http_req_failed && data.metrics.http_req_failed.values.rate) || 0;

  const lines = [
    '',
    '=== aether soak failure classes (#846) ===',
    `http_reqs=${reqs} http_req_failed_rate=${failRate}`,
    ...classes.map(([label]) => `  ${label.padEnd(9)} ${counts[label]}`),
    `  ${'TOTAL'.padEnd(9)} ${total}`,
    '',
    // The reconciliation this exists to serve: classified failures should
    // account for every failed request. A gap means a failure mode this
    // function does not recognise, which is itself the finding.
    `classified=${total} of http_req_failed≈${Math.round(failRate * reqs)}`,
    '',
  ];

  // stdout ONLY, deliberately. Writing a summary file would add a filesystem
  // failure mode (the runner drops privileges to uid 12345) to the very last
  // step of an 8h run, and this repo has already lost soak summaries more than
  // once. `kubectl logs` is what actually collects this. The single-line
  // AETHER_METRIC marker matches the convention in e2e/multicluster_replicator.sh
  // so both harnesses can be scraped the same way.
  lines.push(
    'AETHER_METRIC k6_failure_classes=' + JSON.stringify({ counts, total, reqs, failRate }),
    '',
  );

  return { stdout: lines.join('\n') };
}
