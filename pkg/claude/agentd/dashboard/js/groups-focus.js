// groups-focus.js -- the Groups tab's Agents | Processes focus switch (mock
// G6, option A). The same group tree is shown through one of two lenses:
// Agents keeps the member tables as the main content; Processes puts each
// group's run graphs at the centre. The switch is a pair of icon buttons
// beside the group filter, with count badges and the run-kind checklist
// floating off the Processes icon. It is remembered as a dashboard pref,
// deep-linkable as /groups/processes, and only offered while
// `features.processes` is on.
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

export const GROUPS_FOCUS_PREF = 'tclaude.dash.groups.focus';

export function normalizeGroupsFocus(value) {
  return value === 'processes' ? 'processes' : 'agents';
}

export function processesFocusAvailable(snapshot) {
  return snapshot?.processes_enabled === true;
}

// effectiveGroupsFocus is the lens actually shown: a remembered Processes
// focus reads as Agents while the feature is off or the Route map is up.
export function effectiveGroupsFocus(state) {
  return processesFocusAvailable(state.snapshot.value)
    && state.subview.value !== 'routes'
    && state.focus.value === 'processes' ? 'processes' : 'agents';
}

export function groupsFocusLocation(focus) {
  return normalizeGroupsFocus(focus) === 'processes'
    ? { tab: 'groups', subtab: 'processes' }
    : { tab: 'groups' };
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
  doc.dispatchEvent(new CustomEvent('tclaude:navigated', { detail: { location: groupsFocusLocation(focus) } }));
  return true;
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
// the focus in its subtab (none = Agents). Route map locations leave the
// remembered focus alone.
export function useGroupsFocusRestore(state, doc = document) {
  useEffect(() => {
    const restore = (event) => {
      const location = event.detail?.location;
      if (location?.tab !== 'groups' || location.subtab === 'routes') return;
      state.setFocus(location.subtab === 'processes' ? 'processes' : 'agents');
    };
    doc.addEventListener('tclaude:restore-location', restore);
    return () => doc.removeEventListener('tclaude:restore-location', restore);
  }, [state, doc]);
}

// The Processes button's icon: the same small flow graph as the group
// headers' process indicators (groups-process-indicators.js).
// The Agents button's icon. An inline SVG rather than the 👥 emoji, whose
// colour comes from the OS emoji font (dark purple on some systems, nearly
// invisible on the selected button); CSS colours it like FlowIcon.
function PeopleIcon() {
  return html`<svg class="groups-focus-people" viewBox="0 0 20 16" aria-hidden="true">
    <circle cx="7" cy="5" r="2.8" /><path d="M1.6 14.2c0-3.2 2.4-5.2 5.4-5.2s5.4 2 5.4 5.2z" />
    <circle class="back" cx="13.6" cy="5.6" r="2.4" /><path class="back" d="M11.2 9.6c.7-.3 1.5-.5 2.4-.5 2.6 0 4.7 1.8 4.7 4.6v.5h-5" />
  </svg>`;
}

function FlowIcon() {
  return html`<svg class="groups-focus-flow" viewBox="0 0 22 16" aria-hidden="true">
    <polyline points="3,10 11,5 19,10" />
    <circle cx="3" cy="10" r="2.4" /><circle cx="11" cy="5" r="2.4" /><circle cx="19" cy="10" r="2.4" />
  </svg>`;
}

// RunKindMenu is the run filter: a checklist of run kinds that floats under
// the Processes button on hover or keyboard focus, so it takes no toolbar
// row of its own. Unticking the last kind is refused (toggleRunKind).
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
  return html`<div class="groups-focus-switch" role="group" aria-label=${wizard ? 'Party lens' : 'Groups focus'}>
    ${button('agents', html`<${PeopleIcon} />`, 'Agents', 'Familiars', agentCount(groups),
      wizard ? 'Familiars: the party rosters' : 'Agents: member tables, with process state as small indicators')}
    <span class="groups-focus-runs">
      ${button('processes', html`<${FlowIcon} />`, 'Processes', 'Rites', runs,
        wizard ? 'Rites: each party’s rites as graphs — hover to choose which to show' : 'Processes: run graphs per group — hover to choose which runs to show')}
      <${RunKindMenu} state=${state} wizard=${wizard} counts=${ready ? runKindCounts(source.context.value) : null} />
    </span>
  </div>`;
}

export function GroupsProcessesPlaceholder({ wizard = false }) {
  return html`<div class="groups-processes-placeholder" role="status" data-groups-processes-placeholder>
    <div class="groups-processes-placeholder-glyph" aria-hidden="true">◈</div>
    <strong>${wizard ? 'The rites of each party will gather here.' : 'Process runs per group will appear here.'}</strong>
    <span>${wizard
      ? 'Each party’s rites will be drawn as graphs, with its familiars shown on the steps they perform. Switch to Familiars to tend the party itself.'
      : 'Each group’s runs will show as graph cards, with agents shown as avatars on the nodes they perform. Switch to Agents for the member tables.'}</span>
  </div>`;
}
