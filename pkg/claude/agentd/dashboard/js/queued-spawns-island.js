// The Groups toolbar's ⏸ queued chip: present only while something is
// queued, it opens a popover listing every queued spawn across groups. The
// host rides next to "+ new group" (dock.js re-homes it into the dock head
// while the palette is open), so it is always beside the spawn controls.
import { h, render } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { QueuedSpawnRow, queuedSpawns } from './queued-spawns.js';
import { useDismissablePopover } from './hover-popover.js';

const html = htm.bind(h);

export function QueuedSpawnsChip({ snapshot }) {
  const rows = queuedSpawns(snapshot.value);
  const [open, setOpen] = useState(false);
  // Fixed-position anchor under the chip's right edge. The dock head clips
  // overflow, so an absolutely positioned popover would be cut off there.
  const [anchor, setAnchor] = useState(null);
  const root = useRef(null);
  const toggle = (event) => {
    if (open) {
      setOpen(false);
      return;
    }
    const rect = event.currentTarget.getBoundingClientRect();
    setAnchor({ top: rect.bottom + 6, right: Math.max(8, window.innerWidth - rect.right) });
    setOpen(true);
  };
  useDismissablePopover(root, open, () => setOpen(false));
  useEffect(() => {
    if (!rows.length) setOpen(false);
  }, [rows.length]);
  if (!rows.length) return null;
  return html`<span class="queued-spawns-chip-wrap" ref=${root}>
    <button type="button" id="queued-spawns-open" class=${`queued-spawns-chip${open ? ' open' : ''}`}
      aria-haspopup="dialog" aria-expanded=${open ? 'true' : 'false'}
      title="Queued agents — prepared spawns that have not started yet"
      onClick=${toggle}>
      ⏸ <span class="theme-copy-regular">queued</span><span class="theme-copy-wizard">dormant</span>
      <span class="queued-spawns-count">${rows.length}</span>
    </button>
    ${open && html`<div class="queued-spawns-popover" role="dialog" aria-label="Queued agents"
      style=${anchor ? { top: `${anchor.top}px`, right: `${anchor.right}px` } : undefined}
      onClick=${(event) => { if (event.target.closest('.queued-spawn-row')) setOpen(false); }}>
      <div class="queued-spawns-popover-head">
        <span><span class="theme-copy-regular">Queued agents</span><span class="theme-copy-wizard">Dormant summonings</span></span>
        <span class="queued-spawns-popover-hint">click = review/edit · ▶ = spawn now</span>
      </div>
      ${rows.map((entry) => html`<${QueuedSpawnRow} key=${entry.id} entry=${entry} />`)}
    </div>`}
  </span>`;
}

export function mountQueuedSpawnsIsland({ host, snapshot, registerCleanup }) {
  render(html`<${QueuedSpawnsChip} snapshot=${snapshot} />`, host);
  registerCleanup(() => render(null, host));
}
