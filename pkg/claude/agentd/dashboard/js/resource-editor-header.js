// resource-editor-header.js -- the one header every resource editor wears
// (tk_034e2afc): the drawing board first, then the process editor and pages.
//
//   identity   title, lock chip, status (saved / synced / modified)
//   menu bar   File · Edit · View · Share, each filled by the editor; a menu
//              with no items is left out (Edit only where it adds more than
//              its shortcuts)
//   people     optional, at the right: who is on the resource, which opens
//              Share
//
// A new action goes into a named menu instead of another button on the row.
// The host's moves (Collab tab, overlay, separate window) join View in one
// wording for every editor: editorMoveItems.
//
// A menu item is { key, label, run, title?, shortcut?, disabled?, danger?,
// checked? }, a { sep: true }, a { heading }, a { node } drawn as is (a row
// of people, a picker), or { key, label, children } that opens its children
// in the menu's place, with a way back.

import { h } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { useDismissablePopover } from './hover-popover.js';
import { Menu, MenuItem, MenuSep } from './ui-components.js';
import { TO_COLLAB_LABEL } from './overlay-window-controls.js';

const html = htm.bind(h);

// The moves' words, the same in every editor and every host.
export const TO_OVERLAY_LABEL = 'Open in overlay';
export const TO_WINDOW_LABEL = 'Open in separate window';
export { TO_COLLAB_LABEL };

// editorMoveItems are View's moves for a host: toCollab, toOverlay and
// toWindow are the host's ways there (null where it cannot go), each
// answering the editor's kind for the tooltip ("this drawing board").
export function editorMoveItems({ what = 'this editor', toCollab = null, toOverlay = null, toWindow = null } = {}) {
  const items = [];
  if (toCollab) items.push({ key: 'to-collab', label: TO_COLLAB_LABEL, title: `Move ${what} back to a tab of the dashboard`, run: toCollab });
  if (toOverlay) items.push({ key: 'to-overlay', label: TO_OVERLAY_LABEL, title: `Float ${what} in a window over the dashboard`, run: toOverlay });
  if (toWindow) items.push({ key: 'to-window', label: TO_WINDOW_LABEL, title: `Move ${what} to a browser window of its own`, run: toWindow });
  return items;
}

// shown drops what draws nothing: a separator or heading with nothing after
// it before the next one, and a leading separator.
function shown(items) {
  const out = [];
  for (const item of items || []) {
    if (!item) continue;
    if (item.sep && (!out.length || out.at(-1).sep)) continue;
    out.push(item);
  }
  while (out.length && (out.at(-1).sep || out.at(-1).heading)) out.pop();
  return out.filter((item, i) => !item.heading || (out[i + 1] && !out[i + 1].heading && !out[i + 1].sep));
}

function itemsOf(menu) {
  return shown(typeof menu.items === 'function' ? menu.items() : menu.items);
}

