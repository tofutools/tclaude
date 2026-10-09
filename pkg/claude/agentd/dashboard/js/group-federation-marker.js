import { h } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { relTime } from './helpers.js';

const html = htm.bind(h);

// openNodeView is loaded on demand so the Groups island never depends on the
// separately mounted Skynet modules.
function openNodeView(id) {
  void import('./skynet-island.js').then((m) => m.openNodeView(id)).catch(() => {});
}

// group-federation-marker.js — the Groups-tab marker for a group linked to
// federation peers (snapshot field federation_links, see
// dashboard_group_federation_links.go). The marker only reports links; grants
// and routes are still managed in Fleet administration and the federation CLI.

// linkView describes one link for the popover in operator terms: who, how the
// link exists, and what it lets the peer (or this group) do.
export function linkView(link) {
  const name = link.label || String(link.peer || '').slice(0, 13) || 'peer';
  let how;
  let can;
  if (link.kind === 'route') {
    how = `route → ${link.remote || 'remote group'}`;
    can = 'members message the remote group through a private mirror';
  } else {
    how = link.pool ? `pool grant (${link.pool})` : 'direct grant';
    can = link.slugs?.length ? `peer can: ${link.slugs.join(', ')}` : 'peer grant';
  }
  const live = !!link.online;
  const state = live ? 'live' : link.last_seen ? `offline · seen ${relTime(link.last_seen)}` : 'offline';
  return { name, direction: link.direction === 'out' ? 'out' : 'in', how, can, live, state, unrestricted: link.level === 'unrestricted' };
}

// markerView reduces a group's links to the header chip: the distinct nodes,
// and whether any of them is reachable right now.
export function markerView(links) {
  const list = Array.isArray(links) ? links : [];
  if (!list.length) return null;
  const nodes = [...new Set(list.map((link) => link.label || link.peer))];
  const live = list.some((link) => link.online);
  const title = `Linked to ${nodes.length} federation node${nodes.length === 1 ? '' : 's'}: ${nodes.join(', ')}${live ? '' : ' (all offline)'} — click for details`;
  return { nodes, live, title };
}

export function GroupFederationMarker({ group, openNode = openNodeView }) {
  const view = markerView(group.federation_links);
  const [open, setOpen] = useState(false);
  const rootRef = useRef(null);
  useEffect(() => {
    if (!open) return undefined;
    const onDown = (event) => { if (!rootRef.current?.contains(event.target)) setOpen(false); };
    const onKey = (event) => { if (event.key === 'Escape') setOpen(false); };
    document.addEventListener('mousedown', onDown);
    document.addEventListener('keydown', onKey);
    return () => { document.removeEventListener('mousedown', onDown); document.removeEventListener('keydown', onKey); };
  }, [open]);
  if (!view) return null;
  // The marker sits inside the group's <summary>: swallow clicks so opening
  // the popover never toggles the group open or closed.
  const swallow = (event) => { event.preventDefault(); event.stopPropagation(); };
  return html`<span ref=${rootRef} class="group-federation-marker" onClick=${swallow}>
    <button type="button" class=${`group-federation-chip${view.live ? ' live' : ''}`}
      aria-haspopup="dialog" aria-expanded=${open ? 'true' : 'false'} title=${view.title}
      onClick=${(event) => { swallow(event); setOpen(!open); }}
      onKeyDown=${(event) => { if (event.key === ' ') event.stopPropagation(); }}
    >🌐<span class="gfm-dot" aria-hidden="true"></span></button>
    ${open ? html`<div class="group-federation-pop" role="dialog" aria-label=${`Federation links of ${group.name}`}>
      <div class="gfm-head">Federation links · ${group.name}</div>
      ${group.federation_links.map((link, i) => {
        const v = linkView(link);
        return html`<div key=${i} class="gfm-row">
          <span class=${`gfm-state${v.live ? ' live' : ''}`} title=${v.state}></span>
          <span class="gfm-main">
            <span class="gfm-target">${v.direction === 'out' ? '→' : '←'} ${v.name}${v.unrestricted ? html` <span class="gfm-tag">unrestricted</span>` : null}</span>
            <span class="gfm-how">${v.how} · ${v.state}</span>
            <span class="gfm-can">${v.can}</span>
          </span>
          <button type="button" class="gfm-open" title=${`Open ${v.name}'s dashboard`}
            onClick=${(event) => { swallow(event); setOpen(false); openNode(link.peer); }}>open node</button>
        </div>`;
      })}
      <div class="gfm-foot">Manage grants and routes in Fleet administration or <code>tclaude federation</code>.</div>
    </div>` : null}
  </span>`;
}
