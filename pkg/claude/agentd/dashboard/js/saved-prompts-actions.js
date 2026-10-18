const API = '/api/saved-prompts';

// Each call takes the fetch of the node whose library it edits (node-api.js
// nodeFetch); the page's own by default.
async function request(path = '', options = {}, fetchImpl = (...args) => globalThis.fetch(...args)) {
  const response = await fetchImpl(API + path, { credentials: 'same-origin', ...options });
  if (!response.ok) throw new Error((await response.text()) || `HTTP ${response.status}`);
  return response.status === 204 ? null : response.json();
}

export const loadSavedPrompts = (fetchImpl) => request('', {}, fetchImpl);
export const deleteSavedPrompt = (name, fetchImpl) => request('/' + encodeURIComponent(name), { method: 'DELETE' }, fetchImpl);
export const saveSavedPrompt = (previousName, prompt, fetchImpl) => request(
  previousName ? '/' + encodeURIComponent(previousName) : '', {
    method: previousName ? 'PATCH' : 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(prompt),
  },
  fetchImpl,
);

// The export envelope. Import also accepts a bare [{name, body}] array.
const EXPORT_KIND = 'tclaude.saved-prompts';

export function savedPromptsExportBlob(prompts) {
  const body = { kind: EXPORT_KIND, version: 1, prompts: prompts.map(({ name, body: text }) => ({ name, body: text })) };
  return new Blob([JSON.stringify(body, null, 2) + '\n'], { type: 'application/json' });
}

// parseSavedPromptsImport validates an export file's shape. Name and body
// rules stay the daemon's; this only refuses a file that is not prompts.
export function parseSavedPromptsImport(raw) {
  const data = JSON.parse(raw);
  const list = Array.isArray(data) ? data : data?.kind === EXPORT_KIND ? data.prompts : null;
  if (!Array.isArray(list)) throw new Error('not a saved-prompts export');
  const seen = new Set();
  const out = [];
  for (const item of list) {
    const name = typeof item?.name === 'string' ? item.name.trim() : '';
    if (!name || typeof item.body !== 'string') throw new Error('every prompt needs a name and a body');
    if (seen.has(name)) continue;
    seen.add(name);
    out.push({ name, body: item.body });
  }
  if (!out.length) throw new Error('the file holds no prompts');
  return out;
}
