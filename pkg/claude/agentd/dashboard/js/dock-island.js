import { h, render } from 'preact';
import {
  useCallback, useEffect, useId, useLayoutEffect, useRef, useState,
} from 'preact/hooks';
import htm from 'htm';
import { wizWord } from './slop.js';
import { DockProcesses } from './dock-processes.js';
import { dashPrefs } from './prefs.js';
import {
  LIBRARY_EVENT, menuPlacement, filterSection, leafTexts, orderPinned, pinnedNames, searchTerms, togglePin,
} from './dock-library.js';

const html = htm.bind(h);

function DockChips({ chips = [], full = false }) {
  if (!chips.length) return null;
  return html`
    <span
      class=${`dock-chips ${full ? 'dock-chips-full' : 'dock-chips-compact'}`}
      aria-label=${full ? 'All aliases and settings' : null}
    >
      ${chips.map((chip) => html`
        <span
          key=${chip.key || chip.text}
          class=${chip.className || `dock-chip${chip.more ? ' dock-chip-more' : ''}`}
          title=${chip.title || null}
        >${chip.text}</span>
      `)}
    </span>
  `;
}

// copyName puts a name on the clipboard for a CLI flag or a config file.
export async function copyName(name, notify) {
  try {
    await navigator.clipboard.writeText(name);
    notify?.(`copied ${name}`);
  } catch {
    notify?.('copy failed: the clipboard is not available here', true);
  }
}

// DockMenuItem is one row of a card or section menu.
export function DockMenuItem({ act, kind, name, danger = false, onRun, setOpenMenu, children }) {
  return html`<button
    type="button"
    role="menuitem"
    class=${`dock-card-menu-item${danger ? ' danger' : ''}`}
    data-dock-act=${act}
    data-dock-kind=${kind}
    data-dock-name=${name}
    onClick=${(event) => {
      event.preventDefault();
      setOpenMenu(null);
      onRun();
    }}
  >${children}</button>`;
}

