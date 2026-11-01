// capture-snapshot.js -- the one read of the page behind a capture (tcl-lw1uqb).
// readPage turns the live document into a plain tree in one synchronous walk,
// filterSnapshot is the only place that decides what may leave the page, and
// the image (dashboard-capture.js) and the text (snapshotText) are both drawn
// from what the filter left, so they cannot disagree about a secret.
//
// The image is the operator's viewport. The text reaches past the viewport only
// inside the open form, where collapsed parts and inactive tabs are included and
// labelled. With no form open it covers what is visible.
// dashboard-imperative-boundary: browser-io

export const CAPTURE_MASK = '••••••••';
export const MASKED = '[data-capture-mask], input[type="password"], input[autocomplete$="-password"]';
// Text is capped well under what the daemon accepts, so a long page says it was
// cut rather than being refused.
export const CAPTURE_TEXT_MAX = 60000;
const CAPTURE_VALUE_MAX = 2000;
const CAPTURE_OPTIONS_MAX = 40;
const CAPTURE_LABEL_MAX = 120;
const OPEN_DIALOG = '[aria-modal="true"], dialog[open], .modal-overlay.show, .manage-overlay.show';
const OPEN_PANEL = '[data-capture-form]';
const NOT_CONTENT = /^(script|style|link|noscript|template|meta|title|head)$/;
const FIELD = /^(input|select|textarea)$/;
// What a masked element keeps: enough to lay it out and name it. A title or a
// data attribute can repeat what it holds.
const MASKED_ATTRS = /^(id|class|style|type|hidden|role|name|rows|cols|disabled|readonly|placeholder|aria-label)$/;

const spaced = (value) => String(value || '').replace(/\s+/g, ' ').trim();

// -- read ---------------------------------------------------------------------

function zIndexOf(win, el) {
  const z = Number.parseInt(win.getComputedStyle(el).zIndex, 10);
  return Number.isNaN(z) ? 0 : z;
}

// shown walks up because a closed overlay hides what is inside it, and a
// dashboard keeps some dialogs mounted that way.
function shown(win, el) {
  if (el.closest?.('[data-capture-skip], [inert]')) return false;
  for (let at = el; at; at = at.parentElement) {
    if (win.getComputedStyle(at).display === 'none') return false;
  }
  return true;
}

// openForm is what the operator is filling in: the topmost open dialog, else a
// page marked as a form. A companion window is not modal, so it is neither.
export function openForm(win) {
  const doc = win.document;
  let top = null;
  let height = -Infinity;
  for (const el of doc.querySelectorAll(OPEN_DIALOG)) {
    if (!shown(win, el)) continue;
    const z = zIndexOf(win, el);
    if (z >= height) {
      top = el;
      height = z;
    }
  }
  if (top) return top;
  return [...doc.querySelectorAll(OPEN_PANEL)].find((el) => {
    const rect = el.getBoundingClientRect?.();
    return shown(win, el) && (!rect || rect.width > 0);
  }) || null;
}

function readField(source, tag) {
  if (tag === 'textarea') return { kind: 'textarea', value: source.value };
  if (tag === 'select') {
    const options = [...source.options].map((option) => ({ label: spaced(option.label || option.textContent), selected: !!option.selected }));
    return { kind: 'select', options, multiple: !!source.multiple };
  }
  const kind = String(source.type || 'text').toLowerCase();
  if (kind === 'hidden') return null;
  if (kind === 'checkbox' || kind === 'radio') return { kind, checked: !!source.checked };
  if (kind === 'file') return { kind };
  const list = source.getAttribute('list');
  const choices = list ? [...(source.ownerDocument.getElementById(list)?.options || [])].map((option) => spaced(option.value)) : [];
  return { kind, value: source.value, choices };
}

// readSurface stands in for an element with no DOM content: a canvas or video
// as its current frame, an iframe as nothing, since its page is another document.
function readSurface(source, tag, style, doc) {
  const rect = source.getBoundingClientRect();
  const surface = {
    tag, data: '', width: rect.width, height: rect.height, className: source.getAttribute('class') || '',
    absolute: style.position === 'absolute', left: style.left, top: style.top,
  };
  if (tag === 'iframe') return surface;
  try {
    const canvas = tag === 'canvas' ? source : doc.createElement('canvas');
    if (tag === 'video') {
      canvas.width = source.videoWidth;
      canvas.height = source.videoHeight;
      canvas.getContext('2d').drawImage(source, 0, 0);
    }
    surface.data = canvas.toDataURL('image/png');
  } catch { /* drawn from another origin: it stays a box */ }
  return surface;
}

