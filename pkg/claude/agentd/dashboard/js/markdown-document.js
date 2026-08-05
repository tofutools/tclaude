// The rendered Markdown document itself: the piece that turns the tree from
// markdown-model.js into Preact vnodes, and owns the on-demand parser load
// around it. Surfaces that want a Markdown document — today the notification
// attachment viewer — mount this and give it the source text.

import { Fragment, h } from 'preact';
import { useCallback, useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { loadMarkdownParser, markdownToTree, remoteImageSources, REMOTE_IMAGE } from './markdown-model.js';
import { RemoteImage, RemoteImageNotice } from './markdown-remote-image.js';

const html = htm.bind(h);

// One shared empty set, so resetting the load state on a document that has
// nothing loaded is identity-equal and does not re-render.
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
  const [loaded, setLoaded] = useState(NOTHING_LOADED);

  useEffect(() => {
    let active = true;
    setFailed(false);
    loadMarkdownParser()
      .then((ready) => { if (active) setParser(() => ready); })
      .catch(() => { if (active) setFailed(true); });
    return () => { active = false; };
  }, []);

  // A different document is a different set of decisions: whatever the operator
  // chose to fetch for the last one says nothing about this one.
  useEffect(() => { setLoaded(NOTHING_LOADED); }, [source]);

  const load = useCallback((src) => {
    setLoaded((current) => {
      if (current.has(src)) return current;
      const next = new Set(current);
      next.add(src);
      return next;
    });
  }, []);

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
  const loadAll = () => setLoaded((current) => {
    const next = new Set(current);
    for (const src of held) next.add(src);
    return next;
  });

  return html`<div class=${className}>
    <${RemoteImageNotice} count=${held.length} onLoadAll=${loadAll} />
    <${Fragment}>${nodesToVNodes(nodes, { loaded, load })}</${Fragment}>
  </div>`;
}
