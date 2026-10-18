// The MASQUE TLS form is shared by HTTP/3, HTTP/2 and native CONNECT-IP.
export function directConditionMatches(condition, side, carrier) {
  return condition.split('-').every(part => {
    if (part === 'server' || part === 'client') return side === part;
    if (part === 'masque') return ['masque', 'masque-h2', 'connect-ip'].includes(carrier);
    return carrier === part;
  });
}