const clipped = (style) => [style.overflow, style.overflowX, style.overflowY].some((value) => value && value !== 'visible');

function intersect(a, b) {
  return { left: Math.max(a.left, b.left), top: Math.max(a.top, b.top), right: Math.min(a.right, b.right), bottom: Math.min(a.bottom, b.bottom) };
}

function readAttrs(source, tag) {
  const attrs = [];
  for (const attr of source.attributes) {
    if (tag === 'option' && attr.name === 'selected') continue;
    attrs.push({ name: attr.name, value: attr.value, ns: attr.namespaceURI || null });
  }
  if (tag === 'option' && source.selected) attrs.push({ name: 'selected', value: 'selected', ns: null });
  return attrs;
}

// where says what the walk knows on the way down: the box that still shows
// anything, whether this is inside the open form, inside a collapsed part, and
// whether it is on screen, for an element without a box of its own.
function readNode(source, read, where) {
  if (source.nodeType === 3) return { text: source.nodeValue };
  if (source.nodeType !== 1) return null;
  const { win, form, viewport, byElement } = read;
  const tag = source.tagName.toLowerCase();
  if (NOT_CONTENT.test(tag)) return null;
  const inForm = where.inForm || source === form;
  const style = win.getComputedStyle(source);
  const gone = style.display === 'none';
  // Only the open form reaches into what is hidden.
  if (gone && !inForm) return null;

  const node = {
    tag, name: source.localName || tag, ns: source.namespaceURI || null, attrs: readAttrs(source, tag), children: [],
    gone, display: style.display, sticky: style.position === 'sticky',
    skip: source.hasAttribute('data-capture-skip'), masked: !!source.matches?.(MASKED),
    collapsed: where.collapsed || gone || style.visibility === 'hidden',
    terminal: !!source.classList?.contains('xterm'),
    note: source.getAttribute('data-capture-note') || '',
    folded: source.getAttribute('aria-expanded') === 'false',
    textless: source.getAttribute('data-capture-text') === 'none',
    scroll: { x: source.scrollLeft || 0, y: source.scrollTop || 0 },
  };
  byElement.set(source, node);
  if (FIELD.test(tag)) node.field = readField(source, tag);
  if (tag === 'canvas' || tag === 'video' || tag === 'iframe') node.surface = readSurface(source, tag, style, win.document);
  if (tag === 'img') node.src = source.currentSrc || source.src;
  if (tag === 'summary' && source.parentElement?.tagName === 'DETAILS' && !source.parentElement.open) node.folded = true;

  const clip = style.position === 'fixed' ? viewport : where.clip;
  const rect = source.getBoundingClientRect?.();
  const sized = rect && (rect.width > 0 || rect.height > 0);
  const box = sized ? intersect(rect, clip) : null;
  node.seen = !node.collapsed && (sized ? box.right > box.left && box.bottom > box.top : where.seen);
  if (tag === 'input' || tag === 'textarea') return node;

  const inner = {
    inForm, collapsed: node.collapsed, seen: node.seen,
    clip: sized && clipped(style) ? intersect(rect, clip) : clip,
  };
  const closedDetails = tag === 'details' && !source.open;
  for (const child of source.childNodes) {
    const summary = child.nodeType === 1 && child.tagName === 'SUMMARY';
    const copy = readNode(child, read, closedDetails && !summary ? { ...inner, collapsed: true } : inner);
    if (copy) node.children.push(copy);
  }
  return node;
}

// linkLabels points each field at the elements that name it, by node, so that
// filtering those nodes filters the name too.
function linkLabels(doc, byElement) {
  for (const [source, node] of byElement) {
    const ids = String(source.getAttribute('aria-labelledby') || '').split(/\s+/).filter(Boolean);
    const named = [...ids.map((id) => doc.getElementById(id)), ...(node.field ? source.labels || [] : [])];
    node.labels = named.map((el) => byElement.get(el)).filter((label) => label && label !== node);
    // Its field's line says it, so it is not said again as text.
    if (node.field) for (const label of node.labels) label.naming = true;
  }
}

// readPage runs in one go, so everything in a snapshot is from the same moment.
export function readPage(win) {
  const doc = win.document;
  const form = openForm(win);
  const viewport = { left: 0, top: 0, right: win.innerWidth, bottom: win.innerHeight };
  const byElement = new Map();
  const root = readNode(doc.body, { win, form, viewport, byElement }, { inForm: false, collapsed: false, seen: true, clip: viewport });
  linkLabels(doc, byElement);
  return { root, form: form ? byElement.get(form) || null : null, formTitle: form?.getAttribute('data-capture-form') || '' };
}

