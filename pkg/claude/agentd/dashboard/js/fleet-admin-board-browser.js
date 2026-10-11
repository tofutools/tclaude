import { h } from 'preact';
import { useEffect, useMemo, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { ManagementOverlay as Overlay } from './management-overlay.js';
import { ImportDialog, PublishDialog, canPost, postedBy } from './fleet-admin-board-items.js';
import { dashPrefs } from './prefs.js';
import { applySortState, persistTableSort, persistedTableSort, timestampSortValue } from './sort.js';

const html = htm.bind(h);

// The content browser: everything posted to the boards this node is a member
// of, in one place. It reads the same per-board item lists as the board
// detail view, shows who signed each item and whether that is a peer this
// node already trusts, and installs through the same preview-then-confirm
// flow. Nothing installs from a click: every target except a plain browser
// download previews first and applies only on an explicit confirm.

// VIEW_PREF remembers List or Cards across reloads (dashboard prefs live in
// the daemon, since localStorage is lost with the dashboard's random port).
export const VIEW_PREF = 'tclaude.dash.boards.contentView';
export const VIEWS = Object.freeze([{ id: 'list', label: 'List' }, { id: 'cards', label: 'Cards' }]);

// SORT_TABLE is this browser's key in the dashboard's shared sort prefs; List
// headers and the Cards dropdown read and write the same {col, dir}.
export const SORT_TABLE = 'boardContent';
export const SORT_COLS = Object.freeze([
  { col: 'name', label: 'Name' }, { col: 'kind', label: 'Kind' }, { col: 'publisher', label: 'Posted by' },
  { col: 'board', label: 'Board' }, { col: 'version', label: 'Version' }, { col: 'updated', label: 'Updated' },
]);
// Rows the daemon cannot date or number yet sort last (applySortState).
export const SORT_ACCESSORS = Object.freeze({
  name: (r) => r.name,
  kind: (r) => r.kind,
  publisher: (r) => r.by,
  board: (r) => r.board.name,
  // Version IDs are random: only the hub's sequence number orders them.
  version: (r) => (typeof r.seq === 'number' ? r.seq : null),
  updated: (r) => timestampSortValue(r.updated_at),
});

// nextSort is the dashboard tables' header cycle: asc, desc, then unsorted.
export function nextSort(cur, col) {
  if (!cur || cur.col !== col) return { col, dir: 'asc' };
  return cur.dir === 'asc' ? { col, dir: 'desc' } : null;
}

export const KINDS = Object.freeze([{ id: 'skill', label: 'Skills' }, { id: 'config', label: 'Config' }]);
export const TRUST = Object.freeze({
  mine: { label: 'this node', cls: 'fa-cb-t-me', title: 'Posted by this node' },
  trusted: { label: '✓ trusted peer', cls: 'fa-cb-t-ok', title: 'Signed by a peer this node trusts' },
  unknown: { label: '? unknown publisher', cls: 'fa-cb-t-unk', title: 'Signed by a node this node has not trusted' },
});

function errText(error) { return error?.message || String(error); }
const short = (v) => String(v || '').slice(0, 10);
const when = (iso) => { const t = Date.parse(iso || ''); return Number.isFinite(t) ? new Date(t).toLocaleDateString() : '—'; };
function size(n) {
  if (!(n >= 0)) return '—';
  return n < 1024 ? `${n} B` : n < 1 << 20 ? `${(n / 1024).toFixed(1)} KiB` : `${(n / (1 << 20)).toFixed(1)} MiB`;
}

// trustOf classifies an item's signer against this node's peers.
export function trustOf(publisher, view) {
  if (publisher && publisher === view?.self?.id) return 'mine';
  return (view?.trusted || []).some((p) => p.id === publisher) ? 'trusted' : 'unknown';
}

// peerName is a signer's readable name: this node, or a trusted peer's
// label. Anyone else stays a bare instance ID: an untrusted node picks its
// own name, and could pick a trusted peer's.
export function peerName(id, view) {
  if (id && id === view?.self?.id) return 'this node';
  return (view?.trusted || []).find((p) => p.id === id)?.label || '';
}

// TOKEN is one search word: a filter (its value quoted when it has spaces,
// as board names may) or free text.
const TOKEN = /(\w+):"([^"]*)"?|(\w+):(\S+)|(\S+)/g;
const tokens = (q) => [...String(q || '').matchAll(TOKEN)].map((m) => (m[1] ? { key: m[1].toLowerCase(), value: m[2], raw: m[0] }
  : m[3] ? { key: m[3].toLowerCase(), value: m[4], raw: m[0] } : { raw: m[5] }));

