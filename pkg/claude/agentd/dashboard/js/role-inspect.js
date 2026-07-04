// role-inspect.js — the shared "what does this role carry?" render helper
// (JOH-351). A role picker (today: the templates dialog's per-agent role-library
// dropdown) shows only the role NAME, so picking one is blind — you can't see
// the brief, permissions or launch shape it drops onto the referencing agent.
// This module renders a compact, inspectable panel for a selected role so the
// pick is transparent. One helper, reused by every picker, so the surfaces never
// drift.
//
// Pure data → HTML string (escaped, safe for innerHTML); no DOM, no fetch. The
// caller owns the container and decides when to (re)render — typically on the
// picker's `change`. A role's fields are resolved at DEPLOY time (see the footer
// note), so the panel is a preview of a future deploy, not a live agent's state.

import { esc } from './helpers.js';

// roleLaunchRows returns the [label, value] pairs of a role's launch shape that
// are actually set, in a stable display order. A referenced spawn profile leads
// (it is itself a bundle of launch settings); the five inline fields follow.
function roleLaunchRows(rl) {
  const rows = [];
  if (rl.spawn_profile) rows.push(['profile', '⚙ ' + rl.spawn_profile]);
  if (rl.harness) rows.push(['harness', rl.harness]);
  if (rl.model) rows.push(['model', rl.model]);
  if (rl.effort) rows.push(['effort', rl.effort]);
  if (rl.sandbox) rows.push(['sandbox', rl.sandbox]);
  if (rl.approval) rows.push(['approval', rl.approval]);
  return rows;
}

// roleInspectHTML renders the inspect panel for `rl` (a roleJSON, or a
// "⚠ missing" marker for a dangling reference). Returns '' for no role so the
// caller can hide the container. `opts.missing` renders the dangling-reference
// note instead of a definition; `opts.deployNote` (default true) appends the
// deploy-time-resolution footer.
function roleInspectHTML(rl, opts = {}) {
  if (opts.missing) {
    return `<div class="role-inspect role-inspect-missing">⚠ This role is no longer in the library. `
      + `A referencing agent falls back to its own inline overrides at deploy.</div>`;
  }
  if (!rl) return '';

  const sections = [];

  if (rl.descr) {
    sections.push(`<div class="role-inspect-descr">${esc(rl.descr)}</div>`);
  }

  const launch = roleLaunchRows(rl);
  if (launch.length) {
    const chips = launch
      .map(([k, v]) => `<span class="role-inspect-chip"><b>${esc(k)}</b> ${esc(v)}</span>`)
      .join('');
    sections.push(`<div class="role-inspect-row"><span class="role-inspect-key">launch</span>`
      + `<span class="role-inspect-vals">${chips}</span></div>`);
  } else {
    sections.push(`<div class="role-inspect-row"><span class="role-inspect-key">launch</span>`
      + `<span class="role-inspect-muted">inherits (no defaults set)</span></div>`);
  }

  const perms = rl.permissions || [];
  if (perms.length) {
    const chips = perms.map(p => `<span class="role-inspect-slug">${esc(p)}</span>`).join('');
    sections.push(`<div class="role-inspect-row"><span class="role-inspect-key">grants</span>`
      + `<span class="role-inspect-vals">${chips}</span></div>`);
  } else {
    sections.push(`<div class="role-inspect-row"><span class="role-inspect-key">grants</span>`
      + `<span class="role-inspect-muted">none</span></div>`);
  }

  const brief = (rl.brief || '').trim();
  if (brief) {
    // <details> gives a native expand with no JS wiring — the summary shows the
    // first line (the brief's gist), the body the whole thing.
    const firstLine = brief.split('\n')[0];
    sections.push(`<details class="role-inspect-brief">`
      + `<summary><span class="role-inspect-key">brief</span> ${esc(firstLine)}</summary>`
      + `<pre class="role-inspect-brieftext">${esc(brief)}</pre></details>`);
  } else {
    sections.push(`<div class="role-inspect-row"><span class="role-inspect-key">brief</span>`
      + `<span class="role-inspect-muted">none</span></div>`);
  }

  if (opts.deployNote !== false) {
    sections.push(`<div class="role-inspect-foot">Resolved at deploy — editing the role changes future deploys; `
      + `already-deployed agents are untouched.</div>`);
  }

  return `<div class="role-inspect">${sections.join('')}</div>`;
}

export { roleInspectHTML };
