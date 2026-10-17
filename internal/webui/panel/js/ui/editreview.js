import { esc } from '../lib/dom.js';
import { confirmBox } from './confirm.js';

export function reviewChanges(name, preview, restoring = false) {
  return confirmBox({
    title: `${restoring ? 'Restore settings' : 'Review changes'} · ${esc(name)}`,
    body: 'The tunnel restarts after applying. If it fails to become ready, the previous settings and service state are restored. Credentials are hidden.',
    lines: preview.changes.map(change => ({ text: `${change.field}\n${change.before} → ${change.after}` })),
    go: restoring ? 'Restore and restart' : 'Apply and restart',
    defaultNo: true,
  });
}
