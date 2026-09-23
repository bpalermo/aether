import http from 'k6/http';
import exec from 'k6/execution';
import { check } from 'k6';
import { Counter } from 'k6/metrics';

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

const CLASSES = ['dns', 'conn', 'tls', 'timeout', 'proto', 'http_4xx', 'http_5xx', 'other'];

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
const classCounters = {
  dns: new Counter('aether_fail_dns'),
  conn: new Counter('aether_fail_conn'),
  tls: new Counter('aether_fail_tls'),
  timeout: new Counter('aether_fail_timeout'),
  proto: new Counter('aether_fail_proto'),
  http_4xx: new Counter('aether_fail_http_4xx'),
  http_5xx: new Counter('aether_fail_http_5xx'),
  other: new Counter('aether_fail_other'),
};

// One shared counter carrying the disambiguating tags (#887). The eight class
// counters above answer "what kind of failure"; this one answers "which target"
// and "which exact code/status", which is what the rev231 and rev234 residuals
// needed and could not produce.
const cAll = new Counter('aether_fail');

// --- How the tag breakdown is made visible (#887) -------------------------
//
// VERIFIED against k6 v2.3.0, not assumed. Two facts drive everything below:
//
//   1. `data.metrics` in handleSummary does NOT contain per-tag submetrics.
//      A Counter incremented with {endpoint,code,status} tags shows up as
//      exactly one entry, aggregated across every tag value. The tags exist
//      during the run and are discarded with it unless an --out sink collects
//      them, and we deliberately run without one.
//   2. A submetric DOES appear in `data.metrics` -- including a multi-tag one
//      such as `aether_fail{code:0,status:302}`, and including with count 0
//      when it never fired -- if, and only if, it is declared up front in
//      options.thresholds. That is k6's only declaration mechanism: metrics
//      themselves cannot be created outside the init context (`new Counter()`
//      in a VU throws "metrics must be declared in the init context"), so
//      naming a counter per observed combination is not possible either.
//
// So the key space has to be enumerated at init, which means it has to be
// bounded. It is NOT free: measured on k6 v2.3.0 at 60 rps against a healthy
// target, ~1,950 declared submetrics cost +0.1-0.4s CPU per 20s (~0.5-2% of a
// core, continuously, for 8h30m on every worker) and +28-33MiB RSS. Both of
// those land on exactly the wrong thing: the CPU is the same order as the
// fleet-core deltas the profiling passes exist to detect (#673 was closed on
// -0.070 fleet cores), and the memory eats into a container that this repo has
// already OOM-killed mid-soak once (see the resources comment in
// k6-runner.yaml). An instrument must not cost as much as the effects it is
// meant to measure.
//
// Hence the split below: a SMALL declared key space -- 281 submetrics, measured
// at +5MiB RSS and +0.9s CPU over a 180s 60rps run against a healthy target,
// most of it one-off startup (the same delta over 60s was +0.65s), so the
// steady-state share is well under 1% of a core -- plus a bounded verbatim
// sample logged as failures happen for the full tuple including the error
// string and a timestamp. The sample is free in the healthy case -- the healthy
// case is zero failures -- and its timestamps are what correlate a burst
// against a proxy roll (#823).
//
// The two halves are complementary and each covers the other's blind spot: the
// sample is exact but truncates under a burst, the submetrics are exact under
// a burst but only over declared combinations. The summary reconciles them, so
// a combination this file failed to anticipate is REPORTED rather than lost --
// see `attributed=` in handleSummary.

// Every status code in the IANA HTTP Status Code Registry. Enumerated rather
// than curated down to "the ones we expect": a status we did not think of is
// precisely the one worth seeing, and 63 entries cost nothing.
const HTTP_STATUSES = [
  100, 101, 102, 103,
  200, 201, 202, 203, 204, 205, 206, 207, 208, 226,
  300, 301, 302, 303, 304, 305, 306, 307, 308,
  400, 401, 402, 403, 404, 405, 406, 407, 408, 409, 410, 411, 412, 413, 414,
  415, 416, 417, 418, 421, 422, 423, 424, 425, 426, 428, 429, 431, 451,
  500, 501, 502, 503, 504, 505, 506, 507, 508, 510, 511,
];

