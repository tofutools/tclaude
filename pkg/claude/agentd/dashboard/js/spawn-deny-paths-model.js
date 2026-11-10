// spawn-deny-paths-model.js — the pure rules behind the spawn dialog's
// "Deny paths" control (spawn-deny-paths.js), kept apart so the spawn model
// can build the request without importing the component.

// normalizeDenyPath is the path as the request carries it, or '' when it is
// not one the daemon accepts: absolute, or under the daemon's home by "~/".
export function normalizeDenyPath(text) {
  let path = String(text ?? '').trim();
  if (!path) return '';
  if (!(path === '~' || path.startsWith('~/') || path.startsWith('/'))) return '';
  while (path.length > 1 && path.endsWith('/')) path = path.slice(0, -1);
  return path;
}

// denyPathProblem names what is wrong with a typed path, or ''.
export function denyPathProblem(text) {
  const path = String(text ?? '').trim();
  if (!path) return '';
  return normalizeDenyPath(path) ? '' : `${path}: use an absolute path or ~/…`;
}

// denyCoversCwd reports whether a denied path covers cwd, compared as
// written. A "~" path against an absolute cwd cannot be judged here; the
// daemon judges it.
export function denyCoversCwd(path, cwd) {
  const dir = normalizeDenyPath(cwd);
  if (!path || !dir) return false;
  if (path.startsWith('~') !== dir.startsWith('~')) return false;
  return dir === path || dir.startsWith(path === '/' ? '/' : `${path}/`);
}

// sandboxDenyPaths is what the request sends: the chips plus a valid path
// still in the box, so a typed path is not lost to a spawn clicked before
// Enter.
export function sandboxDenyPaths(draft) {
  const paths = [...(draft?.sandboxDeny || [])];
  const pending = normalizeDenyPath(draft?.sandboxDenyDraft);
  if (pending && !paths.includes(pending)) paths.push(pending);
  return paths;
}
