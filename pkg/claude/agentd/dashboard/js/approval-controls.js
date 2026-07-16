export const REVIEWER_DEFAULT = '';
export const REVIEWER_HUMAN = 'human';
export const REVIEWER_AUTO = 'auto_review';

const CODEX_POLICY_LABELS = {
  never: 'Never ask — no approval prompts',
  untrusted: 'Ask for untrusted commands',
  'on-failure': 'On sandbox failure (deprecated)',
  'on-request': 'Ask when Codex requests',
};

export function approvalPolicyLabel(harnessName, mode, recommended = '') {
  const label = harnessName === 'codex' ? CODEX_POLICY_LABELS[mode] || mode : mode;
  return `${label}${mode === recommended ? ' (recommended)' : ''}`;
}

export function reviewerValue(value) {
  return value == null ? REVIEWER_DEFAULT : value ? REVIEWER_AUTO : REVIEWER_HUMAN;
}

export function readReviewer(value) {
  if (value === REVIEWER_AUTO) return true;
  if (value === REVIEWER_HUMAN) return false;
  return null;
}

export function approvalReviewerOptions(profile = false) {
  return [
    {
      value: REVIEWER_DEFAULT,
      label: profile ? 'Default (unset; inherit lower tiers)' : 'Default (profile, else human)',
    },
    { value: REVIEWER_HUMAN, label: 'Human reviewer' },
    { value: REVIEWER_AUTO, label: 'Codex auto-review' },
  ];
}

export function approvalReviewerHelp(value, approvalPolicy = '') {
  switch (value) {
  case REVIEWER_AUTO:
    if (approvalPolicy === 'never') {
      return '⚠ No effect with “Never ask”: that policy creates no approval requests. Choose an interactive policy to use Codex auto-review.';
    }
    return 'Routes eligible approval requests to a separate Codex reviewer. This changes who reviews; it does not expand the sandbox.';
  case REVIEWER_HUMAN:
    return 'Eligible approval requests pause for a person. A detached agent can block while it waits.';
  default:
    return 'Leaves the reviewer unset so profile tiers may supply it; otherwise Codex uses a human reviewer.';
  }
}
