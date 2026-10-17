import { test } from 'node:test';
import assert from 'node:assert/strict';
import { splitMapping, mappingValue } from '../panel/js/lib/portmapping.js';

test('opening an existing port mapping preserves its spelling', () => {
  for (const raw of ['443', '10000-10009', '[::1]:443=127.0.0.1:8443', '443=80|81']) {
    const original = splitMapping(raw);
    assert.equal(mappingValue(original, original.source, original.target), raw);
  }
});

test('source and target fields create an explicit remap', () => {
  assert.equal(mappingValue(splitMapping(''), '443', '8443'), '443=8443');
  assert.equal(mappingValue(splitMapping('443'), '8080', '443'), '8080=443');
  assert.equal(splitMapping('[::1]:443').target, '443');
});
