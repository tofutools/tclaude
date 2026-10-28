// groups-focus.js -- the Groups tab's Both | Agents | Processes focus switch
// (mock G6, option A, plus the fused lens). The same group tree is shown
// through one of three lenses: Both (the default) shows each group's member
// table and its runs as two sections; Agents keeps only the member tables;
// Processes puts each group's run graphs at the centre. The switch is three
// icon buttons beside the group filter, with count badges and the run-kind
// checklist floating off the lenses that show runs. It is remembered as a
// dashboard pref, deep-linkable as /groups (Both), /groups/agents and
// /groups/processes, and only offered while `features.processes` is on.
//
// The Processes lens itself (the G3 run cards) lives in groups-processes.js;
// GroupsProcessesPlaceholder stands in while this agentd serves no run context.

import { h } from 'preact';
import { useEffect } from 'preact/hooks';
import htm from 'htm';
import {
  RUN_KINDS, RUN_KIND_LABELS, activeRunCount, normalizeRunFilter, processContext, runFilterHas, runKindCounts,
  toggleRunKind,
} from './process-context.js';

const html = htm.bind(h);

export const GROUPS_FOCUS_PREF = 'tclaude.dash.groups.lens';
// The two-lens pref. Its 'agents' was the old default, not a choice anyone
// can be told apart by, so only a remembered 'processes' carries over.
export const LEGACY_GROUPS_FOCUS_PREF = 'tclaude.dash.groups.focus';

export function normalizeGroupsFocus(value) {
  return value === 'processes' || value === 'agents' ? value : 'both';
}

// restoreGroupsFocus reads the remembered lens, falling back to the legacy pref.
export function restoreGroupsFocus(prefs) {
  const saved = prefs.getItem(GROUPS_FOCUS_PREF);
  if (saved !== null) return normalizeGroupsFocus(saved);
  return prefs.getItem(LEGACY_GROUPS_FOCUS_PREF) === 'processes' ? 'processes' : 'both';
}

export function processesFocusAvailable(snapshot) {
  return snapshot?.processes_enabled === true;
}

// effectiveGroupsFocus is the lens actually shown: any remembered lens reads
// as Agents while the feature is off or the Route map is up.
export function effectiveGroupsFocus(state) {
  if (!processesFocusAvailable(state.snapshot.value) || state.subview.value === 'routes') return 'agents';
  return normalizeGroupsFocus(state.focus.value);
}

export function groupsFocusLocation(focus) {
  const lens = normalizeGroupsFocus(focus);
  return lens === 'both' ? { tab: 'groups' } : { tab: 'groups', subtab: lens };
}

// groupsRunLocation is the single-run focus: /groups/runs/<group>/<run-id>.
// Both and Processes show it alike, so the lens is not part of it.
export function groupsRunLocation(run) {
  return { tab: 'groups', subtab: 'runs', selection: run.group, item: run.runId };
}

// shownGroupsLocation is the location for what `focus` shows: the open run
// when that lens shows the single-run focus, else the lens itself.
function shownGroupsLocation(state, focus) {
  const run = state.focusedRun?.value;
  return run && normalizeGroupsFocus(focus) !== 'agents' ? groupsRunLocation(run) : groupsFocusLocation(focus);
}

// focusRun opens the single-run focus on `run` (null closes it) as a user
// gesture, announcing the new location so nav-history pushes one entry. A
// correction (the run's group is gone) rewrites the current entry instead.
export function focusRun(state, run, { correction = false, doc = document } = {}) {
  const before = state.focusedRun.value;
  state.setFocusedRun(run);
  if (state.focusedRun.value === before) return false;
  const location = shownGroupsLocation(state, effectiveGroupsFocus(state));
  doc.dispatchEvent(new CustomEvent('tclaude:navigated', { detail: { location, correction } }));
  return true;
}

// focusFromLocation is the lens a Groups location names: its subtab, or Both.
export function focusFromLocation(location) {
  return normalizeGroupsFocus(location?.subtab);
}

