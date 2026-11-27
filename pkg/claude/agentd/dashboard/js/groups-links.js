// groups-links.js -- a group's Links: one of the group's regular sections
// (FusedSection, like Agents, Resources and the Task board), so it folds the
// same way and remembers the fold per group. With no choice made it starts
// open for a few links and folded for more (linksSectionAuto), so a busy
// group's links do not push its other sections off the screen.
//
// It also shares their mechanics: the blue ＋ (on the section line while
// folded, heading the section while open, as every section's buttons) adds a
// link (the new-link editor, FROM preset to this group); each row's 🗑
// removes one behind the usual confirmation, and so does dragging the row
// onto the shared trash bin (resource-dnd.js); ✎ changes the mode. The
// table is ResourceTable's look and sort, with its own columns.

import { h } from 'preact';
import { useEffect } from 'preact/hooks';
import htm from 'htm';
import { Button } from './ui-components.js';
import { FusedSection } from './groups-processes.js';
import { ResourceTable } from './groups-resources.js';
import { PencilIcon, TrashIcon } from './row-icons.js';
import { registerResourceRemover } from './resource-dnd.js';
import { deleteLink } from './links-controller.js';
import { applySortState, tableSortState, timestampSortValue } from './sort.js';
import { relTime } from './helpers.js';
import { isWizardActive } from './slop.js';

const html = htm.bind(h);

// LINKS_UNFOLDED_MAX is the most links a section shows open by default.
export const LINKS_UNFOLDED_MAX = 3;

export function linksSectionAuto(count) {
  return count <= LINKS_UNFOLDED_MAX;
}

export const LINK_COLS = Object.freeze([
  { key: 'ctl', label: '' },
  { key: 'name', label: 'Other group', wizardLabel: 'Other party', col: 'other' },
  { key: 'dir', label: 'Direction', col: 'dir' },
  { key: 'age', label: 'Age', col: 'age' },
  { key: 'mode', label: 'Mode', col: 'mode' },
]);

export const LINK_SORT = 'grouplinks';

const LINK_ACCESSORS = Object.freeze({
  other: (r) => r.other, dir: (r) => r.direction, mode: (r) => r.link.mode, age: (r) => timestampSortValue(r.link.created_at),
});

// groupLinkRows are the links touching group: outbound first, then inbound,
// each with the group at its other end.
export function groupLinkRows(links, name) {
  return [
    ...(links || []).filter((link) => link.from === name).map((link) => ({ link, direction: 'out', other: link.to })),
    ...(links || []).filter((link) => link.to === name).map((link) => ({ link, direction: 'in', other: link.from })),
  ];
}

// linksSummary is the section line's digest: "→ 2 · ← 1".
export function linksSummary(rows) {
  const out = rows.filter((row) => row.direction === 'out').length;
  const inbound = rows.length - out;
  return [out && `→ ${out}`, inbound && `← ${inbound}`].filter(Boolean).join(' · ');
}

function LinkRow({ row, group, draggable }) {
  const { link, direction, other } = row;
  const wizard = isWizardActive();
  const fields = { 'data-id': link.id, 'data-group': group, 'data-from': link.from, 'data-to': link.to };
  const removeTitle = wizard ? 'Sever this arcane channel' : 'Remove this link';
  // A drop on the trash bin is the 🗑: the same confirmation, then the delete.
  // A link is drawn at both its ends, so each end's row has its own key: one
  // end folding (unregistering) leaves the other's drag working.
  const dragKey = `${link.id}@${group}`;
  useEffect(() => (draggable
    ? registerResourceRemover(`link:${dragKey}`, () => deleteLink({ id: link.id, from: link.from, to: link.to, scope: group }))
    : undefined), [draggable, dragKey, link.from, link.to, group]);
  const dragAttrs = draggable ? { draggable: 'true', 'data-dnd-resource': JSON.stringify({ kind: 'link', id: dragKey, label: `${link.from} → ${link.to}`, group }) } : {};
  return html`<tr class="gr-trow" data-group-link=${link.id} ...${dragAttrs}>
    <td class="gr-ctl"><span class="agent-ctl gr-acts"><span class="row-actions">
      <${Button} size="sm" icon=${true} class="icon-btn gl-edit" data-act="link-edit" ...${fields} data-mode=${link.mode}
        title=${wizard ? 'Rebind this arcane channel' : "Change this link's mode"} aria-label=${wizard ? 'Rebind this arcane channel' : "Change this link's mode"}><${PencilIcon} /><//>
      <${Button} size="sm" icon=${true} class="icon-btn warn gr-remove" data-act="link-delete" ...${fields}
        title=${removeTitle} aria-label=${removeTitle}><${TrashIcon} /><//>
    </span></span></td>
    <td class="name-cell gr-namecell"><span class="rowname">${other || '(deleted)'}</span></td>
    <td class="gq-dim" title=${direction === 'out' ? `${group} may message ${other}` : `${other} may message ${group}`}>${direction === 'out' ? '→ out' : '← in'}</td>
    <td class="gr-timecell"><span class="last-hook" title=${link.created_at || ''}>${relTime(link.created_at)}</span></td>
    <td><span class="id">${link.mode}</span></td>
  </tr>`;
}

// GroupLinksSection is a real group's Links section.
export function GroupLinksSection({ group, snapshot }) {
  const wizard = isWizardActive();
  const cols = wizard ? LINK_COLS.map((c) => (c.wizardLabel ? { ...c, label: c.wizardLabel } : c)) : LINK_COLS;
  const rows = applySortState(groupLinkRows(snapshot?.links, group.name), LINK_ACCESSORS, tableSortState(LINK_SORT, LINK_COLS));
  const summary = linksSummary(rows);
  const addTitle = wizard ? 'Weave an outbound arcane channel from this party' : `Add an outbound link from ${group.name}`;
  const add = html`<${Button} size="sm" variant="primary" class="spawn-btn gr-add-btn gl-add-btn" data-act="link-new" data-group=${group.name}
    data-from=${group.name} data-label=${group.name} title=${addTitle} aria-label=${addTitle}><span class="gr-add-plus" aria-hidden="true">＋</span><//>`;
  // Dragging a row to the bin needs this node's own group boxes, which the
  // fused fleet view does not have (as Resources' rows).
  const draggable = !group.fleet_node;
  return html`<${FusedSection} group=${group.name} section="links" label=${wizard ? 'Arcane channels' : 'Links'}
      auto=${linksSectionAuto(rows.length)} closedActions=${add} attrs=${{ 'data-group-links': group.name }}
      summary=${html`<span class=${`gf-count${rows.length ? '' : ' zero'}`}>${rows.length}</span>${summary && html`<span class="gf-count gl-summary">${summary}</span>`}`}>
    <div class="group-header-actions gf-title-row">${add}</div>
    ${rows.length
      ? html`<${ResourceTable} cols=${cols} sort=${LINK_SORT} restCol="mode" attrs=${{ class: 'gl-table' }}>${rows.map((row) => html`<${LinkRow} key=${`${row.direction}-${row.link.id}`} row=${row} group=${group.name} draggable=${draggable} />`)}<//>`
      : html`<div class="gf-empty ui-empty">${wizard ? 'No arcane channels are woven to or from this party.' : 'No links involving this group.'}</div>`}
  <//>`;
}
