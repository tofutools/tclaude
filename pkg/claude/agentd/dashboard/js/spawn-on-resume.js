// spawn-on-resume.js — the spawn dialog's "On resume" row (launch slices 10
// and 11): a one-line summary of what a later resume or reincarnation of the
// new agent re-reads and keeps, and Edit…, which opens the per-agent editor
// (relaunch-policy-dialog.js) over this spawn's own rows.
//
// The row resolves nothing itself. GET /api/relaunch-policy?preview=1 gives
// the global table and, for the dialog's group and profile, what each row
// comes to without a choice of this spawn's (the daemon walks the same
// profile tiers the spawn does). The operator's own rows are laid over that.
import { h } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { Button } from './ui-components.js';
import { relaunchRowsSummary } from './relaunch-policy-dialog.js';

const html = htm.bind(h);

export function relaunchPreviewURL(group, profile) {
  const q = new URLSearchParams({ preview: '1' });
  if (group) q.set('group', group);
  if (profile) q.set('profile', profile);
  return `/api/relaunch-policy?${q}`;
}

export function SpawnOnResumeRow({ fetchImpl = globalThis.fetch, busy = false, group = '', profile = '', selection = {}, onEdit }) {
  const [data, setData] = useState(null);
  const [failed, setFailed] = useState(false);

  useEffect(() => {
    let live = true;
    (async () => {
      try {
        const response = await fetchImpl(relaunchPreviewURL(group, profile), { credentials: 'same-origin' });
        const body = await response.json().catch(() => null);
        if (!response.ok || !body) throw new Error(`HTTP ${response.status}`);
        if (live) { setData(body); setFailed(false); }
      } catch { if (live) setFailed(true); }
    })();
    return () => { live = false; };
  }, [fetchImpl, group, profile]);

  // The row is information first: when the policy cannot be read it says
  // nothing rather than guessing.
  if (failed || !data) return null;
  const rows = (Array.isArray(data.rows) ? data.rows : []).filter((row) => row.settable !== false);
  const fields = Array.isArray(data.fields) ? data.fields : [];
  const inherited = data.resolved || {};
  const own = Object.keys(selection || {}).length;

  return html`<div class="cron-create-row" id="agent-spawn-on-resume-row">
    <span class="cron-create-label">On resume</span>
    <div class="cron-create-target spawn-on-resume">
      <div class="spawn-on-resume-head">
        <div>
          <div><strong>Relaunch policy</strong>: what a resume or reincarnation of this agent re-reads, and what it keeps${
            own ? html` · <span class="ui-tag ui-tag-accent" data-on-resume-own>${own} set for this spawn</span>` : ''}.</div>
          <div class="ui-hint" data-on-resume-summary>${relaunchRowsSummary(rows, selection, inherited, 'this spawn')}</div>
        </div>
        <${Button} size="sm" id="agent-spawn-on-resume-edit" disabled=${busy || !onEdit}
          onClick=${() => onEdit?.({ rows, fields, inherited })}>Edit…<//>
      </div>
    </div>
  </div>`;
}

// ProfileOnResumeRow is the profile editor's twin: the profile's own rows over
// the global table (a profile's rows are copied into each agent it spawns),
// and Edit…, which opens the same editor in profile scope.
export function ProfileOnResumeRow({ fetchImpl = globalThis.fetch, selection = {}, onEdit }) {
  const [data, setData] = useState(null);
  useEffect(() => {
    let live = true;
    (async () => {
      try {
        const response = await fetchImpl('/api/relaunch-policy', { credentials: 'same-origin' });
        const body = await response.json().catch(() => null);
        if (response.ok && body && live) setData(body);
      } catch { /* the row says nothing */ }
    })();
    return () => { live = false; };
  }, [fetchImpl]);
  if (!data) return null;
  const rows = (Array.isArray(data.rows) ? data.rows : []).filter((row) => row.settable !== false);
  const fields = Array.isArray(data.fields) ? data.fields : [];
  return html`<div class="cron-create-row" id="profile-editor-on-resume-row"
    title="What a later resume or reincarnation of an agent spawned from this profile re-reads and keeps. Copied into each agent at spawn; a row the profile leaves out follows the global table (Config › Launch).">
    <span class="cron-create-label">On resume</span>
    <${Button} id="profile-editor-on-resume-edit" disabled=${!onEdit} onClick=${() => onEdit?.({ rows, fields })}>Edit…<//>
    <span data-on-resume-summary>${relaunchRowsSummary(rows, selection, {}, 'this profile')}</span>
  </div>`;
}