function DockCard({
  section, item, openMenu, setOpenMenu, clipHost, layoutVersion, snapshot, pinned, onTogglePin, notify,
}) {
  const name = section.name(item);
  const isDefault = !!section.isDefault?.(item, snapshot);
  const compactChips = [
    ...(isDefault ? [{ key: 'is-default', text: wizWord('default', 'bound'), className: 'dock-chip dock-chip-default', title: 'The global default' }] : []),
    ...(section.chips?.(item) || []),
  ];
  const fullChips = section.fullChips?.(item) || [];
  const hasDetails = fullChips.length > 0;
  const menuKey = `${section.key}:${name}`;
  const menuOpen = openMenu === menuKey;
  const menuRef = useRef(null);
  const cogRef = useRef(null);
  const cardRef = useRef(null);
  const detailsRef = useRef(null);
  const hoveringRef = useRef(false);
  const [placement, setPlacement] = useState({ up: false, maxHeight: null });
  const [detailsOpen, setDetailsOpen] = useState(false);
  const [dragging, setDragging] = useState(false);
  const [detailsPosition, setDetailsPosition] = useState(null);
  const draggable = section.drag ? 'true' : 'false';
  const detailsID = `${useId()}-dock-details`;
  const detailsDescriptionID = `${detailsID}-description`;
  const gripTitle = section.drag
    ? wizWord('drag onto a group to spawn', 'drag onto a party to summon')
    : wizWord('drag onto a group (coming soon)', 'drag onto a party (coming soon)');

  useLayoutEffect(() => {
    if (!menuOpen || !menuRef.current || !cogRef.current) {
      setPlacement({ up: false, maxHeight: null });
      return;
    }
    const clip = clipHost?.getBoundingClientRect() || { top: 0, bottom: window.innerHeight };
    const cog = cogRef.current.getBoundingClientRect();
    const height = menuRef.current.scrollHeight || menuRef.current.getBoundingClientRect().height;
    setPlacement(menuPlacement({ clipTop: clip.top, clipBottom: clip.bottom, cogTop: cog.top, cogBottom: cog.bottom, height }));
  }, [menuOpen, clipHost, layoutVersion]);

  const positionDetails = useCallback(() => {
    if (!cardRef.current || !detailsRef.current) return;
    const clip = clipHost?.getBoundingClientRect() || { top: 0, bottom: window.innerHeight };
    const cardRect = cardRef.current.getBoundingClientRect();
    const detailsRect = detailsRef.current.getBoundingClientRect();
    const detailsHeight = detailsRef.current.scrollHeight || detailsRect.height;
    const availableHeight = Math.max(0, clip.bottom - clip.top);
    const visibleHeight = Math.min(detailsHeight, availableHeight);
    const top = Math.min(
      Math.max(cardRect.top, clip.top),
      Math.max(clip.top, clip.bottom - visibleHeight),
    );
    const left = Math.max(8, cardRect.left - cardRect.width);
    setDetailsPosition({
      left,
      width: Math.max(0, cardRect.left - left),
      top,
      bottom: 'auto',
      maxHeight: availableHeight,
    });
  }, [clipHost]);

  useLayoutEffect(() => {
    if (detailsOpen) positionDetails();
  }, [detailsOpen, detailsPosition?.width, fullChips.length, layoutVersion, positionDetails]);

  useEffect(() => {
    if (!detailsOpen) return undefined;
    clipHost?.addEventListener('scroll', positionDetails, { passive: true });
    window.addEventListener('scroll', positionDetails);
    window.addEventListener('resize', positionDetails);
    return () => {
      clipHost?.removeEventListener('scroll', positionDetails);
      window.removeEventListener('scroll', positionDetails);
      window.removeEventListener('resize', positionDetails);
    };
  }, [clipHost, detailsOpen, positionDetails]);

  const showDetails = () => {
    if (dragging) return;
    positionDetails();
    setDetailsOpen(true);
  };

  const hideHoveredDetails = () => {
    if (!cardRef.current?.contains(document.activeElement)) setDetailsOpen(false);
  };

  const enterCard = () => {
    hoveringRef.current = true;
    showDetails();
  };

  const leaveCard = () => {
    hoveringRef.current = false;
    hideHoveredDetails();
  };

  // Every kind's menu, in one order; a kind without a hook leaves its row out.
  const menuItem = (act, label, onRun, danger = false) => html`<${DockMenuItem} key=${act} act=${act}
    kind=${section.key} name=${name} danger=${danger} onRun=${onRun} setOpenMenu=${setOpenMenu}>${label}</${DockMenuItem}>`;
  const menuItems = [
    menuItem('edit-item', 'Edit', () => section.onManageItem(item)),
    menuItem('clone-item', wizWord('Clone', 'Mirror'), () => section.onCloneItem(item)),
    section.onRenameItem && menuItem('rename-item', 'Rename…', () => section.onRenameItem(item)),
    ...(section.extraActions || []).map((extra) => menuItem(extra.act, extra.label(), () => extra.run(item))),
    section.onToggleDefault && menuItem('default-item', isDefault ? 'Clear default' : 'Set as default', () => section.onToggleDefault(item, isDefault)),
    menuItem('pin-item', pinned ? 'Unpin' : 'Pin to top', () => onTogglePin(name)),
    menuItem('copy-item', 'Copy name', () => copyName(name, notify)),
    section.onExportItem && menuItem('export-item', 'Export', () => section.onExportItem(item)),
    menuItem('delete-item', wizWord('Delete', 'Dispel'), () => section.onDeleteItem(item), true),
  ].filter(Boolean);

  return html`
    <div
      ref=${cardRef}
      class=${`dock-card${hasDetails ? ' dock-card-has-details' : ''}`}
      draggable=${draggable}
      data-key=${name}
      data-dock-kind=${section.key}
      data-dock-name=${name}
      title=${hasDetails ? null : name}
      onMouseEnter=${hasDetails ? enterCard : null}
      onMouseLeave=${hasDetails ? leaveCard : null}
      onFocusIn=${hasDetails ? showDetails : null}
      onFocusOut=${hasDetails ? (event) => {
        if (!event.currentTarget.contains(event.relatedTarget)) setDetailsOpen(false);
      } : null}
      onDragStart=${section.drag ? () => {
        setDragging(true);
        setDetailsOpen(false);
      } : null}
      onDragEnter=${section.drag ? () => { hoveringRef.current = true; } : null}
      onDragLeave=${section.drag ? (event) => {
        if (!event.currentTarget.contains(event.relatedTarget)) hoveringRef.current = false;
      } : null}
      onDragEnd=${section.drag ? (event) => {
        setDragging(false);
        const hasPointerCoordinates = Number.isFinite(event.clientX)
          && Number.isFinite(event.clientY)
          && (event.clientX !== 0 || event.clientY !== 0);
        const hit = hasPointerCoordinates && document.elementFromPoint
          ? document.elementFromPoint(event.clientX, event.clientY)
          : null;
        const endedOverCard = hit ? event.currentTarget.contains(hit) : hoveringRef.current;
        hoveringRef.current = endedOverCard;
        if (endedOverCard && hasDetails) {
          positionDetails();
          setDetailsOpen(true);
        }
      } : null}
    >
      <span class="dock-grip" aria-hidden="true" title=${hasDetails ? null : gripTitle}>⠿</span>
      <span class="dock-card-body">
        <span class="dock-card-name">${pinned ? html`<span class="dock-card-pin" title="Pinned to the top" aria-label="Pinned">📌</span>` : null}${name}</span>
        <${DockChips} chips=${compactChips} />
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
          aria-describedby=${hasDetails ? detailsDescriptionID : null}
          title=${hasDetails ? null : 'More actions'}
          aria-label=${`Actions for ${name}`}
          onClick=${(event) => {
            event.preventDefault();
            setOpenMenu(menuOpen ? null : menuKey);
          }}
        >⚙</button>
        <div
          ref=${menuRef}
          class=${`dock-card-menu${menuOpen ? ' open' : ''}${placement.up ? ' opens-up' : ''}`}
          style=${placement.maxHeight != null ? `max-height:${placement.maxHeight}px;overflow-y:auto` : null}
          role="menu"
          aria-label=${name}
        >
          ${menuItems}
        </div>
      </span>
      ${hasDetails ? html`
        <div
          ref=${detailsRef}
          id=${detailsID}
          role="region"
          aria-label=${`Full details for ${name}`}
          aria-describedby=${detailsDescriptionID}
          tabIndex="0"
          draggable="false"
          style=${detailsPosition}
          class=${`dock-card-details${detailsOpen && !menuOpen && !dragging ? ' open' : ''}`}
        >
          <span id=${detailsDescriptionID} class="dock-card-details-description">
            ${fullChips.map((chip) => chip.text).join(', ')}
          </span>
          <span class="dock-card-details-name">${name}</span>
          <${DockChips} chips=${fullChips} full />
        </div>
      ` : null}
    </div>
  `;
}