// k6's documented error_code values for failures that never produced a
// response (status 0): general, DNS, TCP, TLS, and the HTTP/2 block, which k6
// spreads over 1400-1439 as 1400 + a protocol-level sub-code.
function transportCodes() {
  const codes = [1000, 1010, 1020, 1050, 1100, 1101, 1110, 1111];
  for (const base of [1200, 1300]) {
    for (let i = 0; i < 30; i++) codes.push(base + i);
  }
  for (let c = 1400; c <= 1439; c++) codes.push(c);
  return codes;
}

// reasonTag is the single joint key for "why did this request fail". k6 sets
// error_code = 1000 + status for an HTTP error response and leaves status 0
// for a transport failure, so the pair is never ambiguous.
function reasonTag(code, status) {
  return `code=${code} status=${status}`;
}

// declaredThresholds enumerates the submetrics the summary is allowed to
// report. `count>=0` is a declaration, not a gate -- it can never fail, and it
// is not pretending to: the gate in this file is `http_req_failed`, below.
function declaredThresholds() {
  const th = {};
  for (const s of HTTP_STATUSES) {
    // A response arrived. Either k6 left error_code at 0, or it derived one
    // from the status.
    if (s !== 200) th[`aether_fail{reason:${reasonTag(0, s)}}`] = ['count>=0'];
    th[`aether_fail{reason:${reasonTag(1000 + s, s)}}`] = ['count>=0'];
  }
  for (const c of transportCodes()) {
    th[`aether_fail{reason:${reasonTag(c, 0)}}`] = ['count>=0'];
  }
  for (const k of CLASSES) {
    for (const t of targets) {
      th[`aether_fail{class:${k},endpoint:${t}}`] = ['count>=0'];
    }
  }
  return th;
}

export const options = {
  scenarios: {
    mesh_soak: {
      executor: 'constant-arrival-rate',
      rate: 60, timeUnit: '1s',
      duration: '8h30m',
      preAllocatedVUs: 60, maxVUs: 180,
    },
  },
  thresholds: Object.assign(declaredThresholds(), {
    http_req_failed: ['rate<0.01'],
    checks: ['rate>0.99'],
  }),
};

// classify returns the failure class a response belongs to, or null if the
// response is a success.
//
// STATUS FIRST, and that ordering is the whole point (#887). Until it was
// fixed, the status checks sat inside an `error_code === 0` branch -- and k6
// sets a NON-ZERO error_code for an HTTP error response: verified on k6 v2.3.0,
// a 404 arrives as error_code 1404, a 503 as 1503, a 504 as 1504, i.e.
// 1000 + status. The status checks were therefore unreachable for every
// response they were written for: `http_4xx` and `http_5xx` could never be
// incremented at all, 4xx fell into the 1400-1499 range test and was filed as
// `proto`, and 5xx fell off the end into `other`. That is where the rev234
// soak's 33 unattributable failures came from -- VictoriaLogs reconciled all
// 33 against the source proxies' access logs as genuine 504/UT responses, 31 on
// main-worker-01 and 2 on main-worker-03, matching the two runners exactly.
//
// A response that carries an HTTP status was RECEIVED and completed; whatever
// error_code k6 derived from it is a restatement of the status, not extra
// information. Classifying by status first also resolves a genuine ambiguity in
// k6's numbering, where 1400-1499 means both "HTTP/2 protocol error" and
// "1000 + a 4xx status": a real HTTP/2 error leaves status at 0, so after this
// ordering only the protocol error can reach the `proto` test.
//
// The transport half stays deliberately conservative about k6's numbering: it
// keys on the documented *hundreds* ranges rather than individual codes,
// because a wrong constant would silently file failures under the wrong defect,
// which is worse than filing them under `other`. Two deviations, both
// intentional:
//
//   * timeouts are matched on the error STRING first, because a timeout can
//     surface under more than one numeric range -- k6 v2.3.0 reports a request
//     timeout as 1050, which no range below would catch;
//   * 1211 is dial-time DNS resolution and belongs with DNS, not with TCP.
//
// Anything unrecognised still lands in `other` rather than being forced into a
// neighbouring class: a 1xx, a 3xx, a non-200 2xx, or a transport code outside
// every documented range. A non-zero `other` remains a prompt to extend this
// function, not a failure to explain -- and the breakdown now says what to
// extend it with.
function classify(res) {
  const ec = res.error_code || 0;
  const err = String(res.error || '');
  const status = res.status || 0;

  // The only success: a clean 200 with nothing attached to it. A 200 that
  // still carries an error (a body that failed to read, say) must NOT return
  // null, or it would count as failed in http_req_failed and vanish from the
  // reconciliation below.
  if (status === 200 && ec === 0 && err === '') return null;

  if (status >= 500) return 'http_5xx';
  if (status >= 400) return 'http_4xx';

  // Past here there is no usable response: status is 0, or it is a 1xx/3xx/2xx
  // that k6 nonetheless flagged.
  if (/timeout|deadline exceeded/i.test(err)) return 'timeout';

  if (ec === 1211) return 'dns';
  if (ec >= 1100 && ec < 1200) return 'dns';
  if (ec >= 1200 && ec < 1300) return 'conn';
  if (ec >= 1300 && ec < 1400) return 'tls';
  if (ec >= 1400 && ec < 1500) return 'proto';
  // 1000 + status for a 5xx, seen without a populated status field. Unreachable
  // while k6 sets both, which it does today -- kept so the range cannot fall
  // silently into `other` if that ever changes.
  if (ec >= 1500 && ec < 1600) return 'http_5xx';
  return 'other';
}

