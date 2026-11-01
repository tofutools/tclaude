// terminal-zoom.js -- the zoom controls a terminal shows, and the remembered size
// behind them. The widget (terminals-core.js) does the zooming; this keeps a
// size per terminal, per surface: the same agent can sit at one size in the
// Terminals tab and another in the companion.
//
// One pref per terminal, not a shared map: a pop-out window holds its own copy
// of the prefs, and writing a whole map back would lose the others' sizes.

import { h } from 'preact';
import { useCallback, useState } from 'preact/hooks';
import htm from 'htm';
import { dashPrefs } from './prefs.js';
import { TERMINAL_FONT_DEFAULT, TERMINAL_FONT_MAX, TERMINAL_FONT_MIN, TERMINAL_FONT_STEP } from './terminals-core.js';

const html = htm.bind(h);

const TERMINAL_FONT_PREF = 'tclaude.dash.terminal.font';

function fontPref(surface, terminal) {
  return `${TERMINAL_FONT_PREF}.${surface}.${terminal}`;
}

export function readTerminalFont(surface, terminal, prefs = dashPrefs) {
  try {
    const size = parseFloat(prefs?.getItem?.(fontPref(surface, terminal)));
    return Number.isFinite(size) ? size : null;
  } catch {
    return null;
  }
}

// useTerminalFont holds what a terminal's controls show, and remembers each
// zoom. Pass `onFontChange` to the widget.
export function useTerminalFont(surface, terminal, prefs = dashPrefs) {
  const [font, setFont] = useState({ size: TERMINAL_FONT_DEFAULT, zoomed: false });
  const onFontChange = useCallback((next) => {
    setFont((now) => (now.size === next.size && now.zoomed === next.zoomed ? now : next));
    try {
      if (next.zoomed) prefs?.setItem?.(fontPref(surface, terminal), next.size);
      else prefs?.removeItem?.(fontPref(surface, terminal));
    } catch { /* prefs unavailable: the size holds for this session */ }
  }, [surface, terminal, prefs]);
  return [font, onFontChange];
}

// A press must leave the keyboard with the terminal being zoomed.
const keepFocus = (event) => event.preventDefault();

// TerminalZoom is the −, size, +, reset group. `widget` returns the live widget.
export function TerminalZoom({ font, widget }) {
  const label = font.zoomed ? `Terminal text size ${font.size}` : `Terminal text size ${font.size}, not zoomed`;
  return html`<span class="term-zoom" role="group" aria-label="Terminal zoom"
    title="Pinch or Ctrl+scroll over the terminal to zoom it" onWheel=${(event) => widget()?.zoomWheel?.(event)}>
    <button type="button" class="companion-tool" aria-label="Zoom terminal out" title="Zoom out" disabled=${font.size <= TERMINAL_FONT_MIN}
      onMouseDown=${keepFocus} onClick=${() => widget()?.zoomBy?.(-TERMINAL_FONT_STEP)}>−</button>
    <span class="term-zoom-size" role="status" aria-label=${label}>${font.size}</span>
    <button type="button" class="companion-tool" aria-label="Zoom terminal in" title="Zoom in" disabled=${font.size >= TERMINAL_FONT_MAX}
      onMouseDown=${keepFocus} onClick=${() => widget()?.zoomBy?.(TERMINAL_FONT_STEP)}>+</button>
    <button type="button" class="companion-tool" aria-label="Reset terminal zoom" title="Back to the terminal's own size" disabled=${!font.zoomed}
      onMouseDown=${keepFocus} onClick=${() => widget()?.resetZoom?.()}>⟲</button>
  </span>`;
}
