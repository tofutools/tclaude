// collab-new-menu.js -- the Collab panel's ＋ New menu (tcl-ievs1w):
// everything the dashboard can make from there, in one place. The header's
// ＋ New offers every kind; a group header's ＋ opens the same menu made for
// that group. It took over the strip's ＋ (open or create a drawing board or a
// process, import a drawing board) and adds what the Groups tab's ＋ makes.
//
// Header menu: Agent… (the spawn dialog), Shell… and Page… (pick the group),
// Drawing board… (the drawing board list: open one, create one, import, export, delete),
// Process… (the process list: open one or start a new one), Import drawing board….
// Group menu: Agent…, Shell, Drawing board and Page made in that group, Process…,
// Import drawing board….

import { h } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { useDismissablePopover } from './hover-popover.js';
import { Menu, MenuItem, MenuSep } from './ui-components.js';
import { DrawingBoardList } from './drawing-board-island.js';
import { ProcessMenuList } from './process-collab-menu.js';
import { drawingBoardDeleteRequest, importDrawingBoardFile } from './drawing-board-manage.js';
import { openDrawingBoardPane } from './drawing-board-overlay-state.js';
import { openProcessTab } from './process-editor-place.js';
import { openAgentSpawnModal } from './agent-spawn-controller.js';
import { addGroupDrawingBoard, addGroupPage, addGroupShell } from './groups-resources.js';
import { groupContextEntry, processRunApi, processContext } from './process-context.js';
import { shellToast } from './shell-state.js';

const html = htm.bind(h);

const cwdOf = (group) => (group?.default_cwd || '').trim();

// newMenuEntries is what the menu lists at its top level: [{ id, label, line,
// disabled, view?, run? }]. A view entry opens a step inside the menu (a
// list, or which group); a run entry acts and closes it.
export function newMenuEntries({ group = null, groups = [], processes = false, wizard = false }) {
  const entries = [{ id: 'agent', label: 'Agent…', line: group ? `Spawn an agent into ${group.name}` : 'Spawn an agent',
    run: (ctx) => ctx.spawn(group ? { groupName: group.name } : {}) }];
  if (group) {
    const cwd = cwdOf(group);
    entries.push({ id: 'shell', label: 'Shell', line: cwd ? `A terminal in ${cwd}` : 'Set the group\'s directory first', disabled: !cwd,
      run: (ctx) => ctx.shell(group) });
    entries.push({ id: 'drawing_board', label: 'Drawing board', line: "A drawing board shared with the group's agents", run: (ctx) => ctx.drawingBoard(group) });
  } else {
    entries.push({ id: 'shell', label: 'Shell…', line: 'A terminal in a group\'s directory', view: 'shell',
      disabled: !groups.some((g) => cwdOf(g)) });
    entries.push({ id: 'drawing_board', label: 'Drawing board…', line: 'Open a drawing board, or create one', view: 'drawing-boards' });
  }
  if (processes) entries.push({ id: 'process', label: wizard ? 'Rite…' : 'Process…', line: wizard ? 'Open a rite, or shape a new one' : 'Open a process, or build a new one', view: 'processes' });
  entries.push(group
    ? { id: 'page', label: 'Page…', line: 'A blank page you draw on; a member takes it over', run: (ctx) => ctx.page(group) }
    : { id: 'page', label: 'Page…', line: 'A blank page in a group', view: 'page', disabled: !groups.length });
  entries.push({ id: 'import', label: 'Import drawing board…', line: 'From an .excalidraw file', separated: true, run: (ctx) => ctx.pickImport() });
  return entries;
}

// defaultNewContext is the menu's seams: what each entry calls.
export const defaultNewContext = () => ({
  spawn: openAgentSpawnModal,
  shell: addGroupShell,
  drawingBoard: (group) => addGroupDrawingBoard(group, { api: processRunApi, notify: shellToast, source: processContext }),
  // The page name must be free in the group: the process context lists its
  // pages, and only the Groups tab keeps it fresh, so read it first.
  page: async (group, pages) => {
    await Promise.resolve(processContext.refresh()).catch(() => {});
    const fresh = groupContextEntry(processContext.context.value, group)?.pages || pages;
    return addGroupPage(group, fresh, { api: processRunApi, notify: shellToast, source: processContext });
  },
  drawingBoardIds: () => processRunApi.drawingBoardIds(),
  openDrawingBoard: openDrawingBoardPane,
  openProcess: openProcessTab,
  notify: shellToast,
  fetchImpl: (...args) => globalThis.fetch(...args),
});

