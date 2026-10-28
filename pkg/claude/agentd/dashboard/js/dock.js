// dock.js — the retractable right-side PALETTE DOCK (JOH-374).
//
// A vertical dock pinned to the right edge of the dashboard listing what
// you can drop onto a group: your spawn PROFILES, your group TEMPLATES
// (summoning circles) and your ROLES (classes). The panel SHELL + card
// rendering live here; the DRAG behaviour (dragstart/dragover/drop) lives in
// dock-dnd.js. All three kinds are drag sources now: profile + role cards
// drop onto a group to open the spawn dialog prefilled (JOH-375 2/4);
// template cards drop onto a group to open the unified summon dialog with a
// drop-mode chooser (reinforce the group / new group in its image), or onto
// empty space for a plain "new party from circle" open (JOH-377 4/4).
//
// NB the name: js/palette.js is already taken (the Ctrl/Cmd-K command
// palette), so this module + its CSS/ids live under the `dock` namespace
// (#agent-dock, .dock-*), NOT .palette-*.
//
// Design intent (operator, 2026-07-04): this dock is a FOUNDATION, not a
// one-off — future editors and agent-work-graph attach points are meant to
// grow from it. So the three sections are instances of ONE data-driven
// idiom (a section = title + category icon + item list + card renderer; a card
// = key, name, chips, actions) rather than three hand-rolled blocks — a fourth
// item kind slots in by adding one SECTIONS entry.
//
// Data rides the 2s poll: the daemon carries the profile / template / role
// registries on the snapshot (dashboard.go), so renderDock() publishes
// lastSnapshot into the keyed Preact island — a manager edit shows up on the
// next tick, and selections/scroll survive the render. The shell (#agent-dock)
// + the edge toggle are STATIC markup in dashboard.html; only #dock-body's
// inner sections are Preact-owned.
//
// Collapse/expand is persisted server-side via dashPrefs (NOT localStorage,
// which the random per-start port would reset) under DOCK_OPEN_KEY. The
// open state is a body class so CSS can reflow <main> to reclaim the space
// when collapsed rather than overlaying dead area.

import { $ } from './helpers.js';
import { wizWord } from './slop.js';
import { dashPrefs } from './prefs.js';
import { lastSnapshot } from './dashboard.js';
import { featureState } from './feature-state-registry.js';
import { syncFullBleedBars } from './hscroll.js';
import { syncChromeTopInset } from './chrome-inset.js';
// The compact one-line summaries live in the DATA modules (profiles.js /
// roles.js); the editor/manager openers live in the MODAL modules. Importing
// each from the module that actually exports it — a bad named import would
// abort the whole ES-module graph at link time (node --check can't catch that,
// it's single-file only).
import { refresh, confirmModal, toast } from './refresh.js';
import { deleteSavedPrompt } from './saved-prompts-actions.js';
import { openSavedPromptsManager as openSavedPrompts } from './modal-saved-prompts.js';
import { profileSummary, profileDetailChips, createProfile, profilesVia } from './profiles.js';
import { openProfileEditor, openProfilesManageModal, removeProfile } from './modal-profiles.js';
import { roleSummary, createRole, rolesVia } from './roles.js';
import { openRoleEditor, openRolesManageModal, removeRole } from './modal-roles.js';
import { templateReadbackBadgeModels } from './template-readback.js';
import { openTemplatesManageModal, openTemplateEditor, openDuplicateModal, deleteTemplate } from './modal-templates.js';
// The generic "clone under a new name" dialog (profiles + roles). Templates
// reuse their own richer openDuplicateModal above — see the SECTIONS clone hooks.
import { openPresetCloneDialog } from './action-dialog-controller.js';
import { managementController } from './management-controller.js';
import { openUsageRulesManager } from './modal-usage-rules.js';
import { usageRulesApi } from './usage-rules-actions.js';
import { fmtAmount, periodLabel, ruleSummary } from './usage-rules-model.js';
import { loadSandboxProfiles, sandboxProfileForWire, sandboxProfileSummary } from './sandbox-profiles-data.js';
import { downloadBlob } from './paf-model.js';
import { announceNodeChanged, selfNodeID } from './node-api.js';
import { nodeHref } from './fleet-model.js';
import { withFused } from './fleet-scope.js';
import {
  BUNDLE_SECTIONS, LIBRARY_EVENT, detectImport, dropPin, exportBundle, renameItem, renamePin,
} from './dock-library.js';

// dockNotify is the island's toast, for the card menu's own feedback.
export const dockNotify = (text, error = false) => toast(text, error);

// libraryChanged asks the dock's self-loading sections (sandbox profiles,
// processes) to reload after a rename or an import changed what they list.
function libraryChanged() {
  document.dispatchEvent(new CustomEvent(LIBRARY_EVENT));
}

// The kinds' singular names, for dialog titles and toasts.
const KIND_NAMES = Object.freeze({
  profiles: () => wizWord('spawn profile', 'loadout'),
  templates: () => wizWord('group template', 'summoning circle'),
  roles: () => wizWord('role', 'class'),
  saved_prompts: () => wizWord('saved prompt', 'incantation'),
  sandbox_profiles: () => wizWord('sandbox profile', 'ward'),
  processes: () => wizWord('process', 'rite'),
});

// openRename asks for the new name, renames through the daemon, and carries
// a pin along. A refusal (name taken) stays in the dialog.
export function openRename(section, name, { label = name, node = null } = {}) {
  managementController().openLibraryName({
    title: `Rename ${KIND_NAMES[section]()}: ${label}${node ? ` on ${node.name}` : ''}`,
    blurb: section === 'processes'
      ? 'Saved as a new version; the history and the groups it is pinned to stay.'
      : 'Everything that uses it (profiles, templates, groups, includes) follows the new name.',
    initial: label,
    okLabel: 'Rename',
    submit: async (to) => {
      if (to === label) return;
      if (node) {
        await renameItem({ section, name, to, fetchImpl: node.fetch });
        toast(`renamed ${label} → ${to} on ${node.name}`);
        libraryChanged();
        announceNodeChanged(node.id);
        return;
      }
      await renameItem({ section, name, to });
      if (section !== 'processes') renamePin(dashPrefs, section, name, to);
      toast(`renamed ${label} → ${to}`);
      libraryChanged();
      await refresh();
    },
  });
}