// EditorMenuBar is the row of menus: one open at a time; while one is open,
// hovering another label opens that one; ←/→ move along the bar, ↓ into the
// open menu, Escape closes it. roots are more refs that count as the bar's own
// for a press (the people stack, which opens Share), so a press there does
// not close a menu its click then toggles.
export function EditorMenuBar({ menus, label = 'Editor menu', open: openKey, onOpen, roots = [] }) {
  const [own, setOwn] = useState('');
  const open = openKey ?? own;
  const setOpen = onOpen || setOwn;
  const [sub, setSub] = useState(null);
  const ref = useRef(null);
  const close = () => { setOpen(''); setSub(null); };
  // Escape is handled below, after a control in the menu (the group
  // picker's list) has had it: its Escape closes its own list first.
  useDismissablePopover([ref, ...roots], open, (why) => { if (why !== 'away') close(); }, { escape: false });
  const live = menus.filter((menu) => itemsOf(menu).length > 0);
  const labels = () => [...(ref.current?.querySelectorAll('.reh-menubar-label') || [])];
  const latest = useRef(null);
  latest.current = { open, live, close, labels };
  useEffect(() => {
    if (!open) return undefined;
    const doc = ref.current?.ownerDocument;
    const onKey = (event) => {
      if (event.key !== 'Escape' || event.defaultPrevented) return;
      const { open: now, live: menus, close: shut, labels: all } = latest.current;
      const i = menus.findIndex((menu) => menu.key === now);
      const inside = ref.current?.contains(event.target);
      event.preventDefault();
      shut();
      if (inside) all()[i]?.focus();
    };
    doc?.addEventListener('keydown', onKey);
    return () => doc?.removeEventListener('keydown', onKey);
  }, [!!open]);
  const show = (key) => { setSub(null); setOpen(key); };
  const onLabelKey = (event, i) => {
    const step = event.key === 'ArrowRight' ? 1 : event.key === 'ArrowLeft' ? -1 : 0;
    if (step) {
      event.preventDefault();
      const j = (i + step + live.length) % live.length;
      labels()[j]?.focus();
      if (open) show(live[j].key);
    } else if (event.key === 'ArrowDown') {
      event.preventDefault();
      show(live[i].key);
      setTimeout(() => ref.current?.querySelector('.reh-menu [role="menuitem"]:not([disabled])')?.focus());
    }
  };
  const onMenuKey = (event) => {
    // A control drawn in the menu (a picker) keeps its own keys.
    if (event.target?.closest?.('.reh-menu-node')) return;
    const items = [...(ref.current?.querySelectorAll('.reh-menu [role="menuitem"]:not([disabled])') || [])];
    const at = items.indexOf(event.target);
    if ((event.key === 'ArrowDown' || event.key === 'ArrowUp') && items.length) {
      event.preventDefault();
      items[(at + (event.key === 'ArrowDown' ? 1 : -1) + items.length) % items.length]?.focus();
    }
  };
  const pick = (item) => {
    if (item.back) return setSub(null);
    if (item.children) return setSub({ ...item, children: typeof item.children === 'function' ? item.children() : item.children });
    close();
    void item.run?.();
  };
  const drawItem = (item, i) => {
    if (item.sep) return html`<${MenuSep} key=${`sep-${i}`} />`;
    if (item.heading) return html`<div key=${`h-${i}`} class="reh-menu-heading ui-eyebrow" role="presentation">${item.heading}</div>`;
    if (item.node) return html`<div key=${item.key || `n-${i}`} class="reh-menu-node" role="presentation">${item.node}</div>`;
    return html`<${MenuItem} key=${item.key || item.label} danger=${item.danger} disabled=${item.disabled} title=${item.title || undefined}
        aria-checked=${item.checked === undefined ? undefined : String(!!item.checked)} role=${item.checked === undefined ? 'menuitem' : 'menuitemcheckbox'}
        data-editor-menu-item=${item.key || item.label} onClick=${() => pick(item)}>
      <span class="reh-menu-label">${item.checked ? '✓ ' : ''}${item.children ? `${item.label} ›` : item.label}</span>
      ${item.shortcut && html`<span class="reh-menu-key">${item.shortcut}</span>`}
    <//>`;
  };
  const current = live.find((menu) => menu.key === open);
  const items = current ? (sub ? [{ label: '‹ Back', key: 'back', back: true }, ...shown(sub.children)] : itemsOf(current)) : [];
  if (!live.length) return null;
  return html`<nav class="reh-menubar" role="menubar" aria-label=${label} ref=${ref}>
    ${live.map((menu, i) => html`<span key=${menu.key} class="reh-menubar-slot">
      <button data-ui-exempt type="button" role="menuitem" class=${`reh-menubar-label${open === menu.key ? ' open' : ''}`}
        aria-haspopup="menu" aria-expanded=${open === menu.key ? 'true' : 'false'} data-editor-menu=${menu.key} title=${menu.title || undefined}
        onClick=${() => (open === menu.key ? close() : show(menu.key))}
        onPointerEnter=${() => { if (open && open !== menu.key) show(menu.key); }}
        onKeyDown=${(event) => onLabelKey(event, i)}>${menu.label}</button>
      ${open === menu.key && html`<${Menu} size="sm" class="reh-menu" aria-label=${menu.label} onKeyDown=${onMenuKey}>
        ${items.map(drawItem)}
      <//>`}
    </span>`)}
  </nav>`;
}

// ResourceEditorHeader lays the header out: identity, the menu bar, then
// people at the right. people is drawn as given; openMenu/onOpenMenu let the
// people stack open Share.
export function ResourceEditorHeader({ title, identity = null, menus = [], people = null, label = 'Editor menu', openMenu, onOpenMenu, class: extra = '' }) {
  const peopleRef = useRef(null);
  return html`<header class=${`reh ${extra}`.trim()}>
    <div class="reh-identity">${title}${identity}</div>
    <${EditorMenuBar} menus=${menus} label=${label} open=${openMenu} onOpen=${onOpenMenu} roots=${[peopleRef]} />
    <span class="reh-spacer"></span>
    ${people && html`<div class="reh-people" ref=${peopleRef}>${people}</div>`}
  </header>`;
}

// PeopleStack is the people slot: up to max initials, then +n, with the full
// list in the tooltip. onClick opens the editor's Share.
export function PeopleStack({ people = [], max = 4, onClick = null, label = 'People on this' }) {
  if (!people.length && !onClick) return null;
  const names = people.map((p) => `${p.name}${p.note ? ` (${p.note})` : ''}`).join(', ');
  return html`<button data-ui-exempt type="button" class="reh-stack" aria-label=${`${label}: ${people.length ? names : 'nobody yet'}`}
      title=${people.length ? names : 'Nobody yet: share it'} onClick=${onClick || undefined}>
    <span class="reh-avatars" aria-hidden="true">${people.slice(0, max).map((p) => html`<span key=${p.id}
      class=${`reh-avatar${p.offline ? ' offline' : ''}${p.state ? ` ${p.state}` : ''}`}>${(p.name || '?').trim().charAt(0).toUpperCase() || '?'}</span>`)}</span>
    <span class="reh-stack-count">${people.length > max ? `+${people.length - max}` : people.length || '＋'}</span>
  </button>`;
}