// chooseGroupsFocus is the user gesture: switch, persist, and announce the
// new location to nav-history so it pushes one entry. It also leaves the
// Route map, whose subview would otherwise keep hiding the chosen lens.
// Re-selecting the shown focus is a no-op, so it never forges a history step.
export function chooseGroupsFocus(state, next, { doc = document } = {}) {
  const focus = normalizeGroupsFocus(next);
  const onRoutes = state.subview.value === 'routes';
  if (state.focus.value === focus && !onRoutes) return false;
  if (onRoutes) state.setSubview('members', '');
  state.setFocus(focus);
  doc.dispatchEvent(new CustomEvent('tclaude:navigated', { detail: { location: shownGroupsLocation(state, focus) } }));
  return true;
}

// revealRunsLens is a click-through to runs (a header glyph, a run badge):
// it keeps a remembered Both, leaving the Route map if need be, and otherwise
// switches to Processes. It reports whether Both is what shows.
export function revealRunsLens(state, { doc = document } = {}) {
  const fused = normalizeGroupsFocus(state.focus.value) === 'both';
  chooseGroupsFocus(state, fused ? 'both' : 'processes', { doc });
  return fused;
}

// showRunsLens switches to `lens` only from Agents: Both already shows every
// group's runs, so a request to see them never takes the operator out of it.
// It leaves the Route map either way, which would otherwise hide the runs.
export function showRunsLens(state, lens = 'processes') {
  if (state.subview?.value === 'routes') state.setSubview('members', '');
  if (normalizeGroupsFocus(state.focus.value) === 'both') return;
  state.setFocus(lens);
}

// agentCount counts distinct agents across the groups currently shown, so the
// Agents segment doubles as a summary of what the filter lets through.
export function agentCount(groups) {
  const seen = new Set();
  for (const group of groups || []) {
    for (const member of group.members || []) {
      const id = member.agent_id || member.conv_id || member.id;
      if (id) seen.add(id);
    }
  }
  return seen.size;
}

// useGroupsFocusRestore applies history traversal: a Groups location carries
// the focus in its subtab (none = Both), or the open run (runs), which keeps
// a lens that shows it and otherwise opens in Processes. Route map locations
// leave the remembered focus alone.
export function useGroupsFocusRestore(state, doc = document) {
  useEffect(() => {
    const restore = (event) => {
      const location = event.detail?.location;
      if (location?.tab !== 'groups' || location.subtab === 'routes') return;
      if (location.subtab === 'runs') {
        showRunsLens(state);
        state.setFocusedRun(location.selection && location.item ? { group: location.selection, runId: location.item } : null);
        return;
      }
      state.setFocusedRun?.(null);
      state.setFocus(focusFromLocation(location));
    };
    doc.addEventListener('tclaude:restore-location', restore);
    return () => doc.removeEventListener('tclaude:restore-location', restore);
  }, [state, doc]);
}

// The Agents button's icon. An inline SVG rather than the 👥 emoji, whose
// colour comes from the OS emoji font (dark purple on some systems, nearly
// invisible on the selected button); CSS colours it like FlowIcon.
function PeopleIcon() {
  return html`<svg class="groups-focus-people" viewBox="0 0 20 16" aria-hidden="true">
    <circle cx="7" cy="5" r="2.8" /><path d="M1.6 14.2c0-3.2 2.4-5.2 5.4-5.2s5.4 2 5.4 5.2z" />
    <circle class="back" cx="13.6" cy="5.6" r="2.4" /><path class="back" d="M11.2 9.6c.7-.3 1.5-.5 2.4-.5 2.6 0 4.7 1.8 4.7 4.6v.5h-5" />
  </svg>`;
}

// The Processes button's icon: the same small flow graph as the group
// headers' process indicators (groups-process-indicators.js).
function FlowIcon() {
  return html`<svg class="groups-focus-flow" viewBox="0 0 22 16" aria-hidden="true">
    <polyline points="3,10 11,5 19,10" />
    <circle cx="3" cy="10" r="2.4" /><circle cx="11" cy="5" r="2.4" /><circle cx="19" cy="10" r="2.4" />
  </svg>`;
}

// BothIcon is the fused lens: the people and the flow graph side by side.
function BothIcon() {
  return html`<span class="groups-focus-both"><${PeopleIcon} /><${FlowIcon} /></span>`;
}

