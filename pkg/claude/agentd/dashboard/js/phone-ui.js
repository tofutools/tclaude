// phone-ui.js -- the phone layout's shared building blocks, in the shape of
// the approved phone mocks: avatars with a state badge, filled state pills,
// cards with a coloured accent, rounded chip rows, scope badges, the app
// bar's icon buttons and rows of equal-width buttons. phone.css styles them
// (its "Phone UI primitives" section); a view composes them instead of
// drawing its own, so every phone view looks like one app.
//
// Nothing here knows about the dashboard's data: callers pass names, states
// and handlers. Every primitive renders plain markup a test can query by
// role or text.

import { h } from 'preact';
import htm from 'htm';
import { Button } from './ui-components.js';
import { Tabs } from './ui-tabs.js';

const html = htm.bind(h);

const join = (...parts) => parts.filter(Boolean).join(' ');

// AVATAR_HUES is how many avatar colours phone.css has (data-hue 0..7).
export const AVATAR_HUES = 8;

// avatarHue picks one of the avatar colours for a name, stably.
export function avatarHue(name) {
  let hash = 0;
  for (const ch of String(name || '')) hash = (hash * 31 + ch.codePointAt(0)) >>> 0;
  return hash % AVATAR_HUES;
}

// Avatar is a person's or an agent's picture: a coloured circle with an
// initial, or (bot) a rounded tile with a glyph, the mock's agent icon. badge
// is a small corner glyph saying what the agent waits on (🔑 an approval, ❓ a
// question, 💥 an error). dim greys it out (an offline agent).
export function Avatar({ name = '', glyph = '', bot = false, badge = '', dim = false, class: extra }) {
  const face = glyph || String(name || '?').trim().charAt(0).toUpperCase() || '?';
  return html`<span class=${join('phone-av', bot && 'phone-av-bot', dim && 'phone-av-dim', extra)}
    data-hue=${bot ? undefined : avatarHue(name)} aria-hidden="true">${face}${badge && html`<i class="phone-av-badge">${badge}</i>`}</span>`;
}

// PILL_TONES are StatePill's colours: ask (purple, a question or decision),
// work (green), idle, off (dim), err (red) and key (amber, an approval).
export const PILL_TONES = Object.freeze(['ask', 'work', 'idle', 'off', 'err', 'key']);

// pillTone maps an agent state word (group-activity.js memberVariant, or a
// card's kind) to its pill tone.
const TONE_OF = Object.freeze({
  asking: 'ask', decide: 'ask', decision: 'ask', question: 'ask',
  working: 'work', busy: 'work',
  idle: 'idle',
  offline: 'off', away: 'off',
  error: 'err', crashed: 'err', failed: 'err',
  approval: 'key', key: 'key',
});
export function pillTone(state) {
  const word = String(state || '').toLowerCase();
  if (PILL_TONES.includes(word)) return word;
  return TONE_OF[word] || 'idle';
}

// StatePill is a state in a filled pill; the text defaults to the state word.
export function StatePill({ state, tone = '', children, class: extra, title }) {
  return html`<span class=${join('phone-pill', `phone-pill-${tone || pillTone(state)}`, extra)} title=${title}>${children ?? state}</span>`;
}

// CARD_ACCENTS colour a card's left edge: key (amber, an approval), ask
// (purple, a decision) and q (blue, a question).
export const CARD_ACCENTS = Object.freeze(['key', 'ask', 'q']);

// Card is the mock's panel: a rounded box, optionally with a coloured left
// edge. A div: dashboard.css hides every <section> but the active tab's.
export function Card({ accent = '', class: extra, children, ...rest }) {
  const edge = CARD_ACCENTS.includes(accent) ? accent : '';
  return html`<div ...${rest} class=${join('phone-box', edge && `phone-box-${edge}`, extra)}>${children}</div>`;
}

