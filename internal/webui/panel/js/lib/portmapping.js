// Keep the original spelling until a row changes; opening the editor must not
// turn every same-port mapping into a new setting.
export function splitMapping(raw) {
  const at = raw.indexOf('=');
  const source = (at < 0 ? raw : raw.slice(0, at)).trim();
  const port = source.slice(source.lastIndexOf(':') + 1);
  return { raw, source, target: at < 0 ? port : raw.slice(at + 1).trim() };
}

export function mappingValue(original, source, target) {
  source = source.trim(); target = target.trim();
  if (source === original.source && target === original.target) return original.raw;
  return `${source}=${target}`;
}