// useSectionItems is a section's items: off the snapshot, or for a section
// that loads its own (sandbox profiles) fetched while it is shown and again
// when the snapshot's version for it changes or LIBRARY_EVENT fires.
function useSectionItems(section, snapshot, wanted) {
  const version = section.load ? section.load.version(snapshot) : null;
  const [loaded, setLoaded] = useState({ items: null, error: '' });
  const [nonce, setNonce] = useState(0);
  useEffect(() => {
    if (!section.load) return undefined;
    const bump = () => setNonce((value) => value + 1);
    document.addEventListener(LIBRARY_EVENT, bump);
    return () => document.removeEventListener(LIBRARY_EVENT, bump);
  }, [section]);
  useEffect(() => {
    if (!section.load || !wanted) return undefined;
    let live = true;
    section.load.fetch()
      .then((items) => { if (live) setLoaded({ items, error: '' }); })
      .catch((cause) => { if (live) setLoaded((value) => ({ ...value, error: cause?.message || String(cause) })); });
    return () => { live = false; };
  }, [section, wanted, version, nonce]);
  if (!section.load) return { items: section.items(snapshot), error: '' };
  return { items: loaded.items, error: loaded.error };
}

// searchTitles are the words that keep a whole section for a search: its
// heading in both vocabularies, and its key.
function searchTitles(section) {
  return [section.title(), section.key.replace(/_/g, ' '), ...(section.searchWords || [])];
}

