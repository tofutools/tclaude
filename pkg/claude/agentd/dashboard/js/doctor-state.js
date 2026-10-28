// doctor-state.js — the Diagnostics › Doctor view's checks and selected category.
//
// The checks come from GET /api/doctor (pkg/claude/doctor). They are fetched on
// load, on a slow poll for the nav badges, and whenever the view is entered, so
// an operator who just installed a tool sees it clear without a reload.

import { computed, signal } from '@preact/signals';
import { dashboardState } from './snapshot-store.js';
import { activitySubtab, activityViewActive } from './activity-state.js';

// Doctor is a Diagnostics subtab (/diagnostics/doctor).
export const DOCTOR_SUBTAB = 'doctor';

// Sidebar order. 'attention' is the default lens: everything not OK/info.
export const DOCTOR_CATEGORIES = Object.freeze([
  { id: 'decisions', label: 'Decisions' },
  { id: 'sandbox', label: 'Sandbox' },
  { id: 'tools', label: 'Tools' },
  { id: 'harnesses', label: 'Harnesses' },
  { id: 'setup', label: 'Hooks & setup' },
]);

// needsAttention reports whether a check asks the operator to do something.
export function needsAttention(check) {
  return check.severity === 'decision' || check.severity === 'blocking' || check.severity === 'warning';
}

const SEVERITY_ORDER = { decision: 0, blocking: 1, warning: 2, info: 3, ok: 4 };

export function sortChecks(checks) {
  return [...checks].sort((a, b) => (SEVERITY_ORDER[a.severity] ?? 9) - (SEVERITY_ORDER[b.severity] ?? 9));
}

export function createDoctorState({ activeTab = dashboardState.activeTab, subtab = activitySubtab } = {}) {
  const checks = signal([]);
  const phase = signal('idle');
  const error = signal(null);
  const category = signal('attention');
  const busy = signal(false);
  const active = computed(() => activityViewActive(activeTab, subtab, DOCTOR_SUBTAB));
  const counts = computed(() => {
    const out = { decision: 0, blocking: 0, warning: 0, attention: 0, all: checks.value.length, byCategory: {} };
    for (const check of checks.value) {
      if (out[check.severity] !== undefined) out[check.severity] += 1;
      if (needsAttention(check)) {
        out.attention += 1;
        out.byCategory[check.category] = (out.byCategory[check.category] || 0) + 1;
      }
    }
    return out;
  });
  const visible = computed(() => {
    const all = checks.value;
    if (category.value === 'attention') return sortChecks(all.filter(needsAttention));
    if (category.value === 'all') return sortChecks(all);
    return sortChecks(all.filter((check) => check.category === category.value));
  });
  return Object.freeze({ checks, phase, error, category, busy, active, counts, visible });
}

export const doctorState = createDoctorState();
