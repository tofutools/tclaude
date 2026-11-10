// groups-focus.js -- what the Groups tab shows around its group tree. There
// is one view: each group card lists its sections (Subgroups / Agents /
// Processes / Resources). Where processes are unavailable (a peer page that
// does not share them) or while the Route map is up, the cards show their
// member tables only. The Both | Agents | Processes lens switch that used to
// sit in the toolbar is gone; its prefs are cleared on read and its deep
// links (/groups/agents, /groups/processes) land on bare /groups.
//
// What stays here: the single-run focus (/groups/runs/<group>/<run-id>), the
// helpers that click through to a group's runs, and the run-kind checklist
// the toolbar's ▾ view menu carries. The run cards themselves live in
// groups-processes.js; GroupsProcessesPlaceholder stands in while this
// agentd serves no run context.

import { h } from 'preact';
import { useEffect } from 'preact/hooks';
import htm from 'htm';
import {
  RUN_KINDS, RUN_KIND_LABELS, normalizeRunFilter, processContext, runFilterHas, runKindCounts, toggleRunKind,
} from './process-context.js';
import { processesAvailable } from './feature-availability.js';
import { ViewMenuHeading, ViewMenuSep } from './view-menu.js';

const html = htm.bind(h);

// The retired lens prefs: the three-lens one and the two-lens one before it.
export const STALE_GROUPS_LENS_PREFS = ['tclaude.dash.groups.lens', 'tclaude.dash.groups.focus'];

// clearStaleGroupsLensPrefs drops a remembered lens, which nothing reads now.
export function clearStaleGroupsLensPrefs(prefs) {
  for (const key of STALE_GROUPS_LENS_PREFS) {
    if (prefs.getItem(key) !== null) prefs.removeItem?.(key);
  }
}

// groupsShowRuns reports whether the group cards carry their Processes
// sections: wherever processes are available, except under the Route map.
export function groupsShowRuns(state) {
  return processesAvailable(state.snapshot.value) && state.subview.value !== 'routes';
}

// groupsRunLocation is the single-run focus: /groups/runs/<group>/<run-id>.
export function groupsRunLocation(run) {
  return { tab: 'groups', subtab: 'runs', selection: run.group, item: run.runId };
}

// shownGroupsLocation is the location for what shows: the open run, else
// bare Groups.
function shownGroupsLocation(state) {
  const run = state.focusedRun?.value;
  return run ? groupsRunLocation(run) : { tab: 'groups' };
}

// focusRun opens the single-run focus on `run` (null closes it) as a user
// gesture, announcing the new location so nav-history pushes one entry. A
// correction (the run's group is gone) rewrites the current entry instead.
export function focusRun(state, run, { correction = false, doc = document } = {}) {
  const before = state.focusedRun.value;
  state.setFocusedRun(run);
  if (state.focusedRun.value === before) return false;
  doc.dispatchEvent(new CustomEvent('tclaude:navigated', { detail: { location: shownGroupsLocation(state), correction } }));
  return true;
}

// showGroupRuns brings the runs back on screen: it leaves the Route map,
// which hides them. `announce` records that as a user gesture (one history
// entry); without it the caller's own navigation is the step. It reports
// whether it left the Route map.
export function showGroupRuns(state, { announce = false, doc = document } = {}) {
  if (state.subview?.value !== 'routes') return false;
  state.setSubview('members', '');
  if (announce) doc.dispatchEvent(new CustomEvent('tclaude:navigated', { detail: { location: shownGroupsLocation(state) } }));
  return true;
}

// useGroupsFocusRestore applies history traversal to the single-run focus:
// /groups/runs/... opens that run, any other Groups location closes it.
// Route map locations leave it alone.
export function useGroupsFocusRestore(state, doc = document) {
  useEffect(() => {
    const restore = (event) => {
      const location = event.detail?.location;
      if (location?.tab !== 'groups' || location.subtab === 'routes') return;
      if (location.subtab === 'runs') {
        showGroupRuns(state);
        state.setFocusedRun(location.selection && location.item ? { group: location.selection, runId: location.item } : null);
        return;
      }
      state.setFocusedRun?.(null);
    };
    doc.addEventListener('tclaude:restore-location', restore);
    return () => doc.removeEventListener('tclaude:restore-location', restore);
  }, [state, doc]);
}

// RunKindFilter is the run filter, a section of the toolbar's ▾ view menu: a
// checklist of run kinds over every group's Processes section, with a count
// per kind once the run context is loaded. Unticking the last kind is
// refused (toggleRunKind). Nothing shows where processes are unavailable.
export function RunKindFilter({ state, wizard = false, source = processContext }) {
  if (!processesAvailable(state.snapshot.value)) return null;
  const filter = state.processFilter?.value;
  const counts = source.status.value === 'ready' ? runKindCounts(source.context.value) : null;
  return html`
    <${ViewMenuSep} />
    <${ViewMenuHeading} title=${wizard ? 'Which rites each party shows' : 'Which runs each group’s Processes section shows'}>${wizard ? 'Rites' : 'Processes'}<//>
    <div class="groups-run-kinds" role="group" aria-label=${wizard ? 'Show rites' : 'Show processes'}>
      ${RUN_KINDS.map((kind) => {
        const checked = runFilterHas(filter, kind);
        return html`<label key=${kind} class=${`filter-toggle groups-run-kind groups-run-kind-${kind}`}>
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

export function GroupsProcessesPlaceholder({ wizard = false }) {
  return html`<div class="groups-processes-placeholder" role="status" data-groups-processes-placeholder>
    <div class="groups-processes-placeholder-glyph" aria-hidden="true">◈</div>
    <strong>${wizard ? 'The rites of each party will gather here.' : 'Process runs per group will appear here.'}</strong>
    <span>${wizard
      ? 'Each party’s rites will be drawn as graphs, with its heroes shown on the steps they perform.'
      : 'Each group’s runs will show as graph cards, with agents shown as avatars on the nodes they perform.'}</span>
  </div>`;
}