function downloadJSON(fileName, value) {
  downloadBlob(new Blob([`${JSON.stringify(value, null, 2)}\n`], { type: 'application/json' }), fileName);
}

// exportItems downloads a config bundle of one item (name) or the section.
export async function exportItems(section, name = '', label = name) {
  try {
    const done = await exportBundle({ section, name, confirm: confirmModal, download: downloadJSON });
    if (done) toast(name ? `exported ${label}` : `exported every ${KIND_NAMES[section]()}`);
  } catch (error) {
    toast(`export failed: ${error.message || String(error)}`, true);
  }
}

// importLibraryFile reads a dropped or picked file: a config bundle opens
// the bundle review; a kind's own export opens that kind's importer.
export async function importLibraryFile(file) {
  const found = detectImport(await file.text());
  const controller = managementController();
  if (found.kind === 'bundle') {
    controller.openLibraryBundleImport({
      bundle: found.value, fileName: file.name, notify: toast,
      onImported: () => { libraryChanged(); void refresh(); },
    });
    return;
  }
  const dialog = { profiles: 'profile-import', sandbox_profiles: 'sandbox-import', templates: 'template-import' }[found.kind];
  if (dialog) {
    controller.openLibraryImport(dialog, found.raw);
    return;
  }
  toast(`${file.name} is not a tclaude export (${found.reason})`, true);
}

// COPY_DROPPED are an item's fields that belong to its node: the store's
// identity and timestamps.
const COPY_DROPPED = ['id', 'created_at', 'updated_at'];

async function failure(response) {
  let body = null;
  try { body = await response.json(); } catch { body = null; }
  const error = new Error(body?.message || body?.error || `HTTP ${response.status}`);
  error.status = response.status;
  error.body = body;
  return error;
}

// copyDirect creates the item on `to` through its kind's create route.
// A peer's routes carry requests up to 32 KiB (fedPeerViewBodyLimit); this
// node's own carry more.
const PEER_BODY_LIMIT = 32 << 10;

async function copyDirect(section, item, to, self) {
  let body = Object.fromEntries(Object.entries(item).filter(([key]) => !COPY_DROPPED.includes(key)));
  if (section.key === 'sandbox_profiles') body = sandboxProfileForWire(body);
  const raw = JSON.stringify(body);
  const size = new TextEncoder().encode(raw).length;
  if (to.id !== self.id && size > PEER_BODY_LIMIT) {
    throw new Error(`it is ${Math.ceil(size / 1024)} KiB, and a request to another node carries at most 32 KiB. Offer it from the node holding it instead.`);
  }
  const response = await to.fetch(section.listPath, {
    method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' }, body: raw,
  });
  if (!response.ok) throw await failure(response);
}

// OFFER_CONFIG is this node's route that offers its config to a peer.
const OFFER_CONFIG = '/api/federation/offer-config';

// offerItem sends this node's item to `to` as a one-item config offer.
// Suspected credentials stop it until the operator confirms each flag.
async function offerItem(section, name, to, self) {
  const only = [`${BUNDLE_SECTIONS[section.key]}/${name}`];
  const send = (allow) => self.fetch(OFFER_CONFIG, {
    method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ peer: to.id, only, allow_flagged: allow }),
  });
  let response = await send(false);
  if (response.status === 422) {
    const flags = (await failure(response)).body?.flags || [];
    const ok = await confirmModal({
      title: 'Send anyway?',
      body: `These fields look like credentials:\n${flags.map((flag) => `• ${flag.item} ${flag.field}: ${flag.hint}`).join('\n')}\n\nThey would leave this node in the offer.`,
      preformatted: true,
      okLabel: 'Send anyway',
    });
    if (!ok) return false;
    response = await send(true);
  }
  if (!response.ok) throw await failure(response);
  return true;
}

// dockNodeOps act on another node's copy of an item in the fused view
// (dock-nodes.js). node is { id, name, fetch }: fetch reaches that node.
// Its editors stay on its own dashboard, which open() shows.
export const dockNodeOps = Object.freeze({
  rename: (section, name, node) => openRename(section.key, name, { node }),
  async remove(section, name, node) {
    const ok = await confirmModal({
      title: `Delete ${KIND_NAMES[section.key]()} on ${node.name}?`,
      body: `Delete "${name}" on ${node.name}? The other nodes keep their copies.`,
      meta: name,
      okLabel: 'Delete',
    });
    if (!ok) return;
    try {
      const response = await node.fetch(section.itemPath(name), { method: 'DELETE', credentials: 'same-origin' });
      if (!response.ok) {
        let body = null;
        try { body = await response.json(); } catch { body = null; }
        throw new Error(body?.message || body?.error || `HTTP ${response.status}`);
      }
      toast(`deleted ${name} on ${node.name}`);
      libraryChanged();
      announceNodeChanged(node.id);
    } catch (error) {
      toast(`delete failed on ${node.name}: ${error.message || String(error)}`, true);
    }
  },
  // copy creates the item on node `to`: through its own create route where
  // it trusts this node unrestricted (direct), else as a one-item config
  // offer from this node that its operator reviews (offer, through this
  // node's own fetch, `self`).
  async copy(section, item, { to, direct, self }) {
    const name = section.name(item);
    const kind = KIND_NAMES[section.key]();
    const ok = await confirmModal(direct ? {
      title: `Copy ${kind} to ${to.name}?`,
      body: `Create "${name}" on ${to.name} as this copy. Anything it names (profiles, prompts, includes) must exist there too.`,
      meta: name,
      okLabel: 'Copy',
    } : {
      title: `Offer ${kind} to ${to.name}?`,
      body: `Send this node's copy of "${name}" to ${to.name}'s operator as a config offer. They preview it and decide whether to import it; nothing changes there until they do. Free text is sent as written.`,
      meta: name,
      okLabel: 'Send offer',
    });
    if (!ok) return false;
    try {
      if (direct) await copyDirect(section, item, to, self);
      else if (!await offerItem(section, name, to, self)) return false;
      toast(direct ? `copied ${name} to ${to.name}` : `offered ${name} to ${to.name}; its operator decides`);
      libraryChanged();
      announceNodeChanged(to.id);
      return true;
    } catch (error) {
      toast(`copy to ${to.name} failed: ${error.message || String(error)}`, true);
      return false;
    }
  },
  open(node) {
    const self = selfNodeID();
    globalThis.location.assign(nodeHref(node.id === self ? '' : node.id, { pathname: '/groups', search: withFused(globalThis.location.search, null) }));
  },
});

