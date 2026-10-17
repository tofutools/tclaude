// dock-library.js -- the operations the right panel's library offers on every
// kind (spawn profiles, group templates, roles, saved prompts, sandbox
// profiles, processes): search, pin to top, rename, export and import.
//
// - Search matches every whitespace-separated term against an item's name and
//   its data (summary chips, aliases, descriptions, prompt text). A term that
//   names the section itself ("roles", "sandbox") keeps that whole section.
// - Pins are a dashboard preference (dashPrefs), so a pinned item leads its
//   section; the rest stays alphabetical as the daemon sends it.
// - Export is a config bundle (tclaude config export --only <section>/<name>):
//   one portable format for every kind, with home-relative path placeholders
//   and suspected credentials flagged before anything leaves the node.
// - Import reads a config bundle into one review, or a kind's own export
//   format (spawn profiles, sandbox profiles, a group template) into that
//   kind's existing importer.
// - Rename goes through the daemon (/api/library/rename), which rewrites the
//   item with only its name changed; references follow, since the store keys
//   them by ID. Processes rename and clone as a new template version.

export const PINS_PREF = 'tclaude.dash.dock.pins.';

// LIBRARY_EVENT asks the self-loading sections (sandbox profiles, processes)
// to reload after a rename or an import changed what they list.
export const LIBRARY_EVENT = 'tclaude:library-changed';

// BUNDLE_SECTIONS maps a dock section key to its config-bundle section.
export const BUNDLE_SECTIONS = Object.freeze({
  profiles: 'profiles',
  templates: 'templates',
  roles: 'roles',
  saved_prompts: 'saved-prompts',
  sandbox_profiles: 'sandbox-profiles',
  processes: 'process-templates',
});

// Kinds the daemon renames by name; processes rename by template id here.
export const RENAME_KINDS = Object.freeze(['profiles', 'templates', 'roles', 'saved_prompts', 'sandbox_profiles']);

export function searchTerms(query) {
  return String(query || '').toLowerCase().split(/\s+/).filter(Boolean);
}

// matchesSearch: every term appears in at least one of the texts.
export function matchesSearch(terms, texts) {
  if (!terms.length) return true;
  const hay = texts.filter((text) => text != null && text !== '').map((text) => String(text).toLowerCase());
  return terms.every((term) => hay.some((text) => text.includes(term)));
}

// leafTexts collects every string and number in an item, so a search finds an
// item by any of its values (a model, a path, a prompt line) but not by the
// field names every item of the kind shares.
export function leafTexts(value, out = [], depth = 0) {
  if (value == null || depth > 8) return out;
  if (typeof value === 'string' || typeof value === 'number') out.push(String(value));
  else if (Array.isArray(value)) value.forEach((entry) => leafTexts(entry, out, depth + 1));
  else if (typeof value === 'object') Object.values(value).forEach((entry) => leafTexts(entry, out, depth + 1));
  return out;
}

// filterSection decides what a section shows for a search: all of it when
// the terms describe the section (its title or key), else the matching items.
export function filterSection({ terms, titles = [], items, textsOf }) {
  if (!terms.length || matchesSearch(terms, titles)) return { items, whole: !!terms.length };
  return { items: items.filter((item) => matchesSearch(terms, textsOf(item))), whole: false };
}

export function pinnedNames(prefs, section) {
  try {
    const raw = JSON.parse(prefs.getItem(PINS_PREF + section) || '[]');
    return Array.isArray(raw) ? raw.filter((name) => typeof name === 'string') : [];
  } catch {
    return [];
  }
}

function writePins(prefs, section, names) {
  if (names.length) prefs.setItem(PINS_PREF + section, JSON.stringify(names));
  else prefs.removeItem(PINS_PREF + section);
}

export function togglePin(prefs, section, name) {
  const pins = pinnedNames(prefs, section);
  const next = pins.includes(name) ? pins.filter((pin) => pin !== name) : [...pins, name];
  writePins(prefs, section, next);
  return next.includes(name);
}

// renamePin keeps a renamed item pinned; dropPin forgets a deleted one.
export function renamePin(prefs, section, from, to) {
  const pins = pinnedNames(prefs, section);
  if (pins.includes(from)) writePins(prefs, section, pins.map((pin) => (pin === from ? to : pin)));
}

export function dropPin(prefs, section, name) {
  const pins = pinnedNames(prefs, section);
  if (pins.includes(name)) writePins(prefs, section, pins.filter((pin) => pin !== name));
}

// orderPinned puts pinned items first, in the order they were pinned, and
// keeps the rest in the order given.
export function orderPinned(items, nameOf, pins) {
  if (!pins.length) return items;
  const rank = new Map(pins.map((name, index) => [name, index]));
  const pinned = items.filter((item) => rank.has(nameOf(item))).sort((a, b) => rank.get(nameOf(a)) - rank.get(nameOf(b)));
  return [...pinned, ...items.filter((item) => !rank.has(nameOf(item)))];
}

