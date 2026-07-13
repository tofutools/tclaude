import { h, render } from 'preact';
import { useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { trustedHTMLToVNodes } from './html-vnodes.js';
import { wizWord } from './slop.js';

const html = htm.bind(h);

function DockChips({ section, item }) {
  const chips = section.chips?.(item) || [];
  const markup = section.chipsHTML?.(item) || '';
  if (!chips.length && !markup) return null;
  return html`
    <span class="dock-chips">
      ${chips.map((chip) => html`
        <span
          key=${chip.text}
          class=${`dock-chip${chip.more ? ' dock-chip-more' : ''}`}
        >${chip.text}</span>
      `)}
      ${markup ? trustedHTMLToVNodes(markup) : null}
    </span>
  `;
}

function DockCard({ section, item, openMenu, setOpenMenu, clipHost, layoutVersion }) {
  const name = section.name(item);
  const menuKey = `${section.key}:${name}`;
  const menuOpen = openMenu === menuKey;
  const menuRef = useRef(null);
  const cogRef = useRef(null);
  const [opensUp, setOpensUp] = useState(false);
  const draggable = section.drag ? 'true' : 'false';
  const gripTitle = section.drag
    ? wizWord('drag onto a group to spawn', 'drag onto a party to summon')
    : wizWord('drag onto a group (coming soon)', 'drag onto a party (coming soon)');

  useLayoutEffect(() => {
    if (!menuOpen || !menuRef.current || !cogRef.current) {
      setOpensUp(false);
      return;
    }
    const clip = clipHost?.getBoundingClientRect() || { top: 0, bottom: window.innerHeight };
    const menuRect = menuRef.current.getBoundingClientRect();
    const cogTop = cogRef.current.getBoundingClientRect().top;
    setOpensUp(menuRect.bottom > clip.bottom && menuRect.height < cogTop - clip.top);
  }, [menuOpen, clipHost, layoutVersion]);

  const run = (event, action) => {
    event.preventDefault();
    setOpenMenu(null);
    if (action === 'edit') section.onManageItem(item);
    else if (action === 'clone') section.onCloneItem(item);
    else section.onDeleteItem(item);
  };

  return html`
    <div
      class="dock-card"
      draggable=${draggable}
      data-key=${name}
      data-dock-kind=${section.key}
      data-dock-name=${name}
      title=${name}
    >
      <span class="dock-grip" aria-hidden="true" title=${gripTitle}>⠿</span>
      <span class="dock-card-icon" aria-hidden="true">${section.icon}</span>
      <span class="dock-card-body">
        <span class="dock-card-name">${name}</span>
        <${DockChips} section=${section} item=${item} />
      </span>
      <span class="dock-card-actions">
        <button
          ref=${cogRef}
          type="button"
          class="dock-card-manage"
          data-dock-act="card-menu"
          data-dock-kind=${section.key}
          data-dock-name=${name}
          aria-haspopup="menu"
          aria-expanded=${menuOpen ? 'true' : 'false'}
          title="More actions"
          aria-label=${`Actions for ${name}`}
          onClick=${(event) => {
            event.preventDefault();
            setOpenMenu(menuOpen ? null : menuKey);
          }}
        >⚙</button>
        <div
          ref=${menuRef}
          class=${`dock-card-menu${menuOpen ? ' open' : ''}${opensUp ? ' opens-up' : ''}`}
          role="menu"
          aria-label=${name}
        >
          <button
            type="button"
            role="menuitem"
            class="dock-card-menu-item"
            data-dock-act="edit-item"
            data-dock-kind=${section.key}
            data-dock-name=${name}
            onClick=${(event) => run(event, 'edit')}
          >Edit</button>
          <button
            type="button"
            role="menuitem"
            class="dock-card-menu-item"
            data-dock-act="clone-item"
            data-dock-kind=${section.key}
            data-dock-name=${name}
            onClick=${(event) => run(event, 'clone')}
          >${wizWord('Clone', 'Mirror')}</button>
          <button
            type="button"
            role="menuitem"
            class="dock-card-menu-item danger"
            data-dock-act="delete-item"
            data-dock-kind=${section.key}
            data-dock-name=${name}
            onClick=${(event) => run(event, 'delete')}
          >${wizWord('Delete', 'Dispel')}</button>
        </div>
      </span>
    </div>
  `;
}

function DockSection({
  section, snapshot, openMenu, setOpenMenu, clipHost,
  isSectionOpen, setSectionOpen,
}) {
  const [open, setOpen] = useState(() => isSectionOpen(section.key));
  const items = section.items(snapshot);

  return html`
    <details
      class="dock-section"
      data-key=${section.key}
      open=${open}
      onToggle=${(event) => {
        const next = event.currentTarget.hasAttribute('open');
        setOpen(next);
        setSectionOpen(section.key, next);
      }}
    >
      <summary class="dock-section-head">
        <span class="dock-section-title">
          <span class="dock-section-chevron" aria-hidden="true">▸</span>
          <span class="dock-section-icon" aria-hidden="true">${section.icon}</span>
          ${section.title()}
          <span class="dock-section-count">${items.length}</span>
        </span>
        <button
          type="button"
          class="dock-section-manage"
          data-dock-act="manage-all"
          data-dock-kind=${section.key}
          title="Open the manager for this kind"
          onClick=${(event) => {
            event.preventDefault();
            section.onManageAll();
          }}
        >⧉</button>
      </summary>
      <div class="dock-section-items">
        ${items.length ? items.map((item) => html`
          <${DockCard}
            key=${section.name(item)}
            section=${section}
            item=${item}
            openMenu=${openMenu}
            setOpenMenu=${setOpenMenu}
            clipHost=${clipHost}
            layoutVersion=${snapshot}
          />
        `) : html`<div class="dock-empty">(${section.empty()})</div>`}
      </div>
    </details>
  `;
}

export function Dock({ host, state, sections, isSectionOpen, setSectionOpen }) {
  const [openMenu, setOpenMenu] = useState(null);
  const snapshot = state.snapshot.value;

  useEffect(() => {
    const close = (restoreFocus = false) => {
      const menu = host.querySelector('.dock-card-menu.open');
      if (!menu) return;
      const focusInside = menu.contains(document.activeElement);
      const cog = menu.parentElement?.querySelector('.dock-card-manage');
      setOpenMenu(null);
      if (restoreFocus && focusInside) queueMicrotask(() => cog?.focus());
    };
    const onClick = (event) => {
      if (!event.target.closest('.dock-card-actions')) close(true);
    };
    const onKeyDown = (event) => {
      if (event.key !== 'Escape' || !host.querySelector('.dock-card-menu.open')) return;
      event.preventDefault();
      close(true);
    };
    document.addEventListener('click', onClick);
    document.addEventListener('keydown', onKeyDown);
    return () => {
      document.removeEventListener('click', onClick);
      document.removeEventListener('keydown', onKeyDown);
    };
  }, [host]);

  return sections.map((section) => html`
    <${DockSection}
      key=${section.key}
      section=${section}
      snapshot=${snapshot}
      openMenu=${openMenu}
      setOpenMenu=${setOpenMenu}
      clipHost=${host}
      isSectionOpen=${isSectionOpen}
      setSectionOpen=${setSectionOpen}
    />
  `);
}

export function mountDockIsland({
  host, state, sections, isSectionOpen, setSectionOpen, registerCleanup,
}) {
  render(html`
    <${Dock}
      host=${host}
      state=${state}
      sections=${sections}
      isSectionOpen=${isSectionOpen}
      setSectionOpen=${setSectionOpen}
    />
  `, host);
  registerCleanup(() => render(null, host));
}