function DockSection({
  section, snapshot, openMenu, setOpenMenu, clipHost,
  isSectionOpen, setSectionOpen, terms, prefs, notify, onMatches,
}) {
  const [open, setOpen] = useState(() => isSectionOpen(section.key));
  const [pinsVersion, setPinsVersion] = useState(0);
  const [dropOver, setDropOver] = useState(false);
  const fileRef = useRef(null);
  const searching = terms.length > 0;
  const { items: all, error } = useSectionItems(section, snapshot, open || searching);
  const known = all || [];
  const { items: matched } = filterSection({
    terms,
    titles: searchTitles(section),
    items: known,
    textsOf: (item) => [
      section.name(item),
      ...(section.chips?.(item) || []).map((chip) => chip.text),
      ...(section.texts ? section.texts(item) : leafTexts(item)),
    ],
  });
  const pins = pinnedNames(prefs, section.key);
  const items = orderPinned(matched, section.name, pins);
  const hidden = searching && all !== null && !items.length;
  useEffect(() => { onMatches?.(section.key, searching && all === null ? null : items.length); }, [items.length, searching, all === null]);
  const sectionMenuKey = `section:${section.key}`;
  const sectionMenuOpen = openMenu === sectionMenuKey;
  const importFile = (file) => { if (file && section.onImportFile) void section.onImportFile(file); };
  const fileDrag = (event) => !!section.onImportFile && [...(event.dataTransfer?.types || [])].includes('Files');
  if (hidden) return null;

  return html`
    <details
      class=${`dock-section${dropOver ? ' is-drop-over' : ''}`}
      data-key=${section.key}
      open=${open || searching}
      onToggle=${(event) => {
        // A search opens every matching section without persisting it.
        if (searching) return;
        const next = event.currentTarget.hasAttribute('open');
        setOpen(next);
        setSectionOpen(section.key, next);
      }}
      onDragOver=${(event) => { if (!fileDrag(event)) return; event.preventDefault(); setDropOver(true); }}
      onDragLeave=${(event) => { if (!event.currentTarget.contains(event.relatedTarget)) setDropOver(false); }}
      onDrop=${(event) => {
        if (!fileDrag(event)) return;
        event.preventDefault();
        setDropOver(false);
        importFile(event.dataTransfer.files?.[0]);
      }}
    >
      <summary class="dock-section-head">
        <span class="dock-section-title">
          <span class="dock-section-chevron" aria-hidden="true">▸</span>
          <span class="dock-section-icon" aria-hidden="true">${section.icon()}</span>
          ${section.title()}
          <span class="dock-section-count">${all === null ? '' : searching && items.length !== known.length ? `${items.length}/${known.length}` : known.length}</span>
        </span>
        <span class="dock-section-tools">
          ${section.onNew ? html`<button
            type="button"
            class="dock-section-manage"
            data-dock-act="new-item"
            data-dock-kind=${section.key}
            title=${`New ${section.title().toLowerCase().replace(/s$/, '')}`}
            aria-label=${`New in ${section.title()}`}
            onClick=${(event) => { event.preventDefault(); section.onNew(); }}
          >＋</button>` : null}
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
          ${section.onExportAll || section.onImportFile ? html`<span class="dock-card-actions">
            <button
              type="button"
              class="dock-section-manage"
              data-dock-act="section-menu"
              data-dock-kind=${section.key}
              aria-haspopup="menu"
              aria-expanded=${sectionMenuOpen ? 'true' : 'false'}
              aria-label=${`More for ${section.title()}`}
              title="Import or export"
              onClick=${(event) => { event.preventDefault(); setOpenMenu(sectionMenuOpen ? null : sectionMenuKey); }}
            >⋯</button>
            <div class=${`dock-card-menu${sectionMenuOpen ? ' open' : ''}`} role="menu" aria-label=${`${section.title()} actions`}>
              ${section.onImportFile ? html`<${DockMenuItem} act="import" kind=${section.key} setOpenMenu=${setOpenMenu}
                onRun=${() => fileRef.current?.click()}>Import…</${DockMenuItem}>` : null}
              ${section.onExportAll ? html`<${DockMenuItem} act="export-all" kind=${section.key} setOpenMenu=${setOpenMenu}
                onRun=${() => section.onExportAll()}>Export all</${DockMenuItem}>` : null}
            </div>
            <input ref=${fileRef} type="file" accept=".json,application/json" hidden data-dock-import=${section.key}
              onChange=${(event) => { const file = event.currentTarget.files?.[0]; event.currentTarget.value = ''; importFile(file); }} />
          </span>` : null}
        </span>
      </summary>
      <div class="dock-section-items">
        ${error ? html`<div class="dock-empty island-error" role="alert">${error}</div>` : null}
        ${all === null && !error ? html`<div class="dock-empty">Loading…</div>` : null}
        ${all !== null && (items.length ? items.map((item) => html`
          <${DockCard}
            key=${section.name(item)}
            section=${section}
            item=${item}
            openMenu=${openMenu}
            setOpenMenu=${setOpenMenu}
            clipHost=${clipHost}
            layoutVersion=${snapshot}
            snapshot=${snapshot}
            pinned=${pins.includes(section.name(item))}
            onTogglePin=${(name) => { togglePin(prefs, section.key, name); setPinsVersion(pinsVersion + 1); }}
            notify=${notify}
          />
        `) : html`<div class="dock-empty">(${section.empty()})</div>`)}
      </div>
    </details>
  `;
}

