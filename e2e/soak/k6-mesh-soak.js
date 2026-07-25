import http from 'k6/http';
import { check } from 'k6';

export const options = {
  scenarios: {
    mesh_soak: {
      executor: 'constant-arrival-rate',
      rate: 60, timeUnit: '1s',
      duration: '7h40m',
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
];

export default function () {
  const url = targets[Math.floor(Math.random() * targets.length)];
  const res = http.get(url, { tags: { endpoint: url } });
  check(res, { 'status is 200': (r) => r.status === 200 });
}