// Bounded verbatim sample of failures, logged as they happen (#887).
//
// Per-VU state is the right tool here precisely because this is NOT an
// aggregate: each line is one event, printed immediately, so k6's per-VU
// isolation costs nothing. The cap is per VU per class, so the worst case is
// SAMPLE_CAP x 8 x maxVUs lines for the whole run; the healthy case is zero,
// because the healthy case has no failures.
const SAMPLE_CAP = 10;
const sampled = {};

function sampleFailure(cls, url, code, status, err) {
  const n = (sampled[cls] || 0) + 1;
  sampled[cls] = n;
  if (n > SAMPLE_CAP) return;
  // One line, one JSON object, greppable as AETHER_FAIL. The timestamp is the
  // load-bearing field: it is what lines a burst up against a proxy roll.
  console.log(
    'AETHER_FAIL ' +
      JSON.stringify({
        t: new Date().toISOString(),
        class: cls,
        endpoint: url,
        code: code,
        status: status,
        error: String(err || ''),
        vu: exec.vu.idInTest,
        n: n,
        truncated: n === SAMPLE_CAP,
      }),
  );
}

export default function () {
  const url = targets[Math.floor(Math.random() * targets.length)];
  const res = http.get(url, { tags: { endpoint: url } });

  const cls = classify(res);
  if (cls !== null) {
    const code = res.error_code || 0;
    const status = res.status || 0;
    // The exact code/status rides along as a tag on the class counter so a raw
    // output sink, if one is ever attached, can resolve a class down to a
    // specific error.
    classCounters[cls].add(1, { endpoint: url, code: String(code), status: String(status) });
    // ... and on the shared counter, whose declared submetrics are what makes
    // the same split visible in the end-of-run summary with no sink at all.
    cAll.add(1, { class: cls, endpoint: url, reason: reasonTag(code, status) });
    sampleFailure(cls, url, code, status, res.error);
  }

  check(res, { 'status is 200': (r) => r.status === 200 });
}

function submetricCount(data, name) {
  const m = data.metrics[name];
  return (m && m.values && m.values.count) || 0;
}

