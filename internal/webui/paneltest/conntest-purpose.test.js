import { test } from 'node:test';
import assert from 'node:assert/strict';
import { purposeRecommendations } from '../panel/js/lib/conntest-purpose.js';

test('purpose verdicts retain their own path, SNI and measured evidence', () => {
  const html = purposeRecommendations([
    { purpose: 'download', transport: 'reverse xhttp', host: '192.0.2.1', family: 'IPv4', speed_measured: true, speed_stable: false, download_mbps: 200, upload_mbps: 20, samples: 60, rtt_ms: 70, jitter_ms: 5, loss_pct: 0 },
    { purpose: 'realtime', transport: 'direct sni', host: '2001:db8::1', family: 'IPv6', sni: '<img onerror="boom">', samples: 60, rtt_ms: 30, jitter_ms: 1, loss_pct: 0 },
    { purpose: 'stability', transport: 'reverse quic', host: '192.0.2.2', family: 'IPv4', samples: 60, rtt_ms: 80, jitter_ms: 0.5, loss_pct: 0 },
  ], true);
  for (const text of ['Download', 'Games / calls', 'Stability', 'IPv4', 'IPv6', '2001:db8::1', '192.0.2.2', '200.0 ↓ / 20.0 ↑', '60 echoes', 'Speed varied', 'Speed not measured', 'long-term uptime']) {
    assert.ok(html.includes(text), text);
  }
  assert.ok(html.includes('&lt;img'));
  assert.ok(!html.includes('<img'));
});

test('older verdicts without purposes still render, without invented choices', () => {
  assert.equal(purposeRecommendations(undefined), '');
  assert.equal(purposeRecommendations([]), '');
  assert.equal(purposeRecommendations([{ purpose: 'other', transport: 'reverse tcp' }]), '');
});
