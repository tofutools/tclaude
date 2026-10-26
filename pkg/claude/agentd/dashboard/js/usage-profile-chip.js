import { h } from 'preact';
import { useLayoutEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { dashboardState } from './snapshot-store.js';
import { usageRulesApi } from './usage-rules-actions.js';
import { openUsageRulesManager } from './modal-usage-rules.js';
import { fmtAmount, periodLabel } from './usage-rules-model.js';
import { shellToast } from './shell-state.js';

const html = htm.bind(h);
const MANAGE = '/manage';
const NEW = '/new';
const SEVERAL = '/several';

async function refreshSnapshot() {
  const { refresh } = await import('./refresh.js');
  return refresh();
}

// budgetsOf reads the snapshot's compact usage-profile view (usage_budgets_snapshot.go).
export function budgetsOf(snapshot) {
  return snapshot?.usage?.budgets || null;
}

// assignedUsageProfiles names the profiles at the global layer, or a group's.
export function assignedUsageProfiles(budgets, scope, group = null) {
  if (!budgets) return [];
  if (scope === 'global') return budgets.global || [];
  return budgets.groups?.[String(group?.id)] || [];
}

// groupLimitLine describes a group's limit readings for the chip's tooltip.
function groupLimitLine(snapshot, group) {
  const limits = (snapshot?.usage?.usage_limits || []).filter((l) => l.scope === 'group' && l.group_id === group?.id);
  return limits.map((l) => `Group limit "${l.name}": ${fmtAmount(l.metric, l.value)} / ${fmtAmount(l.metric, l.budget)} ${periodLabel(l.period)} (${Math.round(l.pct)}%)`).join('\n');
}

// UsageProfileChip is the 💰 picker beside the 🧠 spawn-profile and 🛡
// sandbox-profile chips: the global default usage profile in the toolbar
// (scope "global") or a group's usage profiles on its row (scope "group").
// One profile is picked inline; several, or anything else, open the manager.
export function UsageProfileChip({ scope, group = null, className = '', snapshot = dashboardState.snapshot,
  api = usageRulesApi, onChanged = refreshSnapshot }) {
  const snap = snapshot.value;
  const budgets = budgetsOf(snap);
  // Usage rules are this node's own; a peer's page has no manager for them.
  const remote = Boolean(globalThis.__tclaudeRemoteNode?.id);
  const profiles = budgets?.profiles || [];
  const names = assignedUsageProfiles(budgets, scope, group);
  const [editing, setEditing] = useState(false);
  const [busy, setBusy] = useState(false);
  const selectRef = useRef(null);
  const triggerRef = useRef(null);
  const restoreFocus = useRef(false);
  useLayoutEffect(() => {
    if (editing) selectRef.current?.focus();
    else if (restoreFocus.current) { restoreFocus.current = false; triggerRef.current?.focus(); }
  }, [editing]);
  const where = scope === 'global' ? 'Global default usage profile' : `Usage profiles for ${group?.name}`;
  const role = scope === 'global'
    ? 'budgets every agent whose group has none of its own'
    : "budgets each member of this group, replacing the global default; a spawn profile's usage profiles add to it";
  const close = () => { restoreFocus.current = true; setEditing(false); };
  if (remote) return null;
  if (editing) {
    const current = names.length > 1 ? SEVERAL : names[0] || '';
    return html`<select ref=${selectRef} class="group-default-profile-select usage-profile-select" value=${current} disabled=${busy}
      aria-label=${where}
      onKeyDown=${(event) => { if (event.key === 'Escape') { event.preventDefault(); close(); } }}
      onBlur=${() => { if (!busy) setEditing(false); }}
      onChange=${async (event) => {
        const value = event.currentTarget.value;
        if (value === current) { close(); return; }
        if (value === MANAGE || value === NEW || value === SEVERAL) {
          setEditing(false);
          openUsageRulesManager({ tab: value === NEW ? 'profiles' : 'assignments' });
          return;
        }
        setBusy(true);
        try {
          await api.assign(scope, scope === 'group' ? group.name : '', value ? [value] : []);
          shellToast(value ? `${where}: ${value}` : `${where} cleared`);
          await onChanged();
          close();
        } catch (cause) {
          shellToast(cause.message, true);
        } finally { setBusy(false); }
      }}>
      <option value=${NEW}>＋ new usage profile…</option>
      <option value="">${scope === 'global' ? '(none)' : '(none — no group layer)'}</option>
      ${names.length > 1 ? html`<option value=${SEVERAL}>${names.join(' + ')} (edit in manager…)</option>` : null}
      ${profiles.map((p) => html`<option key=${p.id} value=${p.name}>${p.name}</option>`)}
      <option value=${MANAGE}>Several profiles, limits… (open manager)</option>
    </select>`;
  }
  const limits = scope === 'group' ? groupLimitLine(snap, group) : '';
  const title = [names.length ? `${where}: ${names.join(', ')} — ${role}. Click to change.` : `No ${where.toLowerCase()} — click to set one. It ${role}.`, limits]
    .filter(Boolean).join('\n');
  return html`<span ref=${triggerRef} class=${`${className} usage-profile-chip${names.length ? '' : ' unset'}`} tabindex="0" role="button"
    data-usage-scope=${scope} data-group=${group?.name} title=${title} aria-label=${title}
    onClick=${(event) => { event.preventDefault(); event.stopPropagation(); setEditing(true); }}
    onKeyDown=${(event) => {
      if (event.key !== 'Enter' && event.key !== ' ') return;
      event.preventDefault(); event.stopPropagation(); setEditing(true);
    }}
  ><span class="group-profile-icon">💰</span><span class="qo-text">${names.length ? ` ${names.join(', ')}` : ''}</span></span>`;
}

// AgentBudgetBadge shows an agent's tightest budget on its row once it is
// at least half used or a step has fired; it opens the manager's Agents tab.
export function AgentBudgetBadge({ agentID, snapshot = dashboardState.snapshot }) {
  const budget = budgetsOf(snapshot.value)?.agents?.[agentID];
  if (!budget || budget.dry_run || (budget.pct < 50 && !budget.reached)) return null;
  const tone = budget.reached && budget.reached !== 'notify' ? 'over' : budget.reached || budget.pct >= 80 ? 'warn' : 'ok';
  const reached = budget.reached ? ` · ${budget.reached.replace('_', ' ')} step reached` : '';
  const paused = budget.paused ? ` · ${budget.paused}` : '';
  const title = `Usage budget "${budget.name}"${budget.profile ? ` (${budget.profile})` : ''}: ${fmtAmount(budget.metric, budget.value)} / ${fmtAmount(budget.metric, budget.budget)} (${Math.round(budget.pct)}%)${reached}${paused}${budget.rules > 1 ? ` · tightest of ${budget.rules} rules` : ''}. Click to manage.`;
  return html`<button type="button" class=${`activity-badge agent-budget-badge ${tone}`} title=${title} aria-label=${title}
    data-agent-budget=${agentID}
    onClick=${(event) => { event.stopPropagation(); openUsageRulesManager({ tab: 'agents', agent: agentID }); }}>💰${Math.round(budget.pct)}%</button>`;
}
