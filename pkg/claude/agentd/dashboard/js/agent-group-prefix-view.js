// agent-group-prefix-view.js -- an agent's name after its group's prefix
// ("t/lead", agent-group-prefix.js), as its terminal's tab, title bar and the
// phone terminal's title show it. Short of room, the prefix gives way before
// the name does (mux.css .agent-group-named).

import { h } from 'preact';
import htm from 'htm';

const html = htm.bind(h);

export function GroupMarkedName({ name, mark }) {
  if (!mark) return name;
  return html`<span class="agent-group-named"><span class="agent-group-prefix">${mark.prefix}</span><span class="agent-group-slash">/</span><span class="agent-group-name">${name}</span></span>`;
}
