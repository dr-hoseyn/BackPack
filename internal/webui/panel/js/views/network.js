import { esc } from '../lib/dom.js';
import * as api from '../api.js';

const number = v => Number.isFinite(Number(v)) ? Number(v).toFixed(2) : '—';

// Uses the panel's existing forms and metric rows. A public listener manifest is
// the review boundary; credentials appear only after explicit creation.
export function networkView(ctx) {
  const view = document.getElementById('view');
  view.innerHTML = `<div class="tl-page network-tools">
    <div class="sech2 tl-head"><h2>Network tools</h2><span class="sp"></span><a href="#/conntest">Connection test</a></div>
    <p class="tl-sub">Prepare an Automatic tunnel on Iran, inspect a Kharej carrier, or compare saved measurements.</p>
    <section class="tl-card"><h3>Automatic setup · Iran</h3>
      <form id="autoForm">
        <label>Tunnel name<input name="name" maxlength="40" placeholder="Chosen automatically when empty"></label>
        <label>Public IPv4 or hostname<input name="host" maxlength="253" placeholder="Detect IPv4 when empty"></label>
        <label>Forwarded ports<input name="ports" required placeholder="443,8080=127.0.0.1:2096"></label>
        <label>Profile<select name="profile"><option value="economy">Economy · up to three carriers</option><option value="full">Full · up to eight carriers</option></select></label>
        <label>Carriers (optional, comma-separated)<input name="carriers" maxlength="180" placeholder="trusttunnel,webtransport,xhttp-reality"></label>
        <p class="hint">Leave empty for automatic selection. Combine 2–8 carriers; Economy permits up to three. Available types: trusttunnel, webtransport, xhttp-reality, xhttp, reality, hysteria2, anytls, tuic, naive, shadowtls, short-https and experimental tailcat. Helper-based types require their installed binaries; REALITY and ShadowTLS require a verified cover.</p>
        <label>Tailcat HTTPS DERP map (only when selecting tailcat)<input name="derpMapURL" maxlength="2048" placeholder="https://relay.example.com/derpmap.json"></label>
        <label>Tailcat region ID from that map<input name="derpRegion" type="number" min="1" max="65535" placeholder="Required for Tailcat"></label>
        <label>Verified cover endpoint · optional<input name="cover" placeholder="your-cover.example:443"></label>
        <button class="tl-btn" type="submit">Review required ports</button>
      </form><div id="autoResult" role="status" aria-live="polite"></div>
    </section>
    <section class="tl-card"><h3>Pair from a Setup Link · Kharej</h3>
      <form id="peerForm"><label>Private Setup Link<textarea name="link" required rows="3" autocomplete="off" spellcheck="false"></textarea></label>
        <label>Local tunnel name · optional<input name="name" maxlength="40"></label><button class="tl-btn" type="submit">Review peer settings</button></form>
      <div id="peerResult" role="status" aria-live="polite"></div>
    </section>
    <section class="tl-card"><h3>Carrier diagnosis · Kharej</h3>
      <p>Checks the configured TUIC or Hysteria2 path with temporary helpers. Your running tunnel stays active.</p>
      <form id="diagForm"><label>Local tunnel name<input name="name" required maxlength="64"></label>
        <label>Managed path name · optional<input name="path" maxlength="32" placeholder="For example: hysteria2"></label>
        <button class="tl-btn" type="submit">Run carrier diagnosis</button></form>
      <div id="diagResult" role="status" aria-live="polite"></div>
    </section>
    <section class="tl-card"><h3>Saved Connection Tests</h3>
      <p>Completed samples compare the same route and settings. Steady samples are not an uptime percentage.</p>
      <button class="tl-btn" id="loadHistory">Refresh history and comparison</button>
      <div id="testHistory" aria-live="polite"></div>
    </section>
  </div>`;
  let alive = true, plan = null;
  const auto = view.querySelector('#autoResult'), diag = view.querySelector('#diagResult'), history = view.querySelector('#testHistory');
  const peer = view.querySelector('#peerResult');
  let peerPlan = null;
  const pending = new Set();
  async function operate(form, box, work) {
    if (!alive || pending.has(form)) return;
    pending.add(form);
    const button = form.querySelector('button');
    button.disabled = true;
    box.querySelectorAll('button').forEach(b => { b.disabled = true; });
    try { await work(); } catch (e) { error(box, e); }
    finally {
      pending.delete(form);
      if (alive) {
        button.disabled = false;
        box.querySelectorAll('button').forEach(b => { b.disabled = false; });
      }
    }
  }
  async function release(call, id) {
    if (!id) return;
    try { await call({action:'cancel', id}); }
    catch (e) { if (e.status !== 409) throw e; } // Expired or superseded preview.
  }
  const error = (box, e) => { if (alive) box.textContent = e.message || 'The request failed. Try again.'; };
  view.querySelector('#autoForm').onsubmit = async event => {
    event.preventDefault();
    const form = event.currentTarget;
    return operate(form, auto, async () => {
      const previous = plan; plan = null;
      await release(api.automatic, previous);
      if (!alive) return;
      auto.textContent = 'Checking helpers, cover and available ports…';
      const values = Object.fromEntries(new FormData(form));
      values.derpRegion = Number(values.derpRegion || 0);
      const r = await api.automatic({ action: 'preview', ...values });
      if (!alive) { api.automatic({action:'cancel',id:r.id}).catch(() => {}); return; }
      const id = r.id; plan = id;
      const p = r.preview;
      auto.innerHTML = `<h4>${esc(p.name)} · ${esc(p.profile)}</h4><p>${esc(p.host)} · ${esc((p.ports || []).join(', '))}</p>
        <ul>${(p.listeners || []).map(l => `<li>${esc(l.protocol)} · ${l.network === 'DERP' ? 'outbound DERP; no public listener' : `allow inbound ${esc(l.network)} ${esc(l.port)}`}</li>`).join('')}</ul>
        ${(p.warnings || []).map(w => `<p>${esc(w)}</p>`).join('')}
        <p>Review expires at ${esc(new Date(p.expires).toLocaleTimeString())}.</p>
        <button class="tl-btn" id="createAutomatic">Create this tunnel</button> <button class="tl-btn" id="cancelAutomatic">Cancel</button>`;
      auto.querySelector('#cancelAutomatic').onclick = () => operate(form, auto, async () => {
        if (plan !== id) return;
        plan = null;
        await release(api.automatic, id);
        if (alive) auto.textContent = 'Preview cancelled.';
      });
      auto.querySelector('#createAutomatic').onclick = () => operate(form, auto, async () => {
        if (plan !== id) return;
        plan = null; // A committing operation must not be cancelled by navigation.
        const result = await api.automatic({action:'commit', id});
        if (!alive) return;
        auto.innerHTML = `<p>${esc(result.name)}: ${result.active ? 'service started; pair Kharej to verify the tunnel' : 'created; service is not active, inspect its log'}.</p>
          <label>Private Setup Link · paste into Setup Kharej<textarea readonly rows="4" id="automaticLink"></textarea></label>
          <p>This link contains account secrets. Share it only with the other server.</p>`;
        auto.querySelector('textarea').value = result.link;
      });
    });
  };
  view.querySelector('#diagForm').onsubmit = async event => {
    event.preventDefault(); const form = event.currentTarget, button = form.querySelector('button');
    button.disabled=true; diag.textContent='Checking the carrier…';
    try {
      const r = await api.carrierDiagnose(Object.fromEntries(new FormData(form)));
      if (!alive) return;
      diag.innerHTML = (r.reports || []).map(report => `<h4>${esc(report.path)} · ${report.verified ? 'verified' : 'not verified'}</h4><dl>${(report.stages || []).map(s => `<div class="dr2"><dt>${esc(s.stage)} · ${esc(s.status)}</dt><dd>${esc(s.detail)}</dd></div>`).join('')}</dl>`).join('');
    } catch (e) { error(diag,e); } finally { if (alive) button.disabled=false; }
  };
  view.querySelector('#peerForm').onsubmit = event => {
    event.preventDefault(); const form = event.currentTarget;
    return operate(form, peer, async () => {
      const previous = peerPlan; peerPlan = null;
      await release(api.managedLink, previous);
      if (!alive) return;
      const r = await api.managedLink({action:'preview', ...Object.fromEntries(new FormData(form))});
      if (!alive) { api.managedLink({action:'cancel', id:r.id}).catch(() => {}); return; }
      const id = r.id; peerPlan = id;
      peer.innerHTML = `<h4>${esc(r.preview.name)} · ${esc(r.preview.host)}</h4><p>${esc(r.preview.profile || r.preview.transport)}</p>
        <ul>${(r.preview.paths || []).map(p => `<li>${esc(p)}</li>`).join('')}</ul><button class="tl-btn" id="createPeer">Create this peer</button>`;
      peer.querySelector('button').onclick = () => operate(form, peer, async () => {
        if (peerPlan !== id) return;
        peerPlan = null;
        const result = await api.managedLink({action:'commit', id});
        if (alive) {
          peer.textContent = `${result.name}: ${result.active ? 'service started; verify traffic through the tunnel' : 'created; inspect the service log'}.`;
          form.reset();
        }
      });
    });
  };
  async function loadHistory() {
    history.textContent='Loading saved measurements…';
    try {
      const r = await api.connTestHistory(); if (!alive) return;
      history.innerHTML = `${r.warning ? `<p>${esc(r.warning)}</p>` : ''}<p>${(r.runs || []).length} saved runs</p>
        ${(r.comparison || []).map(s => `<article><h4>${esc(s.side)} · ${esc(s.kind)} · ${esc(s.transport)} · ${esc(s.family)} · ${esc(s.preset)}</h4>
          <p>${esc(s.host)} → ${esc(s.peer || 'peer unknown')}${s.sni ? ` · SNI ${esc(s.sni)}` : ''}</p>
          <p>${s.steady}/${s.runs} steady samples · ${number(s.loss_pct)}% unanswered probes · RTT ${number(s.rtt_ms)} ms · ${s.jitter_samples ? `jitter ${number(s.jitter_ms)} ms` : 'jitter not measured'}</p>
          <p>${s.speed_samples ? `Download ${number(s.median_download_mbps)} / upload ${number(s.median_upload_mbps)} Mb/s` : 'Throughput not measured'}</p></article>`).join('')}
        ${(r.runs || []).map(run => `<details><summary>${esc(new Date(run.at).toLocaleString())} · ${esc(run.side)} · ${run.complete ? 'complete' : 'partial'}</summary>
          <ul>${(run.paths || []).map(p => `<li>${esc(p.family)} ${esc(p.host)} · ${esc(p.state)} · ${(p.rows || []).length} measurements</li>`).join('')}</ul></details>`).join('')}
        ${!(r.runs || []).length ? '<p>No saved tests yet. Browser and terminal tests are saved here after a run.</p>' : ''}`;
    } catch (e) { error(history,e); }
  }
  view.querySelector('#loadHistory').onclick = loadHistory;
  loadHistory();
  ctx.setTeardown(() => { alive=false; if (plan) api.automatic({action:'cancel',id:plan}).catch(() => {}); if(peerPlan) api.managedLink({action:'cancel',id:peerPlan}).catch(()=>{}); });
}
