import { Fragment, h } from 'preact';

const BOOLEAN_ATTRIBUTES = new Set([
  'allowfullscreen', 'async', 'autofocus', 'autoplay', 'checked', 'controls',
  'default', 'defer', 'disabled', 'formnovalidate', 'hidden', 'inert', 'ismap',
  'itemscope', 'loop', 'multiple', 'muted', 'nomodule', 'novalidate', 'open',
  'playsinline', 'readonly', 'required', 'reversed', 'selected',
]);

const SEMANTIC_CLASS_KEYS = new Set([
  'group-activity', 'group-process-chip', 'group-waves-chip',
  'group-pending-chip', 'group-header-cog', 'group-descr',
  'group-default-cwd', 'group-max-members', 'group-default-model',
  'group-sandbox-profile', 'group-link-chips', 'group-header-actions',
  'group-force-block', 'group-links-section', 'group-pending-block',
  'group-subgroups', 'subtable',
]);

function vnodeKey(element) {
  const explicit = element.getAttribute('data-group-key') ||
    element.getAttribute('data-key') || element.id;
  if (explicit) return explicit;
  const action = element.getAttribute('data-act');
  if (action) {
    const target = element.getAttribute('data-group') ||
      element.getAttribute('data-conv') || element.getAttribute('data-id') || '';
    return `action:${action}:${target}`;
  }
  const pager = element.getAttribute('data-pager');
  if (pager) return `pager:${element.getAttribute('data-list') || ''}:${pager}`;
  const sort = element.getAttribute('data-sort-col');
  if (sort) return `sort:${element.getAttribute('data-sort-table') || ''}:${sort}`;
  for (const name of element.classList || []) {
    if (SEMANTIC_CLASS_KEYS.has(name)) return `class:${name}`;
  }
  return undefined;
}

function attributes(element) {
  const props = {};
  for (const attribute of element.attributes) {
    const name = attribute.name;
    if (BOOLEAN_ATTRIBUTES.has(name)) props[name] = true;
    else props[name] = attribute.value;
  }
  const key = vnodeKey(element);
  if (key !== undefined) props.key = key;
  return props;
}

function convert(node) {
  if (node.nodeType === 3) return node.data;
  if (node.nodeType !== 1) return null;
  const props = attributes(node);
  // slop-fx.js intentionally animates reel children with imperative
  // innerHTML swaps. Treat that tiny body as opaque: Preact owns the machine
  // wrapper/status attributes, while the effect owns its children between
  // status renders. A changed renderer string still replaces the reels.
  if (node.classList.contains('slop-machine')) {
    props.dangerouslySetInnerHTML = { __html: node.innerHTML };
    return h(node.localName, props);
  }
  return h(
    node.localName,
    props,
    Array.from(node.childNodes, convert),
  );
}

// Convert trusted dashboard renderer HTML into real Preact VNodes. This is a
// migration seam, not an HTML injection boundary: callers pass only the
// existing escaped render.js output. data-group-key/data-key/id become Preact
// keys, so snapshot reorders retain the same details/row/control DOM nodes.
export function trustedHTMLToVNodes(markup, documentRef = document) {
  const template = documentRef.createElement('template');
  template.innerHTML = markup || '';
  return h(Fragment, null, Array.from(template.content.childNodes, convert));
}