// NewMenu is the ＋ button and its menu. group presets it; groups are the
// ones Shell… and Page… pick from; pagesOf(group) are a group's pages, for a
// free page name. open/onOpen may be held by the caller. request ({ n, view })
// opens the menu on a step: a bare /drawing-boards or /processes URL asks for the
// drawing board or the process list, as it asked the strip's ＋ before.
export function NewMenu({ group = null, groups = [], pagesOf = () => [], processes = false, wizard = false, ctx = defaultNewContext(),
  open: heldOpen, onOpen, label = '＋ New ▾', buttonClass = 'cr-new', request = null }) {
  const [ownOpen, setOwnOpen] = useState(false);
  const held = typeof onOpen === 'function';
  const open = held ? !!heldOpen : ownOpen;
  const [view, setView] = useState('root');
  const [picking, setPicking] = useState(false);
  const rootRef = useRef(null);
  const fileRef = useRef(null);
  const setOpen = (next) => {
    if (!next) setView('root');
    (held ? onOpen : setOwnOpen)(next);
  };
  // However it closed (its own pick, Escape, or another ＋ opening), it opens
  // next on its kinds.
  useEffect(() => { if (!open) setView('root'); }, [open]);
  useEffect(() => {
    if (!request?.n) return;
    setView(request.view === 'processes' && processes ? 'processes' : 'drawing-boards');
    (held ? onOpen : setOwnOpen)(true);
  }, [request?.n]);
  // The delete dialog and the file picker are outside the menu, and the menu
  // is waiting for them.
  useDismissablePopover(rootRef, open && !drawingBoardDeleteRequest.value && !picking, (why) => { if (why !== 'away') setOpen(false); });

  const importFile = async (event) => {
    const file = event.currentTarget.files?.[0];
    event.currentTarget.value = '';
    setPicking(false);
    if (!file) return;
    setOpen(false);
    try {
      const taken = await ctx.drawingBoardIds().catch(() => []);
      const drawingBoard = await importDrawingBoardFile(file, { fetchImpl: ctx.fetchImpl, taken });
      ctx.notify(drawingBoard.skipped
        ? `Imported “${drawingBoard.title}”. ${drawingBoard.skipped} element${drawingBoard.skipped === 1 ? ' a drawing board cannot hold was' : 's a drawing board cannot hold were'} left out.`
        : `Imported “${drawingBoard.title}”`);
      ctx.openDrawingBoard(drawingBoard.id, drawingBoard.title);
    } catch (err) {
      ctx.notify(`Could not import ${file.name}: ${err?.message || err}`, true);
    }
  };
  const run = { ...ctx, page: (g) => ctx.page(g, pagesOf(g)), pickImport: () => { setPicking(true); fileRef.current?.click(); } };
  const entries = newMenuEntries({ group, groups, processes, wizard });
  const entry = (e) => html`${e.separated ? html`<${MenuSep} />` : null}<${MenuItem} key=${e.id} class="gr-new-item" data-new-kind=${e.id}
    disabled=${!!e.disabled} onClick=${(event) => {
      event.stopPropagation();
      if (e.view) { setView(e.view); return; }
      if (e.id !== 'import') setOpen(false);
      void e.run(run);
    }}><span class="gr-new-name">${e.label}</span><span class="gr-new-line">${e.line}</span><//>`;
  const back = html`<button data-ui-exempt type="button" class="cr-new-back" data-new-back="" onClick=${(event) => { event.stopPropagation(); setView('root'); }}>‹ Back</button>`;
  // pickGroup is the "in which group?" step of the header's Shell… and Page….
  const pickGroup = (kind) => {
    const list = kind === 'shell' ? groups.filter((g) => cwdOf(g)) : groups;
    return html`<${Menu} size="sm" class="gr-new-menu cr-new-menu" aria-label=${kind === 'shell' ? 'Shell in which group' : 'Page in which group'}>
      ${back}
      ${list.map((g) => html`<${MenuItem} key=${g.name} class="gr-new-item" data-new-group=${g.name}
        onClick=${(event) => { event.stopPropagation(); setOpen(false); void run[kind](g); }}>
        <span class="gr-new-name">${g.name}</span>${kind === 'shell' ? html`<span class="gr-new-line">${cwdOf(g)}</span>` : null}<//>`)}
    <//>`;
  };
  const body = view === 'drawing-boards'
    ? html`<div class="mux-drawing-board-menu cr-new-list" role="dialog" aria-label="Drawing-boards">${back}
        <${DrawingBoardList} compact fetchImpl=${ctx.fetchImpl} notify=${ctx.notify} onPicking=${setPicking}
          onOpen=${(id, title) => { setOpen(false); void ctx.openDrawingBoard(id, title); }} /></div>`
    : view === 'processes'
      ? html`<div class="mux-drawing-board-menu cr-new-list" role="dialog" aria-label=${wizard ? 'Rites' : 'Processes'}>${back}
        <${ProcessMenuList} fetchImpl=${ctx.fetchImpl} notify=${ctx.notify} wizard=${wizard}
          onOpen=${(id, title) => { setOpen(false); void ctx.openProcess(id, title); }} /></div>`
      : view === 'shell' || view === 'page' ? pickGroup(view)
        : html`<${Menu} size="sm" class="gr-new-menu cr-new-menu" aria-label=${group ? `New in ${group.name}` : 'New'}>${entries.map(entry)}<//>`;
  const title = group ? `Make something in ${group.name}: an agent, a shell, a drawing board, a page or a process` : 'Make something new: an agent, a shell, a drawing board, a page or a process';
  return html`<span class="cr-new-slot" ref=${rootRef}>
    <button data-ui-exempt type="button" class=${buttonClass} aria-haspopup="menu" aria-expanded=${open ? 'true' : 'false'}
      data-new-menu=${group ? group.name : ''} title=${title} aria-label=${title}
      onClick=${(event) => { event.stopPropagation(); setOpen(!open); }}>${label}</button>
    ${open ? body : null}
    <input ref=${fileRef} type="file" accept=".excalidraw,.json,application/json" hidden aria-label="Import an .excalidraw file"
      data-new-import="" onChange=${importFile} onCancel=${() => setPicking(false)} />
  </span>`;
}