// handleSummary prints the class split in a form that survives the run.
//
// Written by hand rather than via the k6-summary jslib: that is a remote
// import from jslib.k6.io, and the soak runner has no reason to have egress to
// it. A summary that fails to render is a soak with no data.
export function handleSummary(data) {
  const counts = {};
  let total = 0;
  for (const label of CLASSES) {
    // A class that never fired has NO metric object at all -- k6 omits it, the
    // same way Envoy omits a never-incremented counter. Report it as an
    // explicit 0 so a reader can tell "did not happen" from "not measured".
    const n = submetricCount(data, 'aether_fail_' + label);
    counts[label] = n;
    total += n;
  }

  // Which target. Exact: the endpoint list is known at init, so every
  // combination is declared and none can be missed.
  const byEndpoint = {};
  for (const label of CLASSES) {
    for (const t of targets) {
      const n = submetricCount(data, `aether_fail{class:${label},endpoint:${t}}`);
      if (n > 0) byEndpoint[`${label} ${t}`] = n;
    }
  }

  // Which exact error. Bounded: only declared combinations can appear, which
  // is why the total is reconciled against the class totals below.
  const byReason = {};
  let attributed = 0;
  for (const s of HTTP_STATUSES) {
    for (const c of s === 200 ? [1000 + s] : [0, 1000 + s]) {
      const r = reasonTag(c, s);
      const n = submetricCount(data, `aether_fail{reason:${r}}`);
      if (n > 0) {
        byReason[r] = n;
        attributed += n;
      }
    }
  }
  for (const c of transportCodes()) {
    const r = reasonTag(c, 0);
    const n = submetricCount(data, `aether_fail{reason:${r}}`);
    if (n > 0) {
      byReason[r] = n;
      attributed += n;
    }
  }

  const reqs = (data.metrics.http_reqs && data.metrics.http_reqs.values.count) || 0;
  const failRate = (data.metrics.http_req_failed && data.metrics.http_req_failed.values.rate) || 0;

  const table = (rows) =>
    Object.keys(rows)
      .sort((a, b) => rows[b] - rows[a])
      .map((k) => `  ${String(rows[k]).padStart(8)}  ${k}`);

  const lines = [
    '',
    '=== aether soak failure classes (#846) ===',
    `http_reqs=${reqs} http_req_failed_rate=${failRate}`,
    ...CLASSES.map((label) => `  ${label.padEnd(9)} ${counts[label]}`),
    `  ${'TOTAL'.padEnd(9)} ${total}`,
    '',
    // The reconciliation this exists to serve: classified failures should
    // account for every failed request. A gap means a failure mode this
    // function does not recognise, which is itself the finding.
    `classified=${total} of http_req_failed≈${Math.round(failRate * reqs)}`,
    '',
    '--- by target (#887) ---',
    ...(total === 0 ? ['  (none)'] : table(byEndpoint)),
    '',
    '--- by reason: k6 error_code / HTTP status (#887) ---',
    ...(total === 0 ? ['  (none)'] : table(byReason)),
    '',
    // Second reconciliation, one level down. The reason breakdown can only
    // report combinations declared at init, so it is capable of under-counting
    // in a way the class totals are not. Saying so is the difference between a
    // breakdown and a breakdown you can trust: attributed<classified means a
    // code/status pair this file did not anticipate, and the AETHER_FAIL sample
    // lines above name it verbatim.
    `attributed=${attributed} of classified=${total}`,
    ...(attributed < total
      ? ['  GAP: an undeclared code/status pair reached a class -- grep AETHER_FAIL to name it']
      : []),
    '',
  ];

  // stdout ONLY, deliberately. Writing a summary file would add a filesystem
  // failure mode (the runner drops privileges to uid 12345) to the very last
  // step of an 8h run, and this repo has already lost soak summaries more than
  // once. `kubectl logs` is what actually collects this. The single-line
  // AETHER_METRIC marker matches the convention in e2e/multicluster_replicator.sh
  // so both harnesses can be scraped the same way -- it is EXTENDED here with
  // the breakdown rather than replaced, because the existing keys are scraped.
  lines.push(
    'AETHER_METRIC k6_failure_classes=' +
      JSON.stringify({ counts, total, reqs, failRate, byEndpoint, byReason, attributed }),
    '',
  );

  return { stdout: lines.join('\n') };
}