// setDefault goes through the 🧠/🛡 toolbar pickers' own commit, loaded on
// first use like the pickers themselves.
let defaultActions = null;
async function setDefault(kind, name) {
  try {
    if (!defaultActions) {
      const { createToolbarProfilePickerActions } = await import('./toolbar-profile-picker-actions.js');
      defaultActions = createToolbarProfilePickerActions({ notify: toast, refresh });
    }
    await defaultActions.commit(kind, name);
  } catch (error) {
    toast(error.message || String(error), true);
  }
}

async function removeSandboxProfile(p) {
  if (await managementController().removeSandboxProfile(p.name)) {
    dropPin(dashPrefs, 'sandbox_profiles', p.name);
    libraryChanged();
    await refresh();
  }
}

// libraryOps are the hooks every snapshot section shares: rename, export
// one or all, import a file, copy the name.
function libraryOps(key) {
  return {
    onRenameItem: (item) => openRename(key, item.name),
    onExportItem: (item) => exportItems(key, item.name),
    onExportAll: () => exportItems(key),
    onImportFile: (file) => importLibraryFile(file),
  };
}

// Usage profiles live in the usage rules store; the global default is their
// global-layer assignment, which the ★ sets to that one profile.
// toggleUsageDefault adds a profile to the global default layer or takes it
// out, leaving any other global profiles in place.
async function toggleUsageDefault(name, current, isDefault) {
  const next = isDefault ? current.filter((n) => n !== name) : [...current, name];
  try {
    await usageRulesApi.assign('global', '', next);
    toast(isDefault ? `${name} removed from the global default usage profiles` : `${name} added to the global default usage profiles`);
    await refresh();
  } catch (error) {
    toast(error.message || String(error), true);
  }
}

// usageLimitItems are a node's enabled global and group limits, then its
// fleet limits, as the snapshot reads them (usage_limits, fleet_limits).
// Limit names need not be unique, and the dock keys its cards by name, so
// a repeated one takes its id.
function usageLimitItems(snap) {
  const usage = snap?.usage || {};
  const items = [
    ...(usage.usage_limits || []),
    ...(usage.fleet_limits || []).map((l) => ({ ...l, scope: 'fleet', fleet_id: Number(String(l.rule_key || '').replace(/^fleet:/, '')) || 0 })),
  ].map((l) => ({ ...l, label: l.scope === 'fleet' ? `${l.name} · fleet` : l.scope === 'group' ? `${l.name} · ${l.group}` : l.name }));
  const seen = new Map();
  for (const l of items) seen.set(l.label, (seen.get(l.label) || 0) + 1);
  return items.map((l) => (seen.get(l.label) > 1 ? { ...l, label: `${l.label} #${l.scope === 'fleet' ? l.fleet_id : l.limit_id}` } : l));
}

function usageLimitChips(l) {
  const where = l.scope === 'fleet' ? `fleet: ${l.target_label}` : l.scope === 'group' ? `group ${l.group}` : 'global';
  const tone = l.reached && l.reached !== 'notify' ? ' over' : l.reached || l.pct >= 80 ? ' warn' : '';
  const reading = `${fmtAmount(l.metric, l.value)} / ${fmtAmount(l.metric, l.budget)} ${l.metric === 'window_pct' ? l.window || '' : periodLabel(l.period)}`;
  return [
    { text: where, more: false },
    { text: `${Math.round(l.pct)}%`, more: false, className: `dock-chip usage-limit-pct${tone}`,
      title: `${reading}${l.reached ? ` · ${l.reached.replace('_', ' ')} step reached` : ''}${l.scope === 'fleet' ? ` · ${l.counted} node(s) counted` : ''}` },
    ...(l.dry_run ? [{ text: 'dry run', more: false }] : []),
  ];
}

// usageNode aims the usage manager at a fused view's peer copy.
const usageNode = (node) => (node ? { node: node.id, nodeLabel: node.name } : {});

// openUsageLimit opens a limit's editor (clone: a copy of it); a fused
// view's peer copy opens on that node, where fleet limits are not managed.
function openUsageLimit(l, node = null, clone = false) {
  if (node && l.scope === 'fleet') return openUsageRulesManager({ node: node.id, nodeLabel: node.name, tab: 'limits' });
  if (node) return openUsageRulesManager({ node: node.id, nodeLabel: node.name, tab: 'limits', limit: l.limit_id, clone });
  if (l.scope === 'fleet') return openUsageRulesManager({ tab: 'fleet', fleetLimit: l.fleet_id, clone });
  return openUsageRulesManager({ tab: 'limits', limit: l.limit_id, clone });
}

