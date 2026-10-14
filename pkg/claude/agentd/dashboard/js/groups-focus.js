// groups-focus.js -- the Groups tab's Agents | Processes focus switch (mock
// G6, option A). The same group tree is shown through one of two lenses:
// Agents keeps the member tables as the main content; Processes puts each
// group's run graphs at the centre. The switch is a segmented control beside
// the group filter, remembered as a dashboard pref, deep-linkable as
// /groups/processes, and only offered while `features.processes` is on.
//
// The Processes lens renders a placeholder until the run-graph cards (G3)
// land.

import { h } from 'preact';
import { useEffect } from 'preact/hooks';
import htm from 'htm';

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
// new location to nav-history so it pushes one entry. Re-selecting the
// current focus is a no-op, so it never forges a history step.
export function chooseGroupsFocus(state, next, { doc = document } = {}) {
  const focus = normalizeGroupsFocus(next);
  if (state.focus.value === focus) return false;
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

export function GroupsFocusSwitch({ state, groups, wizard = false }) {
  if (!processesFocusAvailable(state.snapshot.value)) return null;
  const focus = effectiveGroupsFocus(state);
  const segment = (value, glyph, plain, arcane, count, title) => html`<button
      type="button"
      role="radio"
      class=${`groups-focus-option${focus === value ? ' active' : ''}`}
      aria-checked=${focus === value ? 'true' : 'false'}
      data-groups-focus=${value}
      title=${title}
      onClick=${() => chooseGroupsFocus(state, value)}
    ><span aria-hidden="true">${glyph}</span> ${wizard ? arcane : plain}${count != null && html` <span class="groups-focus-count">${count}</span>`}</button>`;
  return html`<div class="groups-focus-switch" role="radiogroup" aria-label=${wizard ? 'Party lens' : 'Groups focus'}>
    ${segment('agents', '👥', 'Agents', 'Familiars', agentCount(groups), 'Agents focus: member tables, with process state as small indicators')}
    ${segment('processes', '◈', 'Processes', 'Rites', null, 'Processes focus: run graphs per group, with agents as avatars on the nodes they perform')}
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