// Section is a titled block of a view: an eyebrow heading with an optional
// count and a link at its end (more). A div with role="group", not a
// <section>, for the same reason as Card.
// fold ({ open, onToggle }) makes the whole head one tap target that opens
// and closes the section, its chevron at the end.
export function Section({ title, count = null, children, more = null, fold = null, class: extra }) {
  const label = html`<span>${title}${count !== null && html` · ${count}`}</span>`;
  return html`<div class=${join('phone-section', extra)} role="group" aria-label=${title}>
    <h2 class="phone-section-head ui-eyebrow">
      ${fold
        ? html`<button data-ui-exempt type="button" class="phone-section-fold" aria-expanded=${fold.open ? 'true' : 'false'}
            onClick=${fold.onToggle}>${label}<span class="phone-section-chev" aria-hidden="true">${fold.open ? '⌄' : '›'}</span></button>`
        : html`${label}${more}`}
    </h2>
    ${children}
  </div>`;
}

// ChipRow is a row of rounded chips that scrolls sideways: one view's
// filters or groups, one of them picked. items: [{ key, label, count?,
// badge?, badgeTone? }]; count is muted after the label, badge a bubble
// (something waiting there), blue unless badgeTone says danger or warn. It is a tab row (ui-tabs.js), so the keyboard and screen
// reader see the same tabs as everywhere else; only the look is the
// phone's.
export function ChipRow({ items, value, onSelect, label, class: extra }) {
  return html`<${Tabs} class=${join('phone-chips', extra)} label=${label} value=${value} onSelect=${onSelect}
    items=${items.map((item) => ({
      key: item.key,
      title: item.title,
      disabled: item.disabled,
      label: html`${item.label}${item.count !== undefined && item.count !== null && html` <span class="phone-chip-count">${item.count}</span>`}${item.badge > 0 && html` <span class=${`ui-badge ui-badge-${item.badgeTone || 'accent'}`}>${item.badge}</span>`}`,
    }))} />`;
}

// SCOPE_KINDS say how much of a view a phone offers: ro (read-only, blue),
// simp (simplified, green) and desk (desktop only, dashed).
export const SCOPE_KINDS = Object.freeze(['ro', 'simp', 'desk']);

// ScopeBadge labels a menu entry with what the phone can do there.
export function ScopeBadge({ kind = 'ro', children }) {
  return html`<span class=${`phone-scope phone-scope-${SCOPE_KINDS.includes(kind) ? kind : 'ro'}`}>${children}</span>`;
}

// IconButton is an app-bar action: a 44px square with a glyph, its label for
// screen readers and as a tooltip. badge is a count in its corner.
export function IconButton({ icon, label, onClick, disabled = false, danger = false, badge = 0, class: extra, ...rest }) {
  return html`<button data-ui-exempt type="button" ...${rest} class=${join('phone-ib', danger && 'danger', extra)}
    aria-label=${label} title=${label} disabled=${disabled} onClick=${onClick}>
    <span aria-hidden="true">${icon}</span>${badge > 0 && html`<span class="ui-badge ui-badge-accent phone-ib-badge">${badge}</span>`}
  </button>`;
}

// ButtonRow lays out a card's actions: buttons of equal width (grow) in one
// row, wrapping on a narrow phone. buttons: [{ key, label, onClick, tone?:
// 'pri'|'ok'|'no', grow?: false, disabled?, title?, ariaLabel? }]; grow
// defaults to true, a short "⋯" can opt out.
export function ButtonRow({ buttons, class: extra }) {
  return html`<div class=${join('phone-btns', extra)}>
    ${buttons.map((b) => html`<${Button} key=${b.key || b.label} class=${join('phone-btn', b.tone && `phone-btn-${b.tone}`, b.grow !== false && 'phone-btn-grow')}
      disabled=${!!b.disabled} title=${b.title} aria-label=${b.ariaLabel} onClick=${b.onClick}>${b.label}<//>`)}
  </div>`;
}