async function removeUsageLimit(l) {
  const fleet = l.scope === 'fleet';
  const ok = await confirmModal({
    title: fleet ? 'Delete fleet limit?' : 'Delete usage limit?',
    body: fleet
      ? `Delete the fleet limit "${l.name}"? Any spawn block or stop it holds on other nodes is released.`
      : `Delete the ${l.scope} limit "${l.name}"? Its alert history goes with it.`,
    meta: l.name,
    okLabel: 'Delete limit',
  });
  if (!ok) return;
  try {
    await (fleet ? usageRulesApi.deleteFleetLimit(l.fleet_id) : usageRulesApi.deleteLimit(l.limit_id));
    toast(`usage limit deleted: ${l.name}`);
    await refresh();
  } catch (error) {
    toast(error.message || String(error), true);
  }
}

async function removeUsageProfile(p) {
  const ok = await confirmModal({
    title: 'Delete usage profile?',
    body: `Delete the usage profile "${p.name}"? Every assignment of it goes too, so the agents using it fall back to their next layer.`,
    meta: p.name,
    okLabel: 'Delete profile',
  });
  if (!ok) return;
  try {
    await usageRulesApi.deleteProfile(p.id);
    toast(`usage profile deleted: ${p.name}`);
    await refresh();
  } catch (error) {
    toast(error.message || String(error), true);
  }
}

async function removeSavedPrompt(name) {
  const ok = await confirmModal({
    title: 'Delete saved prompt?',
    body: `Delete the saved prompt "${name}"? This is refused while a spawn profile or group still uses it.`,
    meta: name,
    okLabel: 'Delete prompt',
  });
  if (!ok) return;
  try {
    await deleteSavedPrompt(name);
    toast(`saved prompt deleted: ${name}`);
    await refresh();
  } catch (error) {
    toast(error.message || String(error), true);
  }
}

// promptPreview is a saved prompt's first non-blank line, clipped for a chip.
function promptPreview(body) {
  const line = String(body || '').split('\n').map((part) => part.trim()).find(Boolean) || '';
  return line.length > 40 ? `${line.slice(0, 39)}…` : line;
}

// The persisted open/collapsed flag. dash-namespaced like every other
// server-backed dashboard pref. Default OPEN (see isDockOpen): the dock is a
// new surface and discoverability beats density here.
const DOCK_OPEN_KEY = 'tclaude.dash.dock.open';

// summaryChips turns a profileSummary/roleSummary "·"-joined string into a
// few compact chip spans — the profile/role twin of the template's
// roster-shape badges. Capped so a rich profile doesn't blow out the narrow
// dock; profile cards expose the full list on hover/focus.
function summaryChips(summary, max = 4) {
  const parts = String(summary || '')
    .split('·')
    .map(s => s.trim())
    .filter(Boolean);
  if (!parts.length) return [];
  const shown = parts.slice(0, max);
  const extra = parts.length - shown.length;
  const chips = shown.map((text) => ({ text, more: false }));
  if (extra > 0) chips.push({ text: `+${extra}`, more: true });
  return chips;
}

// Another node's item (dock-island.js passes {id, name, fetch}) is edited
// and cloned in place, through that node's catalogs: catalogOn's actions,
// or for a name-only clone its create bound to the node's fetch.
const catalogOn = (node) => managementController().catalogOn(node.id);
const createOn = (node, create) => async (body) => {
  const result = await create(body);
  announceNodeChanged(node.id);
  return result;
};

// SECTIONS — the whole dock is three instances of this one shape. To add a
// fourth item kind (a future editor / work-graph node), add an entry here;
// the renderer + the delegated click handler are already generic.
//
//   key         stable section id (data-key + the snapshot field name)
//   icon()      theme-aware category glyph shown once in the section heading
//   title()     the section heading (both vocab modes via wizWord)
//   empty()     the quiet "(none yet)" line when the list is empty
//   items(snap) the item array off the live snapshot
//   name(item)  the card's display name
//   chips(item) the card's chip HTML (already escaped)
//   drag        true → cards are drag SOURCES (draggable, wired by dock-dnd.js)
//   onManageItem(item, node)  jump to that item's editor / manager overlay
//                       (⚙ → Edit); node, another node's item: edit it there
//   onCloneItem(item, node)   open the "clone under a new name" dialog
//                       (⚙ → Clone); node: the copy is made on that node
//   onDeleteItem(item)  confirm + delete the item (⚙ → Delete), then refresh
//   onManageAll()       jump to the whole-kind manager overlay
//   itemPath(name)      the item's own route, which deletes it on another
//                       node of the fused view (dock-nodes.js)
//   listPath            the kind's route, which creates a copied item on
//                       another node (dockNodeOps.copy)
//   afterProcesses      render after the Processes section, which the dock
//                       draws itself between the snapshot sections
//
// `drag` gates the draggable attribute (dock-dnd.js's dragstart still keys off
// data-dock-kind): all three kinds drop onto a group — profiles + roles open
// the spawn dialog prefilled (JOH-375 2/4), templates open the unified summon
// dialog with a drop-mode chooser (JOH-377 4/4).

