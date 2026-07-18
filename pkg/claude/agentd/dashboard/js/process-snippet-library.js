// process-snippet-library.js -- strict browser boundary for the local
// operator's reusable process-editor snippets. The server persists the same
// v1 selection envelope used by copy/paste; this module revalidates every
// response before the controller may insert it.

import { validateProcessSelectionPayload } from './process-editor-clipboard.js';

const API = '/api/process/snippets';
const SNIPPET_ID = /^psn_[0-9a-f]{32}$/;
export const PROCESS_SNIPPET_NAME_MAX_RUNES = 80;
export const PROCESS_SNIPPET_NAME_MAX_BYTES = 160;
export const PROCESS_SNIPPET_UNAVAILABLE = 'This custom snippet is unavailable because its stored format is invalid or unsupported.';

function apiError(response, body) {
  const error = new Error(body?.message || body?.error || `${response.status} ${response.statusText}`);
  error.status = response.status;
  error.code = body?.code || '';
  return error;
}

async function request(path, options = {}) {
  const response = await fetch(path, {
    credentials: 'same-origin',
    ...options,
    headers: options.body ? { 'Content-Type': 'application/json', ...(options.headers || {}) } : options.headers,
  });
  const body = await response.json().catch(() => ({}));
  if (!response.ok) throw apiError(response, body);
  return body;
}

function safeName(value) {
  const name = typeof value === 'string' ? value.trim() : '';
  if (!name || [...name].length > PROCESS_SNIPPET_NAME_MAX_RUNES
      || new TextEncoder().encode(name).length > PROCESS_SNIPPET_NAME_MAX_BYTES
      || /[\u0000-\u001f\u007f-\u009f]/u.test(name)) return '';
  return name;
}

export function normalizeProcessSnippet(raw) {
  if (!raw || typeof raw !== 'object' || !SNIPPET_ID.test(raw.id)
      || !Number.isSafeInteger(raw.revision) || raw.revision < 1) return null;
  const name = safeName(raw.name) || 'Unavailable custom snippet';
  const base = {
    id: raw.id, name, revision: raw.revision,
    createdAt: typeof raw.createdAt === 'string' ? raw.createdAt : '',
    updatedAt: typeof raw.updatedAt === 'string' ? raw.updatedAt : '',
    available: false, unavailableReason: PROCESS_SNIPPET_UNAVAILABLE,
    payload: null,
  };
  if (raw.available !== true || !raw.envelope) return base;
  try {
    base.payload = validateProcessSelectionPayload(raw.envelope);
    base.available = true;
    base.unavailableReason = '';
  } catch {
    // One stale/corrupt item remains visible and manageable, but can never
    // reach editor mutation. Never retain or surface the rejected raw bytes.
  }
  return base;
}

export function normalizeProcessSnippetLibrary(raw) {
  const snippets = Array.isArray(raw?.snippets)
    ? raw.snippets.map(normalizeProcessSnippet).filter(Boolean) : [];
  snippets.sort((left, right) => left.name.localeCompare(right.name, undefined, { sensitivity: 'base' })
    || left.id.localeCompare(right.id));
  return {
    generation: Number.isSafeInteger(raw?.generation) && raw.generation >= 0 ? raw.generation : 0,
    snippets,
  };
}

export async function loadProcessSnippets({ signal } = {}) {
  return normalizeProcessSnippetLibrary(await request(API, { signal }));
}

export async function createProcessSnippet(name, envelope, { signal } = {}) {
  const body = await request(API, {
    method: 'POST', signal, body: JSON.stringify({ name, envelope }),
  });
  return { generation: body.generation, snippet: normalizeProcessSnippet(body.snippet) };
}

export async function renameProcessSnippet(snippet, name, { signal } = {}) {
  const body = await request(`${API}/${encodeURIComponent(snippet.id)}`, {
    method: 'PATCH', signal, body: JSON.stringify({ name, revision: snippet.revision }),
  });
  return { generation: body.generation, snippet: normalizeProcessSnippet(body.snippet) };
}

export async function deleteProcessSnippet(snippet, { signal } = {}) {
  return request(`${API}/${encodeURIComponent(snippet.id)}`, {
    method: 'DELETE', signal, body: JSON.stringify({ revision: snippet.revision }),
  });
}
