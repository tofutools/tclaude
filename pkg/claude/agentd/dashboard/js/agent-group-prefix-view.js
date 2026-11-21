// agent-group-prefix-view.js -- an agent's name with its group, as its
// terminal shows it (agent-group-prefix.js).
//
// GroupedName is the full "tclaude/lead", the group dimmed, for a title bar or
// the phone terminal's header. Short of room, the group gives way and the name
// never does: the group is cut to the letters that fit beside "/name"
// (fitGroup), measured in the label's own font whenever its width changes.
//
// TabName is a tab's name, after its group prefix when another tab has the
// same name ("t/lead"). The prefix is never cut: short of room, the name
// ellipsizes.

import { h } from 'preact';
import { useLayoutEffect, useRef } from 'preact/hooks';
import htm from 'htm';
import { fitGroup } from './agent-group-prefix.js';

const html = htm.bind(h);

let canvas = null;

// textWidth measures text in an element's font; null where there is no
// canvas to measure with (the group then stays whole).
function textWidth(element) {
  const context = (canvas ||= globalThis.document?.createElement?.('canvas'))?.getContext?.('2d');
  if (!context || typeof globalThis.getComputedStyle !== 'function') return null;
  context.font = getComputedStyle(element).font;
  return (text) => context.measureText(text).width;
}

const named = (group, name, ref = null) => html`<span class="agent-group-named"><span class="agent-group-prefix" ref=${ref}>${group}</span><span class="agent-group-slash">/</span><span class="agent-group-name">${name}</span></span>`;

export function GroupedName({ name, mark }) {
  const shown = useRef(null);
  const group = mark?.group || '';
  useLayoutEffect(() => {
    const text = shown.current?.firstChild;
    const holder = shown.current?.parentElement?.parentElement;
    if (!group || !text || text.nodeType !== 3 || !holder) return undefined;
    // In one go, with no paint between: the whole group back, the room the
    // label has with it, then the group cut to that room. The text node is
    // the one Preact renders, so its next render of the same group keeps it.
    const fit = () => {
      const width = textWidth(shown.current);
      if (!width) return;
      text.data = group;
      text.data = fitGroup(group, holder.clientWidth - width(`/${name}`), width);
    };
    fit();
    if (typeof ResizeObserver !== 'function') return undefined;
    const observer = new ResizeObserver(fit);
    observer.observe(holder);
    return () => observer.disconnect();
  }, [group, name]);
  return group ? named(group, name, shown) : name;
}

export function TabName({ name, prefix }) {
  return prefix ? named(prefix, name) : name;
}
