// process-collab-menu.js -- the Processes half of the Collab strip's ＋ menu
// (tcl-tnbtwp): every process template, by the groups it is pinned to, with a
// filter, and a new one. Picking one opens its editor as a Collab tab
// (process-editor-place.js openProcessTab).

import { h } from 'preact';
import { useCallback, useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { Button, Input } from './ui-components.js';
import { idempotentRequestHeaders } from './request-idempotency.js';
import { randomUUID } from './insecure-origin.js';

const html = htm.bind(h);

// groupProcessTemplates sorts a template listing into sections that match
// query: one per group a template is pinned to, by group name, and the
// templates pinned to none last. A template pinned to two groups is in both.
export function groupProcessTemplates(templates, query = '') {
  const wanted = String(query || '').trim().toLowerCase();
  const sections = new Map();
  const add = (name, template) => {
    if (!sections.has(name)) sections.set(name, []);
    sections.get(name).push(template);
  };
  for (const template of templates || []) {
    const pinned = Array.isArray(template.pinnedGroups) ? template.pinnedGroups.filter(Boolean) : [];
    const text = `${template.name || ''} ${template.id} ${template.description || ''} ${pinned.join(' ')}`.toLowerCase();
    if (wanted && !text.includes(wanted)) continue;
    if (!pinned.length) add('', template);
    for (const group of pinned) add(group, template);
  }
  const byName = (a, b) => (a.name || a.id).localeCompare(b.name || b.id);
  return [...sections]
    .map(([name, rows]) => ({ name, templates: rows.sort(byName) }))
    .sort((a, b) => (!a.name) - (!b.name) || a.name.localeCompare(b.name));
}

// createProcessTemplate stores a blank template named name, and answers its id.
export async function createProcessTemplate(name, { fetchImpl = globalThis.fetch, attempt = randomUUID() } = {}) {
  const { blankEditView } = await import('./process-edit-model.js');
  const scaffold = blankEditView(name);
  const template = { ...scaffold.template };
  delete template.id;
  const path = '/v1/process/templates';
  const body = JSON.stringify({ template, edges: scaffold.edges, layout: scaffold.layout });
  const response = await fetchImpl(path, {
    method: 'POST', headers: idempotentRequestHeaders('POST', path, body, attempt), credentials: 'same-origin', body,
  });
  const result = await response.json().catch(() => null);
  if (!response.ok) throw new Error(result?.message || result?.error || `${response.status} ${response.statusText}`);
  const id = String(result?.id || '').trim();
  if (!id) throw new Error('template creation returned no id');
  return id;
}

// ProcessMenuList offers every template, and a new one; one box does both, as
// the drawing boards half does.
export function ProcessMenuList({ fetchImpl, notify, onOpen, wizard = false }) {
  const [templates, setTemplates] = useState(null);
  const [text, setText] = useState('');
  const [error, setError] = useState('');
  const [creating, setCreating] = useState(false);
  const load = useCallback(async () => {
    try {
      const response = await fetchImpl('/v1/process/templates', { credentials: 'same-origin', cache: 'no-store' });
      const body = await response.json().catch(() => null);
      if (!response.ok) throw new Error(body?.message || body?.error || `${response.status} ${response.statusText}`);
      setTemplates(Array.isArray(body?.templates) ? body.templates : []);
      setError('');
    } catch (err) {
      setError(err?.message || String(err));
    }
  }, [fetchImpl]);
  useEffect(() => { void load(); }, [load]);
  const clean = text.trim();
  const same = (templates || []).find((template) => (template.name || template.id).toLowerCase() === clean.toLowerCase());
  const noun = wizard ? 'rite' : 'process';
  async function submit(event) {
    event.preventDefault();
    if (!clean || creating) return;
    // The box filters too, so naming a template that exists means that one.
    if (same) {
      onOpen(same.id, same.name || same.id);
      return;
    }
    setCreating(true);
    try {
      const id = await createProcessTemplate(clean, { fetchImpl });
      setText('');
      onOpen(id, clean);
    } catch (err) {
      notify(`Could not create the ${noun}: ${err?.message || err}`, true);
    } finally {
      setCreating(false);
    }
  }
  const sections = groupProcessTemplates(templates || [], clean);
  return html`<div class="drawing-board-list compact process-menu-list">
    <form class="drawing-board-new" onSubmit=${submit}>
      <${Input} size="sm" placeholder=${`Search, or name a new ${noun}`} aria-label=${`Search ${noun}es, or name a new one`} maxLength="120"
        value=${text} onInput=${(event) => setText(event.currentTarget.value)} />
      <${Button} size="sm" variant="primary" type="submit" disabled=${!clean || creating}>${same ? 'Open' : creating ? 'Creating…' : `＋ New ${noun}`}<//>
    </form>
    ${error && html`<p class="drawing-board-error ui-error" role="alert">Processes are unavailable: ${error}</p>`}
    ${templates === null && !error && html`<p class="drawing-board-muted">Loading ${noun}es…</p>`}
    ${templates && !templates.length && html`<p class="drawing-board-muted">No ${noun}es yet. A process is a reusable graph of steps that agents and people work through.</p>`}
    ${templates?.length > 0 && !sections.length && html`<p class="drawing-board-muted">No ${noun} matches. ＋ New ${noun} makes one with this name.</p>`}
    <div class="drawing-board-groups">
      ${sections.map(({ name, templates: rows }) => html`<div key=${name} class="drawing-board-section" role="group" aria-label=${name ? `Pinned to ${name}` : 'Not pinned to a group'}>
        ${(name || sections.length > 1) && html`<h3 class="drawing-board-section-name"><span>${name || 'Not pinned'}</span><span class="drawing-board-section-count">${rows.length}</span></h3>`}
        <ul class="drawing-board-rows">
          ${rows.map((template) => {
            const label = template.name || template.id;
            return html`<li key=${template.id} class="drawing-board-row" data-process-menu-row=${template.id}>
              <button data-ui-exempt type="button" class="drawing-board-row-open" title=${template.description || template.id} onClick=${() => onOpen(template.id, label)}>
                <span class="drawing-board-row-title">${label}</span>
                <span class="drawing-board-row-age">v${template.versionCount || 0}</span>
              </button>
            </li>`;
          })}
        </ul>
      </div>`)}
    </div>
  </div>`;
}