// RunKindMenu is the run filter: a checklist of run kinds that floats under
// the Both and Processes buttons on hover or keyboard focus, so it takes no
// toolbar row of its own. Unticking the last kind is refused (toggleRunKind).
function RunKindMenu({ state, wizard, counts }) {
  const filter = state.processFilter?.value;
  return html`<div class="groups-run-menu" role="group" aria-label=${wizard ? 'Show rites' : 'Show processes'}>
    <div class="groups-run-menu-head">${wizard ? 'Show rites' : 'Show processes'}</div>
    ${RUN_KINDS.map((kind) => {
      const checked = runFilterHas(filter, kind);
      return html`<label key=${kind} class=${`groups-run-kind groups-run-kind-${kind}${checked ? ' checked' : ''}`}>
        <input type="checkbox" data-process-filter=${kind} checked=${checked}
          onChange=${(event) => {
            const next = toggleRunKind(state.processFilter?.value, kind);
            // A refused untick leaves the signal unchanged, so nothing
            // re-renders: put the browser-cleared tick back by hand.
            if (next === normalizeRunFilter(state.processFilter?.value)) event.currentTarget.checked = true;
            state.setProcessFilter?.(next);
          }} />
        <span class="groups-run-dot" aria-hidden="true"></span>
        <span class="groups-run-label">${RUN_KIND_LABELS[kind][wizard ? 1 : 0]}</span>
        ${counts && html`<span class="groups-run-count">${counts[kind]}</span>`}
      </label>`;
    })}
  </div>`;
}

export function GroupsFocusSwitch({ state, groups, wizard = false, source = processContext }) {
  if (!processesFocusAvailable(state.snapshot.value)) return null;
  const ready = source.status.value === 'ready';
  const runs = ready ? activeRunCount(source.context.value) : null;
  const focus = effectiveGroupsFocus(state);
  const button = (value, icon, plain, arcane, count, title) => html`<button
      type="button"
      class=${`groups-focus-option${focus === value ? ' active' : ''}`}
      aria-pressed=${focus === value ? 'true' : 'false'}
      aria-label=${`${wizard ? arcane : plain}${count != null ? ` ${count}` : ''}`}
      data-groups-focus=${value}
      title=${title}
      onClick=${() => chooseGroupsFocus(state, value)}
    >${icon}${count != null && html`<span class="groups-focus-count">${count}</span>`}</button>`;
  const counts = ready ? runKindCounts(source.context.value) : null;
  return html`<div class="groups-focus-switch" role="group" aria-label=${wizard ? 'Party lens' : 'Groups focus'}>
    <span class="groups-focus-runs groups-focus-lead">
      ${button('both', html`<${BothIcon} />`, 'Both', 'Whole party', null,
        wizard ? 'Whole party: each party’s roster, then its rites — hover to choose which rites to show'
          : 'Both: each group’s member table, then its processes — hover to choose which runs to show')}
      <${RunKindMenu} state=${state} wizard=${wizard} counts=${counts} />
    </span>
    ${button('agents', html`<${PeopleIcon} />`, 'Agents', 'Heroes', agentCount(groups),
      wizard ? 'Heroes: the party rosters' : 'Agents: member tables, with process state as small indicators')}
    <span class="groups-focus-runs">
      ${button('processes', html`<${FlowIcon} />`, 'Processes', 'Rites', runs,
        wizard ? 'Rites: each party’s rites as graphs — hover to choose which to show' : 'Processes: run graphs per group — hover to choose which runs to show')}
      <${RunKindMenu} state=${state} wizard=${wizard} counts=${counts} />
    </span>
  </div>`;
}

export function GroupsProcessesPlaceholder({ wizard = false }) {
  return html`<div class="groups-processes-placeholder" role="status" data-groups-processes-placeholder>
    <div class="groups-processes-placeholder-glyph" aria-hidden="true">◈</div>
    <strong>${wizard ? 'The rites of each party will gather here.' : 'Process runs per group will appear here.'}</strong>
    <span>${wizard
      ? 'Each party’s rites will be drawn as graphs, with its heroes shown on the steps they perform. Switch to Heroes to tend the party itself.'
      : 'Each group’s runs will show as graph cards, with agents shown as avatars on the nodes they perform. Switch to Agents for the member tables.'}</span>
  </div>`;
}