async function errorOf(response) {
  const raw = await response.text().catch(() => '');
  try {
    const body = JSON.parse(raw);
    return Object.assign(new Error(body.message || body.error || raw || `HTTP ${response.status}`), { status: response.status, code: body.code, body });
  } catch {
    return Object.assign(new Error(raw || `HTTP ${response.status}`), { status: response.status });
  }
}

function bundleFileName(section, name) {
  const safe = (value) => String(value).replace(/[^A-Za-z0-9._-]+/g, '-').replace(/^-+|-+$/g, '') || 'item';
  return `${name ? `${safe(section)}-${safe(name)}` : safe(section)}.tclaude.json`;
}

// exportBundle downloads a config bundle of one item, or of the whole
// section when name is empty. Suspected credentials stop the export until the
// operator confirms, with each flagged field named. Resolves to false when
// declined.
export async function exportBundle({ section, name = '', fetchImpl = (...args) => fetch(...args), confirm, download }) {
  const bundleSection = BUNDLE_SECTIONS[section];
  if (!bundleSection) throw new Error(`cannot export ${section}`);
  const query = new URLSearchParams({ only: name ? `${bundleSection}/${name}` : bundleSection });
  const get = (allow) => fetchImpl(`/api/config-bundle/export?${query}${allow ? '&allow_flagged=true' : ''}`, { credentials: 'same-origin' });
  let response = await get(false);
  if (response.status === 422) {
    const error = await errorOf(response);
    const flags = error.body?.flags || [];
    const ok = await confirm({
      title: 'Export anyway?',
      body: `These fields look like credentials:\n${flags.map((flag) => `• ${flag.item} ${flag.field}: ${flag.hint}`).join('\n')}\n\nThey would leave this node in the file. Remove them first, or export anyway if they are not secrets.`,
      preformatted: true,
      okLabel: 'Export anyway',
    });
    if (!ok) return false;
    response = await get(true);
  }
  if (!response.ok) throw await errorOf(response);
  download(bundleFileName(bundleSection, name), await response.json());
  return true;
}

// detectImport classifies a file by its format marker.
export function detectImport(text) {
  let value;
  try {
    value = JSON.parse(text);
  } catch {
    return { kind: 'unknown', reason: 'not JSON' };
  }
  switch (value?.format) {
    case 'tclaude-config-bundle': return { kind: 'bundle', value };
    case 'tclaude-spawn-profiles': return { kind: 'profiles', value, raw: text };
    case 'tclaude-sandbox-profiles': return { kind: 'sandbox_profiles', value, raw: text };
    case 'tclaude-task-force': return { kind: 'templates', value, raw: text };
    default: return { kind: 'unknown', reason: value?.format ? `unknown format ${value.format}` : 'no format marker' };
  }
}

export async function previewBundleImport({ bundle, skip = [], replace = false, values = {}, keepPaths = false, apply = false, fetchImpl = (...args) => fetch(...args) }) {
  const response = await fetchImpl('/api/config-bundle/import', {
    method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ bundle, skip, replace, values, keep_paths: keepPaths, apply }),
  });
  if (!response.ok) throw await errorOf(response);
  return response.json();
}

export async function renameItem({ section, name, to, fetchImpl = (...args) => fetch(...args) }) {
  if (section === 'processes') return renameProcess({ id: name, to, fetchImpl });
  const response = await fetchImpl('/api/library/rename', {
    method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ kind: section, name, to }),
  });
  if (!response.ok) throw await errorOf(response);
  return response.json();
}

async function processView(id, fetchImpl) {
  const response = await fetchImpl(`/v1/process/templates/${encodeURIComponent(id)}?authorship=omit`, { credentials: 'same-origin' });
  if (!response.ok) throw await errorOf(response);
  return response.json();
}

async function postProcess(path, body, fetchImpl) {
  const response = await fetchImpl(path, {
    method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body),
  });
  if (!response.ok) throw await errorOf(response);
  return response.json();
}

// renameProcess saves the head with a new display name as the next version;
// the template id, its pins and its history stay.
export async function renameProcess({ id, to, fetchImpl = (...args) => fetch(...args) }) {
  const view = await processView(id, fetchImpl);
  const body = { template: { ...view.template, name: to }, sourceHash: view.sourceHash };
  if (view.layout) body.layout = view.layout;
  return postProcess(`/v1/process/templates/${encodeURIComponent(id)}`, body, fetchImpl);
}

// cloneProcess creates a new template (a fresh id, no history, no pins) from
// the head of another.
export async function cloneProcess({ id, to, fetchImpl = (...args) => fetch(...args) }) {
  const view = await processView(id, fetchImpl);
  const { id: _drop, ...template } = view.template || {};
  const body = { template: { ...template, name: to } };
  if (view.layout) body.layout = view.layout;
  return postProcess('/v1/process/templates', body, fetchImpl);
}

// freeName suggests "<name>-copy", bumping a number until it is free.
export function freeName(name, taken) {
  const names = new Set(taken);
  for (let n = 1; ; n += 1) {
    const candidate = n === 1 ? `${name}-copy` : `${name}-copy-${n}`;
    if (!names.has(candidate)) return candidate;
  }
}