// -- filter -------------------------------------------------------------------

function holdsSomething(node) {
  if (node.field) return !!(node.field.value || node.field.options?.some((option) => option.selected));
  return node.children.length > 0;
}

function redact(node) {
  const held = holdsSomething(node);
  node.held = held;
  node.attrs = node.attrs.filter((attr) => MASKED_ATTRS.test(attr.name));
  for (const attr of node.attrs) {
    if (attr.name === 'type' && attr.value.toLowerCase() === 'password') attr.value = 'text';
  }
  node.surface = null;
  node.src = '';
  node.note = '';
  node.children = [];
  if (node.field) {
    node.field = { kind: node.field.kind, value: held ? CAPTURE_MASK : '' };
  } else {
    node.children = [{ text: CAPTURE_MASK }];
  }
}

function filterNode(node) {
  if (node.text !== undefined) return;
  if (node.masked) {
    redact(node);
    return;
  }
  node.children = node.children.filter((child) => {
    if (!child.skip) return true;
    // A label link can still reach it.
    child.children = [];
    child.attrs = [];
    return false;
  });
  node.children.forEach(filterNode);
}

// filterSnapshot is the one pass that takes out what must not leave the page:
// the capture's own cue, and whatever a masked element holds, along with
// anything nested in it. It works in place, so nothing else keeps the original.
export function filterSnapshot(snapshot) {
  filterNode(snapshot.root);
  return snapshot;
}

export const snapshotPage = (win) => filterSnapshot(readPage(win));

// -- text ---------------------------------------------------------------------

const attrOf = (node, name) => node.attrs?.find((attr) => attr.name === name)?.value || '';

// plainText is what a node reads as. A part hidden by itself is left out, such
// as the wizard copy of a label, but a walk inside a collapsed section is not
// stopped by that section being collapsed. A button inside a label is not part
// of the name.
function plainText(node, top = true) {
  if (node.text !== undefined) return node.text;
  if (node.field || node.terminal || (!top && (node.gone || node.tag === 'button'))) return '';
  return node.children.map((child) => plainText(child, false)).join(' ');
}

const nameFrom = (nodes) => spaced(nodes.map((node) => plainText(node)).join(' ')).slice(0, CAPTURE_LABEL_MAX);

function firstHeading(node) {
  for (const child of node.children || []) {
    if (/^(h[1-6]|legend|summary)$/.test(child.tag || '') && !child.gone) return child;
    const nested = firstHeading(child);
    if (nested) return nested;
  }
  return null;
}

function sectionName(node) {
  const named = attrOf(node, 'aria-label') || nameFrom(node.labels || []);
  if (named) return spaced(named);
  const heading = firstHeading(node);
  return heading ? nameFrom([heading]) : '';
}

// siblingLabel is the text a dashboard row puts in front of its control, which
// is often a span rather than a <label>. ancestors is nearest first.
function siblingLabel(node, ancestors) {
  let child = node;
  for (const parent of ancestors.slice(0, 3)) {
    const at = parent.children.indexOf(child);
    for (let i = at - 1; i >= 0; i--) {
      const before = parent.children[i];
      if (before.field) return '';
      const text = spaced(plainText(before, false));
      if (text) return text.length <= CAPTURE_LABEL_MAX ? text : '';
    }
    child = parent;
  }
  return '';
}

function fieldLabel(node, ancestors) {
  return spaced(attrOf(node, 'aria-label')) || nameFrom(node.labels || []) || siblingLabel(node, ancestors)
    || attrOf(node, 'placeholder') || attrOf(node, 'title') || attrOf(node, 'name') || attrOf(node, 'id') || 'field';
}

const listed = (items) => {
  const extra = items.length - CAPTURE_OPTIONS_MAX;
  return `${items.slice(0, CAPTURE_OPTIONS_MAX).join(', ')}${extra > 0 ? `, +${extra} more` : ''}`;
};

function quoted(value) {
  const text = String(value ?? '');
  if (text.length <= CAPTURE_VALUE_MAX) return JSON.stringify(text);
  return `${JSON.stringify(text.slice(0, CAPTURE_VALUE_MAX))} (cut, ${text.length} characters)`;
}

