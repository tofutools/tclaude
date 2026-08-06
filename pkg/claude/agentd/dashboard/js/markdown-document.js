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

const NOTHING_LOADED = new Set();

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
        ...node.attrs, key: index, loaded: remote.loaded.has(node.attrs.src), onLoad: remote.load,
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
  const [failed, setFailed] = useState(false);
  // The set of remote images the operator has asked for, and the document those
  // decisions were made about. They are one piece of state rather than two
  // because a different document is a different set of decisions, and clearing
  // them in an effect would be a commit too late: the new document would paint
  // — and fetch — with the old document's answers still in hand.
  const [choices, setChoices] = useState({ source, loaded: NOTHING_LOADED });
  const loaded = choices.source === source ? choices.loaded : NOTHING_LOADED;

  useEffect(() => {
    let active = true;
    setFailed(false);
    loadMarkdownParser()
      .then((ready) => { if (active) setParser(() => ready); })
      .catch(() => { if (active) setFailed(true); });
    return () => { active = false; };
  }, []);

  // `source` is the one this render painted, so a click always lands on the
  // document whose button was pressed.
  const choose = (sources) => setChoices((current) => {
    const base = current.source === source ? current.loaded : NOTHING_LOADED;
    const next = new Set(base);
    for (const src of sources) next.add(src);
    return next.size === base.size ? current : { source, loaded: next };
  });

  if (failed) {
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

  const held = remoteImageSources(nodes).filter((src) => !loaded.has(src));

  return html`<div class=${className}>
    <${RemoteImageNotice} count=${held.length} onLoadAll=${() => choose(held)} />
    <${Fragment}>${nodesToVNodes(nodes, { loaded, load: (src) => choose([src]) })}</${Fragment}>
  </div>`;
}