export const dockSections = Object.freeze([
  {
    key: 'profiles',
    // A person silhouette reads as an agent/profile, while the old cog was
    // indistinguishable from the real per-card actions cog. Category icons
    // live in headings only; repeating them on every row adds no information.
    icon: () => wizWord('👤', '🐾'),
    // Full section name (JOH-390 item 5): the operator wants the profiles
    // heading spelled out — "Agent profiles" / "Loadouts" — rather
    // than the bare "Profiles" / "Loadouts". Roles keeps its short heading.
    title: () => wizWord('Agent profiles', 'Loadouts'),
    empty: () => wizWord('no profiles yet', 'no loadouts yet'),
    items: (snap) => (snap && snap.profiles) || [],
    name: (p) => p.name,
    chips: (p) => [
      ...(p.aliases || []).slice(0, 2).map((alias) => ({ text: `aka ${alias}`, more: false })),
      ...summaryChips(profileSummary(p), Math.max(1, 4 - Math.min((p.aliases || []).length, 2))),
      ...((p.aliases || []).length > 2 ? [{ text: `+${p.aliases.length - 2} aliases`, more: true }] : []),
    ],
    fullChips: (p) => [
      ...(p.aliases || []).map((alias) => ({ text: `aka ${alias}`, more: false })),
      ...profileDetailChips(p).map((text) => ({ text, more: false })),
    ],
    drag: true,
    onManageItem: (p, node) => (node ? catalogOn(node).openProfileEditor(p) : openProfileEditor(p)),
    // Clone → the generic name dialog; the copy is the source profile re-POSTed
    // under the new name through the Preact-owned action-dialog host.
    onCloneItem: (p, node) => openPresetCloneDialog({ kind: 'profile', kindWizard: 'loadout', source: p,
      create: node ? createOn(node, profilesVia(node.fetch).createProfile) : createProfile }),
    // Delete → the manager's confirm + delete + toast (removeProfile), then a
    // dashboard refresh so the dock card leaves at once (removeProfile only
    // repaints the closed manager overlay).
    onDeleteItem: (p) => removeProfile(p.name).then((removed) => {
      if (removed) dropPin(dashPrefs, 'profiles', p.name);
      return refresh();
    }),
    onManageAll: () => openProfilesManageModal(),
    onNew: () => openProfileEditor(),
    searchWords: ['agent profiles', 'loadouts', 'spawn profiles'],
    isDefault: (p, snap) => !!snap?.spawn_profile_default && snap.spawn_profile_default === p.name,
    onToggleDefault: (p, isDefault) => setDefault('profile', isDefault ? '' : p.name),
    itemPath: (name) => `/api/spawn-profiles/${encodeURIComponent(name)}`,
    listPath: '/api/spawn-profiles',
    ...libraryOps('profiles'),
  },
  {
    key: 'roles',
    icon: () => wizWord('🎭', '🎭'),
    title: () => wizWord('Roles', 'Classes'),
    empty: () => wizWord('no roles yet', 'no classes yet'),
    items: (snap) => (snap && snap.roles) || [],
    name: (rl) => rl.name,
    chips: (rl) => summaryChips(roleSummary(rl)),
    drag: true,
    onManageItem: (rl, node) => (node ? catalogOn(node).openRoleEditor(rl) : openRoleEditor(rl)),
    // Clone → the generic name dialog, cloning via createRole (see profiles).
    onCloneItem: (rl, node) => openPresetCloneDialog({ kind: 'role', kindWizard: 'class', source: rl,
      create: node ? createOn(node, rolesVia(node.fetch).createRole) : createRole }),
    // Delete → removeRole's confirm + delete + toast (incl. the 409 role_in_use
    // surfacing), then a dashboard refresh so the dock card leaves at once.
    onDeleteItem: (rl) => removeRole(rl.name).then(() => refresh()),
    onManageAll: () => openRolesManageModal(),
    onNew: () => openRoleEditor(),
    searchWords: ['roles', 'classes'],
    itemPath: (name) => `/api/roles/${encodeURIComponent(name)}`,
    listPath: '/api/roles',
    ...libraryOps('roles'),
  },
  {
    key: 'saved_prompts',
    icon: () => wizWord('📝', '📜'),
    title: () => wizWord('Saved prompts', 'Incantations'),
    empty: () => wizWord('no saved prompts yet', 'no incantations yet'),
    items: (snap) => (snap && snap.saved_prompts) || [],
    name: (p) => p.name,
    chips: (p) => (promptPreview(p.body) ? [{ text: promptPreview(p.body), more: false, title: p.body }] : []),
    // Not a drag source: a prompt is added through the spawn dialog, a
    // profile or the group settings rather than dropped onto a group.
    drag: false,
    onManageItem: (p, node) => openSavedPrompts({ initial: p.name, node: node?.id || '' }),
    onCloneItem: (p, node) => openSavedPrompts({ initial: p.name, clone: true, node: node?.id || '' }),
    onDeleteItem: (p) => removeSavedPrompt(p.name),
    onManageAll: () => openSavedPrompts(),
    onNew: () => openSavedPrompts(),
    searchWords: ['saved prompts', 'incantations'],
    itemPath: (name) => `/api/saved-prompts/${encodeURIComponent(name)}`,
    listPath: '/api/saved-prompts',
    ...libraryOps('saved_prompts'),
  },
  {
    key: 'templates',
    icon: () => wizWord('🧩', '🔮'),
    // Spelled out like the profiles heading (operator follow-up to JOH-390
    // item 5): these are the GROUP templates, and "Summoning circles" is
    // already the full arcane name.
    title: () => wizWord('Group templates', 'Summoning circles'),
    empty: () => wizWord('no templates yet', 'no circles yet'),
    items: (snap) => (snap && snap.templates) || [],
    name: (t) => t.name,
    chips: (t) => templateReadbackBadgeModels(t),
    drag: true,
    // The per-card ⚙ deep-links into THIS template's editor (JOH-390 item 6),
    // matching the profiles/roles cards — it used to fall back to the whole-kind
    // manager, which ignored the item.
    onManageItem: (t, node) => (node ? catalogOn(node).openTemplateEditor(t) : openTemplateEditor(t)),
    // Clone → templates reuse their OWN richer duplicate dialog (a template
    // carries a whole roster, so its bespoke blurb is worth keeping) rather than
    // the generic action-dialog name shell profiles/roles use. Both are name dialogs.
    onCloneItem: (t, node) => (node ? catalogOn(node).openTemplateDuplicate(t.name) : openDuplicateModal(t.name)),
    // Delete → deleteTemplate already runs the confirm + DELETE + refresh,
    // so the dock calls it directly (no extra refresh needed here).
    onDeleteItem: (t) => deleteTemplate(t.name).then((removed) => {
      if (removed) dropPin(dashPrefs, 'templates', t.name);
    }),
    onManageAll: () => openTemplatesManageModal(),
    onNew: () => openTemplateEditor(null),
    searchWords: ['group templates', 'summoning circles'],
    extraActions: [{ act: 'deploy-item', label: () => wizWord('Deploy…', 'Summon…'), run: (t) => managementController().openTemplateDeploy(t.name) }],
    itemPath: (name) => `/api/templates/${encodeURIComponent(name)}`,
    listPath: '/api/templates',
    ...libraryOps('templates'),
  },
  {
    key: 'sandbox_profiles',
    // Sandbox profiles close the panel, after the Processes section.
    afterProcesses: true,
    icon: () => wizWord('🛡', '🛡'),
    title: () => wizWord('Sandbox profiles', 'Wards'),
    empty: () => wizWord('no sandbox profiles yet', 'no wards yet'),
    // The snapshot carries only the names (environment values stay behind
    // the management API), so the section loads the profiles itself, again
    // whenever the snapshot's revision says one changed.
    load: {
      fetch: (fetchImpl) => loadSandboxProfiles(fetchImpl),
      version: (snap) => snap?.sandbox_profiles_rev ?? (snap?.sandbox_profiles || []).join('\n'),
    },
    name: (p) => p.name,
    chips: (p) => summaryChips(sandboxProfileSummary(p)),
    fullChips: (p) => summaryChips(sandboxProfileSummary(p), 99).map((chip) => ({ ...chip, more: false })),
    // A sandbox profile is assigned through the 🛡 pickers, not dropped.
    drag: false,
    onManageItem: (p, node) => (node ? catalogOn(node).openSandboxEditor(p) : managementController().openSandboxProfileEditor(p)),
    // A clone's suggested name dodges that node's own profiles, so its list
    // loads first.
    onCloneItem: (p, node) => (node
      ? (async () => { const on = catalogOn(node); await on.load('sandbox'); on.openSandboxClone(p); })()
      : managementController().openSandboxProfileClone(p)),
    onDeleteItem: (p) => removeSandboxProfile(p),
    onManageAll: () => managementController().openSandboxProfilesManageModal(),
    onNew: () => managementController().openSandboxProfileEditor(),
    searchWords: ['sandbox profiles', 'wards'],
    isDefault: (p, snap) => !!snap?.sandbox_profile_default && snap.sandbox_profile_default === p.name,
    onToggleDefault: (p, isDefault) => setDefault('sandbox', isDefault ? '' : p.name),
    itemPath: (name) => `/api/sandbox-profiles/${encodeURIComponent(name)}`,
    listPath: '/api/sandbox-profiles',
    ...libraryOps('sandbox_profiles'),
  },
  {
    key: 'usage_profiles',
    // Budgets for each agent; global and group limits live in the manager.
    afterProcesses: true,
    icon: () => wizWord('💰', '💰'),
    title: () => wizWord('Usage profiles', 'Tithes'),
    empty: () => wizWord('no usage profiles yet', 'no tithes yet'),
    items: (snap) => snap?.usage?.budgets?.profiles || [],
    name: (p) => p.name,
    chips: (p) => (p.rules || []).slice(0, 3).map((rule) => ({
      text: rule.metric === 'window_pct' ? `${rule.window}` : `${fmtAmount(rule.metric, rule.budget)} ${periodLabel(rule.period)}`,
      more: false, title: `${rule.name}: ${ruleSummary(rule)}`,
    })),
    fullChips: (p) => (p.rules || []).map((rule) => ({ text: `${rule.name}: ${ruleSummary(rule)}`, more: false })),
    // Assigned through the 💰 pickers, not dropped.
    drag: false,
    onManageItem: (p, node) => openUsageRulesManager({ ...usageNode(node), tab: 'profiles', profile: p.id }),
    onCloneItem: (p, node) => openUsageRulesManager({ ...usageNode(node), tab: 'profiles', profile: p.id, clone: true }),
    onDeleteItem: (p) => removeUsageProfile(p),
    onManageAll: () => openUsageRulesManager({ tab: 'profiles' }),
    onNew: () => openUsageRulesManager({ tab: 'profiles', newProfile: true }),
    searchWords: ['usage profiles', 'budgets', 'tithes'],
    isDefault: (p, snap) => (snap?.usage?.budgets?.global || []).includes(p.name),
    onToggleDefault: (p, isDefault) => toggleUsageDefault(p.name, lastSnapshot?.usage?.budgets?.global || [], isDefault),
  },
  {
    key: 'usage_limits',
    // The active global, group and fleet limits with their readings; each
    // carries its alert ladder. Disabled ones live in the manager.
    afterProcesses: true,
    icon: () => wizWord('🚦', '🚦'),
    title: () => wizWord('Usage limits & alerts', 'Coffer wards'),
    empty: () => wizWord('no active usage limits', 'no coffer wards'),
    items: (snap) => usageLimitItems(snap),
    name: (l) => l.label,
    chips: (l) => usageLimitChips(l),
    drag: false,
    onManageItem: (l, node) => openUsageLimit(l, node),
    onCloneItem: (l, node) => openUsageLimit(l, node, true),
    onDeleteItem: (l) => removeUsageLimit(l),
    onManageAll: () => openUsageRulesManager({ tab: 'limits' }),
    onNew: () => openUsageRulesManager({ tab: 'limits', newLimit: true }),
    searchWords: ['usage limits', 'alerts', 'alert rules', 'budgets', 'spend caps', 'fleet limits', 'coffer wards'],
  },
]);