// parseQuery splits the search box into kind:/publisher:/board: filters and
// free text. Unknown prefixes stay free text.
export function parseQuery(q) {
  const out = { kind: '', publisher: '', board: '', text: [] };
  for (const t of tokens(q)) {
    if (['kind', 'publisher', 'board'].includes(t.key) && t.value) out[t.key] = t.value.toLowerCase();
    else out.text.push(t.raw.toLowerCase());
  }
  return out;
}

// withToken sets (or, when it is already set to value, clears) one filter in
// the query text, so facets, tabs and typing all edit the same search.
export function withToken(q, key, value) {
  const rest = tokens(q).filter((t) => t.key !== key).map((t) => t.raw);
  const had = parseQuery(q)[key] === String(value).toLowerCase();
  const v = String(value || '');
  return [...(v && !had ? [/[\s"]/.test(v) ? `${key}:"${v.replace(/"/g, '')}"` : `${key}:${v}`] : []), ...rest].join(' ');
}

// matches applies a parsed query to one browser row.
export function matches(row, f, exact = false) {
  if (f.kind && row.kind !== f.kind) return false;
  // A board's exact name picks that board alone; otherwise any board whose
  // name contains the text.
  if (f.board && (exact ? row.board.name.toLowerCase() !== f.board : !row.board.name.toLowerCase().includes(f.board))) return false;
  if (f.publisher) {
    const known = ['trusted', 'unknown', 'mine'].includes(f.publisher);
    if (known ? row.trust !== f.publisher : !`${row.by} ${row.publisher}`.toLowerCase().includes(f.publisher)) return false;
  }
  const hay = `${row.name} ${row.description || ''} ${row.by} ${row.publisher} ${row.board.name}`.toLowerCase();
  return f.text.every((t) => hay.includes(t));
}

// browserRows flattens per-board item lists into one list, newest board
// order first, with trust and display names resolved once.
export function browserRows(perBoard, view) {
  const rows = [];
  for (const { board, items } of perBoard) {
    for (const it of items || []) {
      const by = postedBy(it, (id) => peerName(id, view));
      rows.push({ ...it, key: `${board.id}/${it.id}`, board, kind: it.invalid ? 'invalid' : it.kind || 'config', version: it.latest_version || it.version, trust: trustOf(it.publisher, view), by });
    }
  }
  return rows;
}

function TrustBadge({ trust, compact = false }) {
  const t = TRUST[trust] || TRUST.unknown;
  return html`<span class=${`fa-cb-trust ${t.cls}`} title=${t.title} data-trust=${trust}>${compact ? t.label.split(' ')[0] : t.label}</span>`;
}

function KindChip({ kind }) {
  return html`<span class=${`fa-cb-kind fa-cb-k-${kind}`}>${kind}</span>`;
}

// fingerprint shortens a base64 public key for display.
const fingerprint = (pub) => (pub ? `${String(pub).slice(0, 8)}…${String(pub).slice(-8)}` : '—');

// useItemFiles fetches (verifies and scans) a version on this node and lists
// its files. Skills come from a target-less import preview, which writes
// nothing and is the daemon's own account of scripts and executables.
function useItemFiles(row, actions) {
  const [state, setState] = useState(null);
  useEffect(() => {
    if (!row || row.invalid) { setState(null); return undefined; }
    let off = false;
    setState({ key: row.key, loading: true });
    // Arrowing through the list should not fetch every row it passes.
    const timer = setTimeout(async () => {
      try {
        await actions.fetchBoardItem(row.board.id, row.id, row.version);
        if (row.kind === 'skill') {
          const p = await actions.previewBoardItem(row.board.id, row.id, row.version, {});
          if (!off) setState({ key: row.key, files: (p?.files || []).map((f) => ({ path: f.path, bytes: f.bytes, security: !!f.security, executable: !!f.executable, text: f.text, binary: !!f.binary })) });
        } else {
          const c = await actions.boardItemContents(row.board.id, row.id, row.version);
          if (!off) setState({ key: row.key, files: (c?.entries || []).map((e) => ({ path: e.path, bytes: e.size, entry: true })) });
        }
      } catch (e) {
        if (!off) setState({ key: row.key, error: errText(e) });
      }
    }, 250);
    return () => { off = true; clearTimeout(timer); };
  }, [row?.key, row?.version]);
  // Until the effect runs for a new row, the old row's files are not its own.
  return state && state.key === row?.key ? state : { loading: true };
}

// FileList shows every file; scripts and executables are flagged, and any
// text file opens inline.
function FileList({ row, files, actions }) {
  const [open, setOpen] = useState({});
  const toggle = async (f) => {
    if (open[f.path]) { setOpen({ ...open, [f.path]: null }); return; }
    if (!f.entry) { setOpen({ ...open, [f.path]: { text: f.text, binary: f.binary } }); return; }
    setOpen({ ...open, [f.path]: { loading: true } });
    try {
      const r = await actions.boardItemEntry(row.board.id, row.id, row.version, f.path, 0, 1 << 18);
      setOpen((o) => ({ ...o, [f.path]: { text: String(r?.text ?? ''), truncated: !!r?.truncated } }));
    } catch (e) {
      setOpen((o) => ({ ...o, [f.path]: { error: errText(e) } }));
    }
  };
  return html`<div class="fa-cb-files">${files.map((f) => html`<div key=${f.path} class="fa-cb-file" data-file=${f.path}>
    <button type="button" class=${`fa-cb-file-row${f.security ? ' fa-cb-script' : ''}`} onClick=${() => toggle(f)} aria-expanded=${open[f.path] ? 'true' : 'false'}>
      <code>${f.security ? '⚠ ' : ''}${f.path}</code><span class="muted">${[f.executable && 'executable', f.security && 'security-relevant', size(f.bytes)].filter(Boolean).join(' · ')}</span></button>
    ${open[f.path] && html`<div class="fa-cb-file-body">${open[f.path].loading ? html`<div class="muted">Loading…</div>`
      : open[f.path].error ? html`<div class="fa-danger">${open[f.path].error}</div>`
      : open[f.path].binary ? html`<div class="fa-warn">Binary file: download the item to inspect its bytes.</div>`
      : html`<pre>${open[f.path].text}</pre>${open[f.path].truncated && html`<div class="muted">Showing the first 256 KiB.</div>`}`}</div>`}
  </div>`)}</div>`;
}

// ItemDetail is the item's provenance, trust and contents, with its actions.
export function ItemDetail({ row, view, actions, onInstall, onOpenBoard }) {
  const files = useItemFiles(row, actions);
  if (!row) return html`<div class="muted fa-cb-empty-detail">Select an item to see who posted it and what it contains.</div>`;
  if (row.invalid) return html`<div class="fa-cb-detail" data-detail=${row.key}><b>${row.name || 'Invalid item'}</b><div class="fa-danger">${row.error || 'could not be verified'}</div><div class="muted">It cannot be installed.</div></div>`;
  const prov = row.provenance || {};
  const scripts = (files?.files || []).filter((f) => f.security).length;
  return html`<div class="fa-cb-detail" data-detail=${row.key}>
    <div class="fa-row"><b class="fa-cb-title">${row.name}</b><${KindChip} kind=${row.kind} /><span class="muted">v ${short(row.version)} · ${row.board.name}</span><span class="spacer"></span>
      <button type="button" class="primary" data-cb-act="install" onClick=${() => onInstall(row)}>Install…</button></div>
    ${row.description && html`<div class="fa-cb-desc-full">${row.description}</div>`}
    ${row.trust === 'unknown' && html`<div class="fa-cb-banner" role="note"><b>Unknown publisher.</b> This node has not trusted <code>${row.publisher}</code>. The signature proves this version came from that node's key, unchanged; it does not tell you who runs that node.</div>`}
    <div class="fa-cb-kv">
      <span>Posted by</span><span>${row.by} <${TrustBadge} trust=${row.trust} /></span>
      <span>Instance</span><span><code>${row.publisher}</code></span>
      <span>Signing key</span><span><code title=${prov.pubkey || ''}>${fingerprint(prov.pubkey)}</code></span>
      <span>Signature</span><span class="fa-cb-ok">✓ verified on this node</span>
      <span>Content</span><span><code title=${row.digest || ''}>sha256 ${short(row.digest)}…</code> · ${size(row.bytes)}</span>
      <span>Credential scan</span><span>${files?.files ? html`<span class="fa-cb-ok">✓ clean when posted and when fetched here</span>` : files?.error ? html`<span class="fa-danger">${files.error}</span>` : html`<span class="muted">checked when posted; again when fetched</span>`}</span>
    </div>
    <div class="fa-cb-sect">Files${scripts ? html` · <span class="fa-sensitive">${scripts} security-relevant</span>` : ''}</div>
    ${!files || files.loading ? html`<div class="muted">Fetching and verifying…</div>` : files.error ? html`<div class="muted">Files unavailable.</div>` : html`<${FileList} row=${row} files=${files.files} actions=${actions} />`}
    ${onOpenBoard && html`<div class="fa-row"><button type="button" class="fa-link" data-cb-act="board" onClick=${() => onOpenBoard(row.board.id)}>Versions and more on ${row.board.name}…</button></div>`}
  </div>`;
}

// TARGETS are where a skill can go. A browser download writes nothing here,
// so it alone skips the preview.
export const TARGETS = Object.freeze([
  { id: 'harness', label: 'Harness user skill directory', note: 'Every session of that harness picks it up.' },
  { id: 'group', label: "A group's directory", note: "A project skill in the group's default directory: agents working there pick it up." },
  { id: 'custom', label: 'Custom directory', note: 'An existing directory you choose. Symlinks and .. are refused; the resolved path is shown in the preview.' },
  { id: 'download', label: 'Download', note: 'Save the verified archive through the browser. Nothing is written by tclaude.' },
]);

// skillChoices turns the dialog's state into preview/import choices.
export function skillChoices(o) {
  const targets = o.target === 'harness' ? o.harness : o.target === 'group' ? (o.group ? [o.group] : []) : o.target === 'custom' ? ['custom'] : [];
  return {
    ...(targets.length ? { skill_targets: [...targets] } : {}),
    ...(o.target === 'custom' && o.customDir.trim() ? { custom_dir: o.customDir.trim() } : {}),
    ...(o.replace ? { replace: true } : {}),
    ...(o.preserveExec ? { preserve_executable: true } : {}),
  };
}

const rootKind = (r) => r.kind || (String(r.id).startsWith('group:') ? 'group' : r.id === 'custom' ? 'custom' : 'harness');

// SkillInstallDialog: pick a target, preview exactly what lands where, then
// install with that preview's single-use token.
export function SkillInstallDialog({ row, actions, confirm, onClose, onDone }) {
  const [opts, setOpts] = useState({ target: 'harness', harness: [], group: '', customDir: '', replace: false, preserveExec: false });
  const [roots, setRoots] = useState(null);
  const [preview, setPreview] = useState(null);
  const [stale, setStale] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [customError, setCustomError] = useState('');
  const seq = useRef(0);
  const ref = [row.board.id, row.id, row.version];
  const run = (o, first = false) => {
    const mine = ++seq.current;
    setBusy(true); setError(''); setCustomError('');
    actions.fetchBoardItem(...ref).then(() => actions.previewBoardItem(...ref, first ? {} : skillChoices(o)))
      .then((p) => {
        if (mine !== seq.current) return;
        if (first || !roots) setRoots(p?.skill_roots || []);
        if (!first) { setPreview(p); setStale(false); }
      })
      .catch((e) => {
        if (mine !== seq.current) return;
        if (e?.code === 'custom_dir') setCustomError(errText(e)); else setError(errText(e));
      })
      .finally(() => { if (mine === seq.current) setBusy(false); });
  };
  useEffect(() => run(opts, true), []);
  const change = (patch) => { seq.current += 1; setBusy(false); setOpts({ ...opts, ...patch }); setStale(true); setCustomError(''); };
  const all = roots || [];
  const harness = all.filter((r) => rootKind(r) === 'harness');
  const groups = all.filter((r) => rootKind(r) === 'group');
  const choices = skillChoices(opts);
  const targetReady = opts.target === 'download' || (choices.skill_targets || []).length > 0 && (opts.target !== 'custom' || !!choices.custom_dir);
  const changes = preview?.changes || [];
  const live = changes.filter((c) => c.action !== 'unchanged');
  const replaces = live.filter((c) => c.action === 'replace');
  // An older daemon ignores targets it does not know: say so rather than
  // previewing "nothing to install".
  const unsupported = !stale && preview && opts.target === 'custom' && !(preview.skill_roots || []).some((r) => r.id === 'custom');
  const fresh = !!preview?.preview_token && !stale && !busy;
  const blocked = !fresh || unsupported || !live.length || (replaces.length > 0 && !opts.replace);
  const step = opts.target === 'download' ? 1 : fresh && live.length ? 2 : 1;
  const install = () => confirm({
    title: `Install skills/${preview?.skill || row.name}?`,
    body: `Writes ${live.map((c) => c.path || c.item).join(', ')}${replaces.length ? `, overwriting ${replaces.length} existing cop${replaces.length === 1 ? 'y' : 'ies'}` : ''}. From ${row.board.name}, signed by ${row.by}${row.trust === 'unknown' ? ' (a publisher this node has not trusted)' : ''}. ${opts.preserveExec ? 'Executable bits are kept: agents can run the listed scripts directly.' : 'Executable bits are removed; an interpreter can still run scripts.'}`,
    okLabel: 'Install',
    busyLabel: 'Installing…',
    action: () => actions.importBoardItem(...ref, preview.preview_token, choices),
  }).then((r) => { if (r) onDone(`Installed ${row.name} (${live.length} location${live.length === 1 ? '' : 's'})`); })
    .catch((e) => { setStale(true); setError(e?.code === 'preview_required' || e?.code === 'preview_changed' ? 'Something changed since the preview: preview again, then install.' : `${errText(e)}: preview again to retry.`); });
  const download = () => {
    setBusy(true); setError('');
    actions.fetchBoardItem(...ref).then(() => actions.downloadBoardItemArchive(...ref, { preserveExecutable: opts.preserveExec }))
      .then(() => onDone(`Downloading ${row.name}`)).catch((e) => setError(errText(e))).finally(() => setBusy(false));
  };
  const scripts = (preview?.files || []).filter((f) => f.security);
  return html`<${Overlay} id="fleet-cb-install" labelledby="fleet-cb-install-title" onClose=${onClose}>
    <h3 id="fleet-cb-install-title">Install ${row.name} <span class="muted">v ${short(row.version)}</span></h3>
    <div class="muted">From ${row.board.name}, posted by ${row.by} <${TrustBadge} trust=${row.trust} /></div>
    <div class="fa-cb-steps">${['Target', 'Preview', 'Install'].map((s, i) => html`<span key=${s} class=${i < step ? 'done' : i === step ? 'on' : ''}>${i + 1} · ${s}</span>`)}</div>
    ${TARGETS.map((t) => html`<label key=${t.id} class=${`fa-cb-opt${opts.target === t.id ? ' on' : ''}`} data-target=${t.id}>
      <input type="radio" name="fleet-cb-target" checked=${opts.target === t.id} onChange=${() => change({ target: t.id })} />
      <span class="fa-cb-opt-body"><span><b>${t.label}</b>${t.id === 'custom' && html` <span class="fa-badge">human only</span>`}</span><span class="muted">${t.note}</span>
        ${opts.target === t.id && t.id === 'harness' && (!roots ? html`<span class="muted">Loading…</span>` : html`${harness.map((r) => html`<label key=${r.id} class="fa-cb-sub"><input type="checkbox" data-skill-target=${r.id} checked=${opts.harness.includes(r.id)}
            onChange=${(e) => change({ harness: e.currentTarget.checked ? [...opts.harness, r.id] : opts.harness.filter((x) => x !== r.id) })} /> ${r.label} <code>${r.path}</code></label>`)}`)}
        ${opts.target === t.id && t.id === 'group' && (!roots ? html`<span class="muted">Loading…</span>` : groups.length ? html`<select id="fleet-cb-group" value=${opts.group} onChange=${(e) => change({ group: e.currentTarget.value })}>
            <option value="">pick a group…</option>${groups.map((r) => html`<option key=${r.id} value=${r.id} selected=${opts.group === r.id}>${r.label}</option>`)}</select>
            ${opts.group && html`<code class="fa-cb-path">${groups.find((r) => r.id === opts.group)?.path}</code>`}`
          : html`<span class="fa-warn">No group has a default directory${roots.some((r) => r.kind) ? '' : ', or this node does not support group targets yet'}.</span>`)}
        ${opts.target === t.id && t.id === 'custom' && html`<input id="fleet-cb-custom" class="fa-cb-input" value=${opts.customDir} placeholder="/absolute/path/to/a/skills/directory" autocomplete="off" spellcheck="false" onInput=${(e) => change({ customDir: e.currentTarget.value })} />
          ${customError && html`<span class="fa-danger" role="alert">${customError}</span>`}
          ${unsupported && html`<span class="fa-warn">This node cannot install to a custom directory yet.</span>`}`}
      </span></label>`)}
    <label class="fa-cb-check"><input type="checkbox" id="fleet-cb-exec" checked=${opts.preserveExec} onChange=${(e) => change({ preserveExec: e.currentTarget.checked })} />
      Keep executable bits${scripts.length ? html` on ${scripts.map((f, i) => html`${i ? ', ' : ''}<code>${f.path}</code>`)}` : ''}: agents can run them directly. I have inspected them. <span class="muted">(Otherwise they are removed; scripts stay readable.)</span></label>
    ${opts.target !== 'download' && html`<label class="fa-cb-check"><input type="checkbox" id="fleet-cb-replace" checked=${opts.replace} onChange=${(e) => change({ replace: e.currentTarget.checked })} />
      Overwrite a skill with this name if one exists <span class="muted">(the preview shows which)</span></label>`}
    ${opts.target !== 'download' && preview && !stale && html`<div class="fa-cb-sect">Preview</div>
      ${changes.length ? html`<table class="fa-table" id="fleet-cb-preview"><tbody>${changes.map((c) => html`<tr key=${c.item} data-change=${c.item}>
        <td><code>${c.path || c.item}</code></td>
        <td class=${c.action === 'replace' ? 'fa-warn' : c.action === 'unchanged' ? 'muted' : ''}>${c.action === 'replace' ? 'overwrites existing' : c.action === 'create' ? 'new' : 'unchanged'}</td></tr>`)}</tbody></table>`
        : html`<div class="muted">Nothing to install for this target.</div>`}
      ${(preview.warnings || []).map((w) => html`<div class="fa-warn">${w}</div>`)}`}
    ${error && html`<div class="fa-danger" role="alert">${error}</div>`}
    <div class="modal-buttons"><span class="spacer"></span>
      <button type="button" onClick=${onClose}>Cancel</button>
      ${opts.target === 'download'
        ? html`<button type="button" class="primary" id="fleet-cb-download" disabled=${busy} onClick=${download}>Download</button>`
        : html`<button type="button" id="fleet-cb-preview-btn" disabled=${busy || !targetReady} onClick=${() => run(opts)}>${busy ? 'Previewing…' : preview && !stale ? 'Preview again' : 'Preview'}</button>
          <button type="button" class="primary" id="fleet-cb-install-btn" disabled=${blocked} title=${stale ? 'Preview first' : ''} onClick=${install}>Install…</button>`}
    </div>
  </${Overlay}>`;
}

// UploadDialog picks the board to post to; posting itself is the board
// detail view's Post config… dialog, unchanged.
function UploadPicker({ boards, onPick, onClose }) {
  return html`<${Overlay} id="fleet-cb-upload" labelledby="fleet-cb-upload-title" onClose=${onClose}>
    <h3 id="fleet-cb-upload-title">Upload to which board?</h3>
    <div class="muted">Skills and config you post are signed by this node and readable by every member of the board.</div>
    <div class="fa-cb-pick">${boards.map((b) => html`<button type="button" key=${b.id} data-upload-board=${b.id} onClick=${() => onPick(b)}>${b.name}</button>`)}</div>
    <div class="modal-buttons"><span class="spacer"></span><button type="button" onClick=${onClose}>Cancel</button></div>
  </${Overlay}>`;
}

function Facets({ rows, query, setQuery, boards }) {
  const f = parseQuery(query);
  const count = (pred) => rows.filter(pred).length;
  const facet = (key, value, label, n) => html`<button type="button" key=${`${key}:${value}`} class=${`fa-cb-facet${f[key] === value.toLowerCase() ? ' on' : ''}`} data-facet=${`${key}:${value}`}
    onClick=${() => setQuery(withToken(query, key, value))}><span>${label}</span><span class="muted">${n}</span></button>`;
  return html`<div class="fa-cb-facets">
    <h5>Kind</h5>${KINDS.map((k) => facet('kind', k.id, k.label, count((r) => r.kind === k.id)))}
    <h5>Board</h5>${boards.map((b) => facet('board', b.name, b.name, count((r) => r.board.id === b.id)))}
    <h5>Publisher</h5>${facet('publisher', 'trusted', '✓ Trusted peers', count((r) => r.trust === 'trusted'))}${facet('publisher', 'unknown', '? Unknown', count((r) => r.trust === 'unknown'))}${facet('publisher', 'mine', 'This node', count((r) => r.trust === 'mine'))}
  </div>`;
}

function Card({ row, selected, onSelect }) {
  return html`<button type="button" class=${`fa-cb-card${selected ? ' sel' : ''}`} data-card=${row.key} onClick=${() => onSelect(row)}>
    <span class="fa-cb-card-meta">${row.invalid ? html`<span class="fa-danger">invalid</span>` : html`<${KindChip} kind=${row.kind} />`}<span class="muted">v ${short(row.version)} · ${row.board.name}</span></span>
    <b>${row.name}</b>
    ${row.description && html`<span class="muted fa-cb-desc">${row.description}</span>`}
    <span class="fa-cb-card-foot"><span class="fa-cb-by" title=${row.by}>${row.by}</span>${!row.invalid && html`<${TrustBadge} trust=${row.trust} />`}</span>
  </button>`;
}

// ContentBrowser lists every item on every board this node belongs to, as a
// dense list with a detail pane or as cards with facets.
export function ContentBrowser({ boards, view, actions, confirm, toast, onOpenBoard = null, prefs = dashPrefs, readSort = persistedTableSort, writeSort = persistTableSort }) {
  const [perBoard, setPerBoard] = useState(null);
  const [tick, setTick] = useState(0);
  const [query, setQuery] = useState('');
  const [mode, setModeState] = useState(() => (prefs.getItem(VIEW_PREF) === 'cards' ? 'cards' : 'list'));
  const [selected, setSelected] = useState('');
  const [dialog, setDialog] = useState(null);
  const [sort, setSortState] = useState(() => readSort(SORT_TABLE));
  // Updates go through the current value, so two quick clicks cycle twice.
  const setSort = (next) => setSortState((cur) => {
    const v = typeof next === 'function' ? next(cur) : next;
    writeSort(SORT_TABLE, v);
    return v;
  });
  const boardKey = (boards || []).map((b) => `${b.id}:${b.epoch ?? ''}`).join(',');
  useEffect(() => {
    let off = false;
    setPerBoard(null);
    Promise.all((boards || []).map((board) => actions.boardItems(board.id).then((items) => ({ board, items }), (e) => ({ board, items: [], error: errText(e) }))))
      .then((v) => { if (!off) setPerBoard(v); });
    return () => { off = true; };
  }, [boardKey, tick]);
  const setMode = (m) => { setModeState(m); prefs.setItem(VIEW_PREF, m); };
  const rows = useMemo(() => browserRows(perBoard || [], view), [perBoard, view]);
  const f = parseQuery(query);
  const exactBoard = (boards || []).some((b) => b.name.toLowerCase() === f.board);
  const shown = applySortState(rows.filter((r) => matches(r, f, exactBoard)), SORT_ACCESSORS, sort);
  const sel = shown.find((r) => r.key === selected) || null;
  const failed = (perBoard || []).filter((p) => p.error);
  const posters = (boards || []).filter((b) => canPost(b) && !b.frozen);
  const done = (msg) => { setDialog(null); toast(msg, false); setTick((n) => n + 1); };
  const install = (row) => setDialog(row.kind === 'skill' ? { kind: 'skill', row } : { kind: 'config', row });
  const upload = () => setDialog(posters.length === 1 ? { kind: 'publish', board: posters[0] } : { kind: 'pick' });
  const move = (delta) => {
    if (!shown.length) return;
    const i = shown.findIndex((r) => r.key === selected);
    setSelected(shown[Math.max(0, Math.min(shown.length - 1, (i < 0 ? -1 : i) + delta))].key);
  };
  const detail = (row) => html`<${ItemDetail} row=${row} view=${view} actions=${actions} onInstall=${install} onOpenBoard=${onOpenBoard} />`;
  const d = dialog;
  return html`<div class="fa-cb" id="fleet-content-browser" data-view=${mode}>
    <div class="fa-row">
      <input id="fleet-cb-search" class="fa-grow" placeholder="Filter by name, description, publisher or board" title="Also: kind:skill, publisher:trusted|unknown|mine, board:NAME" value=${query} autocomplete="off" spellcheck="false" onInput=${(e) => setQuery(e.currentTarget.value)} />
      ${mode === 'cards' && html`<span class="fa-cb-sort"><label for="fleet-cb-sort-col" class="muted">Sort</label>
        <select id="fleet-cb-sort-col" onChange=${(e) => setSort(e.currentTarget.value ? { col: e.currentTarget.value, dir: sort?.dir || 'asc' } : null)}>
          <option value="" selected=${!sort}>posted order</option>${SORT_COLS.map((c) => html`<option key=${c.col} value=${c.col} selected=${sort?.col === c.col}>${c.label}</option>`)}</select>
        <button type="button" id="fleet-cb-sort-dir" disabled=${!sort} title=${sort?.dir === 'desc' ? 'Descending: click for ascending' : 'Ascending: click for descending'}
          onClick=${() => setSort((cur) => cur && { ...cur, dir: cur.dir === 'asc' ? 'desc' : 'asc' })}>${sort?.dir === 'desc' ? '▼' : '▲'}</button></span>`}
      <span class="fa-cb-views" role="group" aria-label="View">${VIEWS.map((v) => html`<button type="button" key=${v.id} data-view-pick=${v.id} aria-pressed=${mode === v.id ? 'true' : 'false'} class=${mode === v.id ? 'on' : ''} onClick=${() => setMode(v.id)}>${v.label}</button>`)}</span>
      ${posters.length > 0 && html`<button type="button" class="primary" id="fleet-cb-upload-btn" onClick=${upload}>Upload…</button>`}
    </div>
    ${failed.length > 0 && html`<div class="fa-warn" id="fleet-cb-failed">Could not read ${failed.map((p) => p.board.name).join(', ')}: ${failed[0].error}</div>`}
    ${!perBoard ? html`<div class="muted">Loading…</div>` : !rows.length ? html`<div class="muted" id="fleet-cb-empty">Nothing has been posted to your boards yet.</div>`
      : mode === 'cards' ? html`<div class="fa-cb-cards-wrap">
          <${Facets} rows=${rows} query=${query} setQuery=${setQuery} boards=${boards} />
          <div class="fa-cb-cards" id="fleet-cb-cards">${shown.length ? shown.map((r) => html`<${Card} key=${r.key} row=${r} selected=${r.key === selected} onSelect=${(x) => { setSelected(x.key); setDialog({ kind: 'detail', row: x }); }} />`) : html`<div class="muted">No items match.</div>`}</div>
        </div>`
      : html`<div class="fa-cb-list-wrap">
          <div class="fa-cb-list">
            <div class="fa-cb-tabs" role="tablist">${[{ id: '', label: 'All' }, ...KINDS].map((k) => html`<button type="button" role="tab" key=${k.id || 'all'} data-kind-tab=${k.id || 'all'} aria-selected=${f.kind === k.id ? 'true' : 'false'} class=${f.kind === k.id ? 'on' : ''}
              onClick=${() => setQuery(withToken(query, 'kind', k.id || f.kind))}>${k.label} <span class="muted">${k.id ? rows.filter((r) => r.kind === k.id).length : rows.length}</span></button>`)}</div>
            <table class="fa-table" id="fleet-cb-table" tabindex="0" onKeyDown=${(e) => { if (e.key === 'ArrowDown' || e.key === 'ArrowUp') { e.preventDefault(); move(e.key === 'ArrowDown' ? 1 : -1); } }}>
              <thead><tr>${SORT_COLS.map((c) => {
                const on = sort?.col === c.col;
                return html`<th key=${c.col} class=${on ? 'sortable sort-active' : 'sortable'} data-sort-col=${c.col} title=${`Sort by ${c.label}`}
                  aria-sort=${on ? (sort.dir === 'asc' ? 'ascending' : 'descending') : 'none'} onClick=${() => setSort((cur) => nextSort(cur, c.col))}>${c.label}<span class="sort-arrow">${on ? (sort.dir === 'asc' ? '▲' : '▼') : '▾'}</span></th>`;
              })}</tr></thead>
              <tbody>${shown.map((r) => html`<tr key=${r.key} data-row=${r.key} class=${r.key === selected ? 'on' : ''} onClick=${() => setSelected(r.key)}>
                <td>${r.name || 'Invalid item'}</td><td>${r.invalid ? html`<span class="fa-danger">invalid</span>` : html`<${KindChip} kind=${r.kind} />`}</td>
                <td>${!r.invalid && html`<${TrustBadge} trust=${r.trust} compact=${true} />`} ${r.by}</td><td class="muted">${r.board.name}</td><td class="muted"><code>${short(r.version)}</code></td><td class="muted">${when(r.updated_at)}</td></tr>`)}</tbody></table>
            ${!shown.length && html`<div class="muted">No items match.</div>`}
          </div>
          <div class="fa-cb-pane">${detail(sel)}</div>
        </div>`}
    ${d?.kind === 'detail' && html`<${Overlay} id="fleet-cb-detail" ariaLabel=${d.row.name} onClose=${() => setDialog(null)}>${detail(d.row)}
      <div class="modal-buttons"><span class="spacer"></span><button type="button" onClick=${() => setDialog(null)}>Close</button></div></${Overlay}>`}
    ${d?.kind === 'skill' && html`<${SkillInstallDialog} row=${d.row} actions=${actions} confirm=${confirm} onClose=${() => setDialog(null)} onDone=${done} />`}
    ${d?.kind === 'config' && html`<${ImportDialog} board=${d.row.board} item=${d.row} version=${d.row.version} by=${d.row.by} actions=${actions} confirm=${confirm} onClose=${() => setDialog(null)} onDone=${done} />`}
    ${d?.kind === 'pick' && html`<${UploadPicker} boards=${posters} onPick=${(b) => setDialog({ kind: 'publish', board: b })} onClose=${() => setDialog(null)} />`}
    ${d?.kind === 'publish' && html`<${PublishDialog} board=${d.board} actions=${actions} confirm=${confirm} onClose=${() => setDialog(null)} onDone=${done} />`}
  </div>`;
}
