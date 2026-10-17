import { esc } from './dom.js';

const labels = { download: 'Download', realtime: 'Games / calls', stability: 'Stability' };

// The server ranks the measured candidates. The browser only presents that
// verdict, preserving the selected path and SNI instead of ranking it again.
export function purposeRecommendations(choices, showPath = false) {
  const shown = (choices || []).filter(c => labels[c.purpose] && c.transport);
  if (!shown.length) return '';
  const number = n => Number(n || 0).toFixed(1);
  return `<div class="ct-bgrid ct-purposes">${shown.map(c => {
    const identity = [showPath ? c.family : '', showPath ? c.host : '', c.sni].filter(Boolean).join(' · ');
    const evidence = [
      c.speed_measured ? `${number(c.download_mbps)} ↓ / ${number(c.upload_mbps)} ↑ Mb/s` : 'Speed not measured',
      c.samples >= 2 ? `${number(c.rtt_ms)} ms RTT · ${number(c.jitter_ms)} ms jitter · ${number(c.loss_pct)}% loss · ${c.samples} echoes` : 'Echo variation not measured',
      c.speed_measured && !c.speed_stable ? 'Speed varied; repeat to confirm' : '',
    ].filter(Boolean).join(' · ');
    return `<div class="ct-bc"><span>${labels[c.purpose]}</span><b>${esc(c.transport)}</b><small>${esc(identity)}</small><small>${esc(evidence)}</small></div>`;
  }).join('')}</div><p class="tl-meta">Based on this sample. Stability does not measure long-term uptime; games/calls use echo latency and variation.</p>`;
}