// The per-section collapse flag lives under this dashPrefs prefix (req 5),
// server-backed like the open/collapsed dock flag itself — NOT localStorage,
// which the random per-start port would reset. Default EXPANDED (see
// isSectionOpen): a collapsed '0' is the only stored value, mirroring the
// groups' per-group fold key idiom.
const DOCK_SECTION_KEY = 'tclaude.dash.dock.section.';

// isSectionOpen reads a section's persisted collapse flag, defaulting to OPEN
// (only an explicit '0' collapses) — the three kinds stay discoverable by
// default, and a deliberate collapse survives restarts.
export function isDockSectionOpen(key) {
  return dashPrefs.getItem(DOCK_SECTION_KEY + key) !== '0';
}

export function setDockSectionOpen(key, open) {
  if (open) dashPrefs.removeItem(DOCK_SECTION_KEY + key);
  else dashPrefs.setItem(DOCK_SECTION_KEY + key, '0');
}

// Compatibility publish boundary for the shared snapshot poll. The dock body
// is exclusively Preact-owned; publishing a shallow snapshot wakes the keyed
// component tree without rebuilding an active drag source or actions menu.
export function renderDock() {
  featureState('dock')?.publish(lastSnapshot);
}

// isDockOpen reads the persisted flag, defaulting to OPEN when unset (the
// dock is a new, discovery-worthy surface). Only an explicit '0' collapses.
function isDockOpen() {
  return dashPrefs.getItem(DOCK_OPEN_KEY) !== '0';
}

