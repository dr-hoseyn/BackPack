import { el } from '../lib/dom.js';
import { splitMapping, mappingValue } from '../lib/portmapping.js';

export function mountPortMappings(root, settings, direct) {
  const field = root.querySelector('[name="ports"]');
  if (!field || field.hasAttribute('data-off')) return;
  const host = direct && settings.layer !== 4
    ? (settings.peerIp || 'peer tunnel IP').split('/')[0] : '127.0.0.1';
  field.type = 'hidden';
  field.closest('.f')?.querySelector('.hint')?.remove();
  const box = el('div', { class: 'port-mappings' });
  const rows = el('div', { class: 'mapping-rows' });
  const empty = el('p', { class: 'hint', text: 'No forwarded ports. Add a mapping to expose a service.' });
  const hint = el('p', { class: 'hint', text: `Source is where users connect on this server. Target is reached from the peer; a port alone uses ${host}. The tunnel port is separate.` });
  const changed = () => {
    field.value = [...rows.children].map(row => row.mappingValue()).join(',');
    empty.hidden = rows.children.length > 0;
    field.dispatchEvent(new Event('input', { bubbles: true }));
  };
  function add(raw = '') {
    const original = splitMapping(raw);
    const source = el('input', { type: 'text', value: original.source, placeholder: '443', required: true, 'aria-label': 'Source port on this server' });
    const target = el('input', { type: 'text', value: original.target, placeholder: '8443 or host:port', required: true, 'aria-label': 'Target port or address on peer' });
    const row = el('div', { class: 'mapping-row' },
      el('label', {}, el('span', { text: 'Source · this server' }), source),
      el('span', { class: 'mapping-arrow', 'aria-hidden': 'true', text: '→' }),
      el('label', {}, el('span', { text: 'Target · peer' }), target),
      el('button', { type: 'button', class: 'mapping-remove', 'aria-label': 'Delete mapping', text: 'Delete', on: { click: () => { row.remove(); changed(); } } }),
    );
    row.mappingValue = () => mappingValue(original, source.value, target.value);
    row.addEventListener('input', changed);
    rows.append(row);
    return source;
  }
  for (const raw of field.value.split(',').map(s => s.trim()).filter(Boolean)) add(raw);
  box.append(hint, rows, empty, el('button', { type: 'button', class: 'mapping-add', text: 'Add mapping', on: { click: () => { const source = add(); changed(); source.focus(); } } }));
  empty.hidden = rows.children.length > 0;
  field.after(box);
}
