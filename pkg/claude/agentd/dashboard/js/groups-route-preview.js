// Use a distinct loopback hostname for each lease: cookies are scoped to a
// hostname, not a port. A raw 127.0.0.1 preview would receive dashboard cookies.
export function hostRoutePreviewURL(lease, { local, hostname } = {}) {
  if (!local || !hostname) return '';
  const id = /^hlease_([a-f0-9]{32})$/.exec(String(lease?.id || ''));
  const endpoint = /^127\.0\.0\.1:([1-9]\d{0,4})$/.exec(String(lease?.endpoint || ''));
  if (!id || !endpoint || Number(endpoint[1]) > 65535) return '';
  const host = `route-${id[1]}.localhost`;
  if (String(hostname).toLowerCase().replace(/\.+$/, '') === host) return '';
  return `http://${host}:${endpoint[1]}/`;
}
