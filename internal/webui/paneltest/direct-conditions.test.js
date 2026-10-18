import { test } from 'node:test';
import assert from 'node:assert/strict';
import { directConditionMatches } from '../panel/js/lib/direct-conditions.js';

test('all verified HTTP carriers show the TLS fields for the correct side', () => {
  for (const carrier of ['masque', 'masque-h2', 'connect-ip']) {
    assert.equal(directConditionMatches('server-masque', 'server', carrier), true);
    assert.equal(directConditionMatches('client-masque', 'client', carrier), true);
    assert.equal(directConditionMatches('server-masque', 'client', carrier), false);
    assert.equal(directConditionMatches('client-masque', 'server', carrier), false);
  }
  for (const carrier of ['udp', 'pck', 'spoof', 'sni']) {
    assert.equal(directConditionMatches('server-masque', 'server', carrier), false);
  }
});

test('carrier-specific conditions retain their original meaning', () => {
  assert.equal(directConditionMatches('client-spoof', 'client', 'spoof'), true);
  assert.equal(directConditionMatches('client-spoof', 'server', 'spoof'), false);
  assert.equal(directConditionMatches('sni', 'server', 'sni'), true);
  assert.equal(directConditionMatches('sni', 'server', 'pck'), false);
});
