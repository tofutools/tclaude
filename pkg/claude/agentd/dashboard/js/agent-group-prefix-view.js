// agent-group-prefix-view.js -- an agent's name after its group's prefix
// ("t/lead", agent-group-prefix.js), as its terminal's tab, title bar and the
// phone terminal's title show it. Short of room, the prefix gives way before
// the name does: it is cut to the letters that fit beside "/name"
// (fitPrefix), measured in the label's own font whenever the label's width
// changes. The name keeps its CSS ellipsis for when even one letter of the
// prefix leaves it too little room.

import { h } from 'preact';
import { useLayoutEffect, useRef } from 'preact/hooks';
import htm from 'htm';
import { fitPrefix } from './agent-group-prefix.js';

const html = htm.bind(h);

let canvas = null;

// textWidth measures text in an element's font; null where there is no
// canvas to measure with (the prefix then stays whole).
function textWidth(element) {
  const context = (canvas ||= globalThis.document?.createElement?.('canvas'))?.getContext?.('2d');
  if (!context || typeof globalThis.getComputedStyle !== 'function') return null;
  context.font = getComputedStyle(element).font;
  return (text) => context.measureText(text).width;
}

export function GroupMarkedName({ name, mark }) {
  const shown = useRef(null);
  const prefix = mark?.prefix || '';
  useLayoutEffect(() => {
    const text = shown.current?.firstChild;
    const holder = shown.current?.parentElement?.parentElement;
    if (!prefix || !text || text.nodeType !== 3 || !holder) return undefined;
    // In one go, with no paint between: the whole prefix back, the room the
    // label has with it, then the prefix cut to that room. The text node is
    // the one Preact renders, so its next render of the same prefix keeps it.
    const fit = () => {
      const width = textWidth(shown.current);
      if (!width) return;
      text.data = prefix;
      text.data = fitPrefix(prefix, holder.clientWidth - width(`/${name}`), width);
    };
    fit();
    if (typeof ResizeObserver !== 'function') return undefined;
    const observer = new ResizeObserver(fit);
    observer.observe(holder);
    return () => observer.disconnect();
  }, [prefix, name]);
  if (!mark) return name;
  return html`<span class="agent-group-named"><span class="agent-group-prefix" ref=${shown}>${prefix}</span><span class="agent-group-slash">/</span><span class="agent-group-name">${name}</span></span>`;
}
