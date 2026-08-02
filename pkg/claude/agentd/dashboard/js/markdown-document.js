// The rendered Markdown document itself: the piece that turns the tree from
// markdown-model.js into Preact vnodes, and owns the on-demand parser load
// around it. Surfaces that want a Markdown document — today the notification
// attachment viewer — mount this and give it the source text.

import { Fragment, h } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { loadMarkdownParser, markdownToTree } from './markdown-model.js';

const html = htm.bind(h);

// nodesToVNodes is the whole render step. Every tag and attribute here already
// passed markdown-model.js's allowlists, and text arrives as a string child,
// which Preact escapes — so no HTML string is ever built or injected.
function nodesToVNodes(nodes) {
  return nodes.map((node, index) => {
    if (typeof node === 'string') return node;
    const props = { ...node.attrs, key: index };
    const children = nodesToVNodes(node.children || []);
    return children.length ? h(node.tag, props, children) : h(node.tag, props);
  });
}

// MarkdownDocument renders `source` as Markdown. It reports its own loading and
// failure states rather than throwing them at the caller, because the parser
// arrives over the network on first use and a document that cannot be parsed
// must still leave the surrounding surface usable.
export function MarkdownDocument({ source, className = 'markdown-document' }) {
  const [parser, setParser] = useState(null);
  const [failed, setFailed] = useState(false);

  useEffect(() => {
    let active = true;
    setFailed(false);
    loadMarkdownParser()
      .then((loaded) => { if (active) setParser(() => loaded); })
      .catch(() => { if (active) setFailed(true); });
    return () => { active = false; };
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
    nodes = markdownToTree(parser, source);
  } catch {
    return html`<div class="markdown-document-state" role="alert">
      <strong>This document could not be rendered.</strong>
      <span>Download the file to read its original text.</span>
    </div>`;
  }
  if (!nodes.length) {
    return html`<div class="markdown-document-state" role="status">This document is empty.</div>`;
  }
  return html`<div class=${className}><${Fragment}>${nodesToVNodes(nodes)}</${Fragment}></div>`;
}