function fieldValue(node) {
  const { field } = node;
  if (node.masked) return field.value ? '[masked]' : '[masked, empty]';
  if (field.kind === 'checkbox' || field.kind === 'radio') return field.checked ? 'on' : 'off';
  if (field.kind === 'file') return '[file]';
  if (field.kind === 'select') {
    const picked = field.options.filter((option) => option.selected).map((option) => JSON.stringify(option.label));
    return `${picked.join(', ') || '(none)'} (options: ${listed(field.options.map((option) => option.label))})`;
  }
  const choices = field.choices?.length ? ` (suggestions: ${listed(field.choices)})` : '';
  return `${quoted(field.value)}${choices}`;
}

function fieldLine(node, ancestors) {
  const facts = [node.field.kind];
  const id = attrOf(node, 'id');
  if (id) facts.push(`#${id}`);
  if (node.collapsed) facts.push('collapsed');
  if (node.attrs.some((attr) => attr.name === 'disabled')) facts.push('disabled');
  if (node.attrs.some((attr) => attr.name === 'readonly')) facts.push('read-only');
  return `${fieldLabel(node, ancestors)} [${facts.join(', ')}]: ${fieldValue(node)}`;
}

function holdsField(node) {
  return !!node.children?.some((child) => child.field || holdsField(child));
}

const INLINE = /^(inline|inline-block|inline-flex|inline-grid|contents|ruby)$/;

// lineWriter gathers inline text into lines, and stops at the limit.
function lineWriter(limit) {
  const lines = [];
  let line = '';
  let size = 0;
  const writer = {
    full: false,
    add(text) { line += text; },
    has: () => spaced(line) !== '',
    flush() {
      const text = spaced(line);
      line = '';
      if (!text || writer.full) return;
      if (size + text.length > limit) {
        writer.full = true;
        lines.push(`(text cut at ${size} characters)`);
        return;
      }
      size += text.length + 1;
      lines.push(text);
    },
    lines,
  };
  return writer;
}

function writeNode(node, out, scope, ancestors) {
  if (out.full) return;
  if (node.text !== undefined) {
    if (ancestors[0] && scope.show(ancestors[0]) && !ancestors.some((each) => each.naming)) out.add(node.text);
    return;
  }
  if (node.textless || (node.gone && !scope.form)) return;
  if (node.note) {
    out.flush();
    out.add(`(${node.note})`);
    out.flush();
  }
  if (node.terminal) {
    if (scope.show(node)) {
      out.flush();
      out.add('[terminal]');
      out.flush();
    }
    return;
  }
  if (node.field) {
    if (scope.show(node)) {
      out.flush();
      out.add(fieldLine(node, ancestors));
      out.flush();
    }
    return;
  }
  if (node.collapsed) {
    if (!scope.form || !holdsField(node)) return;
    // A single hidden field already says so on its own line.
    const name = ancestors[0]?.collapsed ? '' : sectionName(node);
    if (name) {
      out.flush();
      out.add(`[collapsed: ${name}]`);
      out.flush();
    }
    for (const child of node.children) writeNode(child, out, scope, [node, ...ancestors]);
    return;
  }
  if (node.tag === 'button') {
    if (!scope.show(node)) return;
    const name = spaced(plainText(node)) || attrOf(node, 'aria-label') || attrOf(node, 'title');
    if (name) out.add(` [${name}${node.folded ? ', collapsed' : ''}] `);
    return;
  }
  const inline = INLINE.test(node.display || '');
  const cell = node.display === 'table-cell';
  if (cell && out.has()) out.add(' | ');
  else if (!inline && !cell) out.flush();
  for (const child of node.children) writeNode(child, out, scope, [node, ...ancestors]);
  if (node.folded && scope.show(node)) out.add(' [collapsed] ');
  if (!inline && !cell) out.flush();
  else out.add(' ');
}

// snapshotText renders a filtered snapshot for an agent. Field lines carry the
// id a fill can name. Terminals are named but not read: unlike a field, a secret
// printed in one carries no mark, and text is easier to pass on than a picture.
export function snapshotText(snapshot, limit = CAPTURE_TEXT_MAX) {
  const { root, form } = snapshot;
  const out = lineWriter(limit);
  if (form) {
    out.add(`Form: ${snapshot.formTitle || sectionName(form) || 'open dialog'} (the whole form, including what is off screen or collapsed)`);
    out.flush();
    writeNode(form, out, { form: true, show: () => true }, []);
  } else {
    out.add('Page: what is on screen (no form is open)');
    out.flush();
    writeNode(root, out, { form: false, show: (node) => node.seen }, []);
  }
  out.flush();
  return out.lines.join('\n');
}
