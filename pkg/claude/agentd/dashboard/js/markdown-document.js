// The rendered Markdown document itself: the piece that turns the tree from
// markdown-model.js into Preact vnodes, and owns the on-demand parser load
// around it. Surfaces that want a Markdown document — today the notification
// attachment viewer — mount this and give it the source text.

import { Fragment, h } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { loadMarkdownParser, markdownToTree, remoteImageSources, REMOTE_IMAGE } from './markdown-model.js';
import { RemoteImage, RemoteImageNotice } from './markdown-remote-image.js';

const html = htm.bind(h);

// One shared empty set for both halves of a fresh document's state, so
// resolving a document nobody has acted on is identity-stable.
const NOTHING = new Set();

// nodesToVNodes is the whole render step. Every tag and attribute here already
// passed markdown-model.js's allowlists, and text arrives as a string child,
// which Preact escapes — so no HTML string is ever built or injected. The one
// node that is not an element is the remote image, which the model marks for a
// component rather than for the DOM.
function nodesToVNodes(nodes, remote) {
  return nodes.map((node, index) => {
    if (typeof node === 'string') return node;
    if (node.tag === REMOTE_IMAGE) {
      return h(RemoteImage, {
        ...node.attrs,
        key: index,
        loaded: remote.loaded.has(node.attrs.src),
        failed: remote.failed.has(node.attrs.src),
        onLoad: remote.load,
        onError: remote.fail,
      });
    }
    const props = { ...node.attrs, key: index };
    const children = nodesToVNodes(node.children || [], remote);
    return children.length ? h(node.tag, props, children) : h(node.tag, props);
  });
}

// MarkdownDocument renders `source` as Markdown. It reports its own loading and
// failure states rather than throwing them at the caller, because the parser
// arrives over the network on first use and a document that cannot be parsed
// must still leave the surrounding surface usable.
//
// `attachments` are the files published with the same notification, so a
// document can illustrate itself with an image attached beside it. Without them
// such a reference degrades to its alt text, which is what a surface that has
// no attachment list to give still gets.
export function MarkdownDocument({ source, attachments, className = 'markdown-document' }) {
  const [parser, setParser] = useState(null);
  const [parserFailed, setParserFailed] = useState(false);
  // The remote images the operator has asked for, the ones whose fetch then
  // failed, and the document all of it was decided about. One piece of state
  // rather than three, because a different document is a different set of
  // decisions, and clearing them in an effect would be a commit too late: the
  // new document would paint — and fetch — with the old document's answers
  // still in hand.
  //
  // A failure belongs to the document and not to the image, because it changes
  // what the document is still holding back — and that is what the count above
  // it reports and what "load all" acts on.
  const [choices, setChoices] = useState({ source, loaded: NOTHING, failed: NOTHING });
  const decided = choices.source === source;
  const loaded = decided ? choices.loaded : NOTHING;
  const failed = decided ? choices.failed : NOTHING;

  useEffect(() => {
    let active = true;
    setParserFailed(false);
    loadMarkdownParser()
      .then((ready) => { if (active) setParser(() => ready); })
      .catch(() => { if (active) setParserFailed(true); });
    return () => { active = false; };
  }, []);

  // `source` is the one this render painted, so a click — or an error from an
  // image this render put on screen — always lands on the document it came from.
  const base = (current) => (current.source === source
    ? current
    : { source, loaded: NOTHING, failed: NOTHING });

  // Asking for an image clears any earlier failure, which is what makes the
  // same call serve both "load" and "try again".
  const choose = (sources) => setChoices((current) => {
    const from = base(current);
    const nextLoaded = new Set(from.loaded);
    const nextFailed = new Set(from.failed);
    let changed = from !== current;
    for (const src of sources) {
      if (!nextLoaded.has(src)) { nextLoaded.add(src); changed = true; }
      if (nextFailed.delete(src)) changed = true;
    }
    return changed ? { source, loaded: nextLoaded, failed: nextFailed } : current;
  });

  const markFailed = (src) => setChoices((current) => {
    const from = base(current);
    if (from.failed.has(src)) return current;
    const nextFailed = new Set(from.failed);
    nextFailed.add(src);
    return { source, loaded: from.loaded, failed: nextFailed };
  });

  if (parserFailed) {
    return html`<div class="markdown-document-state" role="alert">
      <strong>The Markdown renderer could not be loaded.</strong>
      <span>Download the file to read it, or reload the dashboard to try again.</span>
    </div>`;
  }
  if (!parser) {
    return html`<div class="markdown-document-state" role="status">Rendering document…</div>`;
  }

  let nodes;
  try {
    nodes = markdownToTree(parser, source, { attachments });
  } catch {
    return html`<div class="markdown-document-state" role="alert">
      <strong>This document could not be rendered.</strong>
      <span>Download the file to read its original text.</span>
    </div>`;
  }
  if (!nodes.length) {
    return html`<div class="markdown-document-state" role="status">This document is empty.</div>`;
  }

  // Held back is not the same as never asked for: an image whose fetch failed is
  // back to being a placeholder the operator can act on, so it is counted and
  // offered again like any other.
  const held = remoteImageSources(nodes).filter((src) => !loaded.has(src) || failed.has(src));
  const remote = { loaded, failed, load: (src) => choose([src]), fail: markFailed };

  return html`<div class=${className}>
    <${RemoteImageNotice} count=${held.length} onLoadAll=${() => choose(held)} />
    <${Fragment}>${nodesToVNodes(nodes, remote)}</${Fragment}>
  </div>`;
}