// setDockOpen persists the open/collapsed pref and reflects it — the ONE
// mutation point behind every show/hide control: the edge tab and the in-dock
// collapse button (both via toggleDock in bindDock) and the Ctrl/Cmd-K command
// palette's "Show / Hide right panel" command (js/palette.js imports it).
// Writing the pref and applying it stay together so no caller can persist one
// state and paint another. Idempotent — re-setting the current state just
// re-applies. NB the dock is Groups-tab-only, so setting it open off the Groups
// tab persists the intent but paints nothing until the tab is active (applyDock
// Open folds isDockTab into the effective open state); the palette's show
// command switches to the Groups tab first so the panel is actually visible.
export function setDockOpen(open) {
  dashPrefs.setItem(DOCK_OPEN_KEY, open ? '1' : '0');
  applyDockOpen(open);
}

// isDockTab reports whether the Groups tab is the active one. The dock is
// offered ONLY there: its cards drag onto GROUP rows (dock-dnd.js), so the
// palette is meaningless on Jobs / Access / Config / Costs / … . We read the
// pane's `.active` class — the same source of truth every tab-switch site
// writes (bindTabs, the costs/plugins auto-hide redirects, showAccessTab, the
// command palette, keyboard cycling) — so a single observer over it (bindDock)
// catches every path without hooking each one. When Groups isn't active,
// applyDockOpen forces the effective open state off (no reserved page space)
// and CSS (body:not(.dock-tab)) hides the whole shell, edge toggle included.
function isDockTab() {
  return !!document.getElementById('tab-groups')?.classList.contains('active');
}

// applyDockOpen reflects the open state onto the body class (CSS reflows the
// page to reclaim the space when collapsed) and keeps the two show/hide controls
// in sync: the edge tab and the in-dock collapse button both mirror one state
// (JOH-390 item 7 removed the third, top-bar, toggle). It also re-homes the
// groups-toolbar globals (item 4) and re-syncs the dock top-inset, since the
// reserved space changes with the open state.
//
// The dock is Groups-tab-only: the tab availability rides a `dock-tab` body
// class (CSS hides the whole shell — panel + edge toggle — off the Groups tab)
// and folds into the EFFECTIVE open state, so no page space is reserved while
// the dock is hidden. `open` here is still the persisted PREF (isDockOpen),
// left untouched, so returning to Groups restores whatever open/collapsed state
// the human last chose. Called on boot, on the toggle, and on every tab switch
// (bindDock's Groups-pane observer) so the gate re-evaluates each time.
function applyDockOpen(open) {
  const onTab = isDockTab();
  document.body.classList.toggle('dock-tab', onTab);
  const eff = open && onTab;
  document.body.classList.toggle('dock-open', eff);
  const edge = $('#dock-toggle');
  if (edge) {
    edge.setAttribute('aria-expanded', eff ? 'true' : 'false');
    edge.title = eff
      ? wizWord('Collapse the palette', 'Furl the grimoire')
      : wizWord('Expand the palette', 'Unfurl the grimoire');
  }
  // Re-home the groups-toolbar globals into the open dock's head (JOH-390 item 4)
  // / return them to the toolbar when collapsed OR off the Groups tab. Done here
  // so it tracks EVERY effective-open change (boot, toggle, tab switch) in
  // lockstep with the body class. Off-tab both homes are hidden (the toolbar
  // lives in the inactive #tab-groups pane), so the move is invisible churn.
  syncDockActions(eff);
  syncDockTop();
  // Toggling the dock changes the reserved width and whether the horizontal
  // clearance spacer should be parked (req 3), but mutates no <main> child — so
  // hscroll's MutationObserver won't fire. Nudge it directly so the spacer +
  // full-bleed bars re-fit in the same frame.
  syncFullBleedBars();
}