export function Dock({
  host, state, sections, isSectionOpen, setSectionOpen, prefs = dashPrefs, notify = null,
}) {
  const [openMenu, setOpenMenu] = useState(null);
  const [query, setQuery] = useState('');
  const [matches, setMatches] = useState({});
  const snapshot = state.snapshot.value;
  const terms = searchTerms(query);
  const onMatches = useCallback((key, count) => {
    setMatches((current) => (current[key] === count ? current : { ...current, [key]: count }));
  }, []);

  useEffect(() => {
    const close = (restoreFocus = false) => {
      const menu = host.querySelector('.dock-card-menu.open');
      if (!menu) return;
      const focusInside = menu.contains(document.activeElement);
      const cog = menu.parentElement?.querySelector('.dock-card-manage, .dock-section-manage[data-dock-act="section-menu"]');
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

  // The process library follows the snapshot sections (only while Processes
  // is enabled); a section marked afterProcesses comes after it.
  const renderSection = (section) => html`
    <${DockSection}
      key=${section.key}
      section=${section}
      snapshot=${snapshot}
      openMenu=${openMenu}
      setOpenMenu=${setOpenMenu}
      clipHost=${host}
      isSectionOpen=${isSectionOpen}
      setSectionOpen=${setSectionOpen}
      terms=${terms}
      prefs=${prefs}
      notify=${notify}
      onMatches=${onMatches}
    />
  `;
  // Local groups only: pin and start go to this daemon's routes by name.
  const groups = (snapshot?.groups || []).filter((group) => !group.fleet_node).map((group) => group.name).filter(Boolean);
  const processes = !!snapshot?.processes_enabled;
  const shown = [...sections.map((section) => section.key), ...(processes ? ['processes'] : [])];
  const nothing = terms.length > 0 && shown.every((key) => matches[key] === 0);
  return [
    html`<div key="search" class="dock-search">
      <input
        type="search"
        class="dock-search-input"
        placeholder=${wizWord('Filter profiles, templates, roles…', 'Scry the grimoire…')}
        aria-label="Filter the library"
        value=${query}
        onInput=${(event) => setQuery(event.currentTarget.value)}
        onKeyDown=${(event) => { if (event.key === 'Escape' && query) { event.preventDefault(); event.stopPropagation(); setQuery(''); } }}
      />
    </div>`,
    nothing ? html`<div key="no-match" class="dock-empty dock-search-empty">No matches for “${query.trim()}”.</div>` : null,
    ...sections.filter((section) => !section.afterProcesses).map(renderSection),
    processes ? html`<${DockProcesses} key="processes" groups=${groups}
      isSectionOpen=${isSectionOpen} setSectionOpen=${setSectionOpen} openMenu=${openMenu} setOpenMenu=${setOpenMenu} clipHost=${host}
      terms=${terms} prefs=${prefs} notify=${notify} onMatches=${onMatches} />` : null,
    ...sections.filter((section) => section.afterProcesses).map(renderSection),
  ];
}

export function mountDockIsland({
  host, state, sections, isSectionOpen, setSectionOpen, notify, registerCleanup,
}) {
  render(html`
    <${Dock}
      host=${host}
      state=${state}
      sections=${sections}
      isSectionOpen=${isSectionOpen}
      setSectionOpen=${setSectionOpen}
      notify=${notify}
    />
  `, host);
  registerCleanup(() => render(null, host));
}
