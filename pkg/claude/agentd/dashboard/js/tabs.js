// tabs.js — the legacy Groups / Links tab renderers.
//
// Builds the listing tables for the Groups and Links
// tabs from snapshot data, each with its text-filter helper.
// Extracted from dashboard.js as part of the Stage 2 module split.

import { $, esc, relTime, themeWords } from './helpers.js';
import {
  sortHead, applySort,
  LINK_COLS, LINK_ACCESSORS,
} from './sort.js';
import { morphInto } from './morph.js';
import { featureState } from './feature-state-registry.js';

// lastSnapshot still lives in dashboard.js — the snapshot-refresh
// cluster is not extracted yet. Importing it back forms a deliberate,
// benign cycle (dashboard.js <-> tabs.js): it is safe because tabs.js
// runs no top-level code that reads it — the render*Tab functions
// touch it only when called, long after both modules finish
// evaluating (it is a read-only live binding here). This import
// re-points to the proper module once the snapshot cluster is
// extracted in a later PR.
import { lastSnapshot } from './dashboard.js';

function renderGroupsTab() {
  if (!lastSnapshot) return;
  // Preact owns the Groups render. This adapter remains for legacy mutations
  // and drag/drop modules that already call renderGroupsTab after updating the
  // shared snapshot; publishing a shallow copy also catches in-place updates.
  featureState('groups')?.publish(lastSnapshot);
}

function fmtRemaining(secs) {
  if (!secs || secs <= 0) return 'expired';
  if (secs < 60) return secs + 's';
  if (secs < 3600) {
    const m = Math.floor(secs / 60);
    const s = secs % 60;
    return s > 0 ? `${m}m${s}s` : `${m}m`;
  }
  const h = Math.floor(secs / 3600);
  const m = Math.floor((secs % 3600) / 60);
  return m > 0 ? `${h}h${m}m` : `${h}h`;
}

// -- Links tab --------------------------------------------------------
// Inter-group communication links surface as a flat read-only table
// in v1. Use `tclaude agent groups link add/rm` to mutate. The list
// shows direction (FROM → TO) and mode so the human can reason about
// who can message whom.
function renderLinks(rows) {
  if (!rows || !rows.length) {
    return '<div class="empty">'
      + '<span class="theme-copy-regular">No inter-group links yet. Create one with the <strong>+ new link</strong> button above.</span>'
      + '<span class="theme-copy-wizard">No arcane channels yet. Weave one with the <strong>+ weave channel</strong> button above.</span>'
      + '</div>';
  }
  return `
    <table>
      ${sortHead('links', LINK_COLS)}
      <tbody>
        ${applySort('links', rows, LINK_ACCESSORS).map(l => `
          <tr data-key="link-${esc(String(l.id))}">
            <td class="id">${l.id}</td>
            <td><span class="rowname">${esc(l.from || '(deleted)')}</span></td>
            <td class="muted">→</td>
            <td><span class="rowname">${esc(l.to || '(deleted)')}</span></td>
            <td><span class="id">${esc(l.mode)}</span></td>
            <td><span class="muted">${esc(relTime(l.created_at) || '')}</span></td>
            <td><div class="row-actions">
              <button data-act="link-edit" data-id="${l.id}" data-from="${esc(l.from)}" data-to="${esc(l.to)}" data-mode="${esc(l.mode)}" title="Change this link's mode">${themeWords('edit', 'rebind')}</button>
              <button class="danger" data-act="link-delete" data-id="${l.id}" data-group="${esc(l.from)}" data-from="${esc(l.from)}" data-to="${esc(l.to)}" title="Remove this link">${themeWords('delete', 'sever')}</button>
            </div></td>
          </tr>
        `).join('')}
      </tbody>
    </table>
  `;
}
function filterLinks(rows, q) {
  if (!q) return rows;
  const needle = q.toLowerCase();
  return rows.filter(l =>
    ((l.from || '').toLowerCase().includes(needle)) ||
    ((l.to || '').toLowerCase().includes(needle)) ||
    ((l.mode || '').toLowerCase().includes(needle))
  );
}
function renderLinksTab() {
  if (!lastSnapshot) return;
  const q = $('#filter-links').value;
  const rows = lastSnapshot.links || [];
  const filtered = filterLinks(rows, q);
  morphInto($('#links-list'), renderLinks(filtered));
  $('#filter-links-count').innerHTML = q
    ? `${filtered.length} / ${rows.length}`
    : themeWords(
      `${rows.length} link${rows.length === 1 ? '' : 's'}`,
      `${rows.length} channel${rows.length === 1 ? '' : 's'}`,
    );
}

export {
  renderGroupsTab, renderLinksTab, fmtRemaining,
};
