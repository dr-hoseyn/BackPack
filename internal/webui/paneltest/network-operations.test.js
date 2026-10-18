import { test } from 'node:test';
import assert from 'node:assert/strict';

// A small event harness for the real view and API modules. It models selectors
// used by the operation handlers; layout and browser rendering are separate QA.
class Node {
  constructor() { this.nodes = new Map(); this.disabled = false; this.innerHTML = ''; this.textContent = ''; }
  querySelector(selector) {
    if (!this.nodes.has(selector)) this.nodes.set(selector, new Node());
    return this.nodes.get(selector);
  }
  querySelectorAll() { return [...this.nodes.values()]; }
  reset() {}
}
const view = new Node();
globalThis.document = { documentElement: { dataset: {} }, getElementById: () => view };
globalThis.FormData = class extends Map { constructor(form) { super(form.fields || []); } };
const { networkView } = await import('../panel/js/views/network.js');
const tick = () => new Promise(resolve => setImmediate(resolve));

function mount(kind) {
  view.nodes.clear();
  const calls = [];
  globalThis.fetch = (url, options = {}) => {
    if (url.endsWith('/history')) return Promise.resolve(new Response('{}'));
    return new Promise(resolve => calls.push({
      body: JSON.parse(options.body),
      finish: (data = {}, status = 200) => resolve(new Response(JSON.stringify(data), {status})),
    }));
  };
  let leave;
  networkView({setTeardown(fn) { leave = fn; }});
  const form = view.querySelector(kind === 'auto' ? '#autoForm' : '#peerForm');
  const box = view.querySelector(kind === 'auto' ? '#autoResult' : '#peerResult');
  form.fields = [['name', 'test-tunnel'], [kind === 'auto' ? 'ports' : 'link', 'test-value']];
  return {form, box, calls, leave: () => leave(), submit: () => form.onsubmit({preventDefault() {}, currentTarget: form}),
    preview: id => ({id, preview: {name:'test-tunnel', host:'192.0.2.1', profile:'economy', expires:'2030-01-01T00:00:00Z'}}),
    create: () => box.querySelector(kind === 'auto' ? '#createAutomatic' : 'button')};
}

test('custom carrier setup sends the DERP region as an integer and avoids suggesting an inbound Tailcat port', async () => {
  const h = mount('auto');
  h.form.fields.push(['carriers','trusttunnel,tailcat'],['derpMapURL','https://relay.example/map.json'],['derpRegion','901']);
  const pending = h.submit(); await tick();
  assert.equal(h.calls[0].body.derpRegion, 901);
  assert.equal(h.calls[0].body.carriers, 'trusttunnel,tailcat');
  const reply = h.preview('custom');
  reply.preview.listeners = [{protocol:'tailcat',network:'DERP',port:''}];
  h.calls[0].finish(reply); await pending;
  assert.match(h.box.innerHTML, /outbound DERP; no public listener/);
  h.leave(); await tick(); h.calls[1].finish(); await tick();
});

for (const kind of ['auto', 'peer']) {
  test(`${kind}: repeated submit and clicks cannot replace an in-flight commit`, async () => {
    const h = mount(kind);
    const preview = h.submit(); await tick();
    await h.submit();
    assert.equal(h.calls.length, 1);
    h.calls[0].finish(h.preview('reviewed-id')); await preview;
    const before = h.box.innerHTML;
    const commit = h.create().onclick(); await tick();
    await h.create().onclick(); await h.submit();
    assert.equal(h.calls.length, 2);
    assert.deepEqual(h.calls[1].body, {action:'commit', id:'reviewed-id'});
    assert.equal(h.form.querySelector('button').disabled, true);
    h.leave(); await tick();
    assert.equal(h.calls.length, 2, 'navigation tried to cancel an already committing operation');
    h.calls[1].finish({name:'test-tunnel',active:true,link:'private-link'}); await commit;
    assert.equal(h.box.innerHTML, before, 'late commit changed a departed view');
  });

  test(`${kind}: late preview after navigation releases only its own reservation`, async () => {
    const h = mount(kind);
    const preview = h.submit(); await tick(); h.leave();
    h.calls[0].finish(h.preview('late-id')); await preview; await tick();
    assert.equal(h.calls.length, 2);
    assert.deepEqual(h.calls[1].body, {action:'cancel',id:'late-id'});
    h.calls[1].finish(); await tick();
  });

  test(`${kind}: replacement releases the old preview before requesting another`, async () => {
    const h = mount(kind);
    const first = h.submit(); await tick();
    h.calls[0].finish(h.preview('old-id')); await first;
    const oldCreate = h.create().onclick;
    const next = h.submit(); await tick();
    assert.equal(h.calls.length, 2);
    assert.deepEqual(h.calls[1].body, {action:'cancel',id:'old-id'});
    h.calls[1].finish({error:'expired'}, 409); await tick();
    assert.equal(h.calls.length, 3);
    assert.equal(h.calls[2].body.action, 'preview');
    h.calls[2].finish(h.preview('new-id')); await next;
    await oldCreate();
    assert.equal(h.calls.length, 3, 'stale review committed the new preview');
    h.leave(); await tick();
    assert.deepEqual(h.calls[3].body, {action:'cancel',id:'new-id'});
    h.calls[3].finish(); await tick();
  });
}