// The groups-toolbar globals re-homed into the open dock's head (JOH-390 item 4):
// the "+ new group" primary, the ⚙ more-actions cog (+ its .action-menu) and the
// 🧠 dashboard-default-profile chip and its 🛡 sandbox-profile peer. While the
// dock is OPEN they live in the dock head (row 1 = new-group + cog; row 2 = the
// two profile controls); collapsed, they go
// back to their exact toolbar slots so the filter bar renders as before.
//
// We MOVE the live DOM nodes (not clones), so every listener rides along:
// id-bound ones (#group-create-open's click), the cog's direct menu listener,
// and the profile chips' cross-feature action route all remain valid. The toolbar
// filter bar + the dock head are both STATIC markup (Preact owns #dock-body and
// #groups-list), so nothing re-creates or clobbers the moved nodes.
//
// The cog's .action-menu still anchors to .filter-bar-cog (position:relative
// rides along) and opens downward INTO the dock body; at the header's top it
// stays within #agent-dock's box, so .dock-inner's overflow:hidden never clips it.
const DOCK_ACTION_SPECS = [
  { sel: '#group-create-open', dock: '#dock-actions-primary' },
  { sel: '.filter-bar-cog', dock: '#dock-actions-primary' },
  { sel: '#queued-spawns-control', dock: '#dock-actions-primary' },
  { sel: '#dashboard-default-profile-control', dock: '#dock-actions-profile' },
  { sel: '#dashboard-default-sandbox-profile-control', dock: '#dock-actions-profile' },
];
let dockActionHomes = null;
let lastDockActionsOpen = null;
function syncDockActions(open) {
  // Only act when the dock-open bit actually FLIPPED — the class observer below
  // fires on every body-class mutation (hscroll flags, dock-anim, wizard, …), so
  // this guard keeps those to a cheap no-op and prevents a redundant re-append
  // that could reorder the primary row.
  if (open === lastDockActionsOpen) return;
  lastDockActionsOpen = open;
  // Capture each control's ORIGINAL toolbar slot once, before the first move.
  // The nextSibling anchors are the inter-control whitespace text nodes, which
  // never move — so insertBefore restores the exact slot regardless of the order
  // we process the controls in.
  if (!dockActionHomes) {
    dockActionHomes = DOCK_ACTION_SPECS.map((s) => {
      const el = $(s.sel);
      if (!el) return null;
      return { el, dock: $(s.dock), home: el.parentNode, next: el.nextSibling };
    });
  }
  for (const h of dockActionHomes) {
    if (!h || !h.el) continue;
    if (open) {
      // Append in spec order → primary row becomes [+ new group][⚙ cog]. Guarded
      // so a repeat call (idempotent re-apply) doesn't re-append and reorder.
      if (h.dock && h.el.parentNode !== h.dock) h.dock.appendChild(h.el);
    } else if (h.home && h.el.parentNode !== h.home) {
      h.home.insertBefore(h.el, h.next);
    }
  }
}

// The dock's top inset is the shared content-area inset (--dock-top), measured
// and published by chrome-inset.js — the human-notification reader rides the
// same variable. The dock owns the listeners that keep it fresh (see bindDock);
// the measurement itself is no longer dock-private.
const syncDockTop = () => syncChromeTopInset();

export function bindDock() {
  if (!$('#agent-dock')) return;
  // Re-home the toolbar globals off ANY change to body.dock-open, not only the
  // applyDockOpen path (JOH-390 item 4). The production toggle always routes
  // through applyDockOpen, but this keeps the controls correctly placed if the
  // class is flipped directly (e.g. by test or automation code), so the
  // move can never desync from the visible open state. The change-guard in
  // syncDockActions makes the extra body-class mutations a no-op.
  new MutationObserver(() => syncDockActions(document.body.classList.contains('dock-open')))
    .observe(document.body, { attributes: true, attributeFilter: ['class'] });
  // The dock is Groups-tab-only (req): re-evaluate the gate on every tab switch.
  // Every tab-switch site toggles the `active` class on the <section> panes, so
  // one observer over the Groups pane's class catches them all — present and
  // future — without hooking each site. applyDockOpen re-reads isDockTab, so it
  // hides/shows the shell and drops/reserves the page space to match. (This
  // observes the Groups PANE, not body, so applyDockOpen's own body-class writes
  // can't retrigger it — no feedback loop with the observer above.)
  const groupsPane = document.getElementById('tab-groups');
  if (groupsPane) {
    new MutationObserver(() => applyDockOpen(isDockOpen()))
      .observe(groupsPane, { attributes: true, attributeFilter: ['class'] });
  }
  applyDockOpen(isDockOpen());
  // Enable the slide transition only AFTER the initial state is painted, so a
  // default-open dock doesn't flash-slide in on load (the CSS resting state is
  // collapsed). A rAF lands after the first paint of the applied state.
  requestAnimationFrame(() => document.body.classList.add('dock-anim'));

  // One toggler drives both show/hide controls — the edge tab and the in-dock
  // collapse — flipping the same dashPrefs-backed state (JOH-390 item 7 removed
  // the third, top-bar, control). The command palette's show/hide commands flip
  // the same state through the shared setDockOpen.
  const toggleDock = () => setDockOpen(!isDockOpen());
  // Two show/hide controls now (JOH-390 item 7 removed the top-bar toggle): the
  // edge chevron tab and the in-dock "Hide ›/Furl ›" collapse.
  $('#dock-toggle')?.addEventListener('click', toggleDock);
  $('#dock-collapse')?.addEventListener('click', toggleDock);

  // Keep the content-area top-inset (req 1) fresh as the page scrolls the
  // chrome away and as the chrome's own height changes (slop marquee showing/
  // hiding, wrapping controls or tab strip, window resize). syncDockTop is
  // rAF-coalesced, so these can fire freely. Passive scroll listener — we never
  // preventDefault. Observe EVERY chrome bar, not just the header: --dock-top
  // tracks nav's bottom, which also moves when the marquee toggles between
  // header and nav or the nav strip wraps — neither resizes the header.
  window.addEventListener('scroll', syncDockTop, { passive: true });
  window.addEventListener('resize', syncDockTop);
  // Section labels and category icons both have wizard-mode variants. Repaint
  // immediately when the theme flips instead of waiting for the next poll.
  document.addEventListener('tclaude:wizard', renderDock);
  if ('ResizeObserver' in window) {
    const ro = new ResizeObserver(syncDockTop);
    for (const sel of ['header', '#slop-marquee', 'nav']) {
      const el = document.querySelector(sel);
      if (el) ro.observe(el);
    }
  }

  // Publish the current snapshot (if one already exists) into the Preact-owned
  // dock body. Later polls use the same compatibility boundary.
  renderDock();
}
