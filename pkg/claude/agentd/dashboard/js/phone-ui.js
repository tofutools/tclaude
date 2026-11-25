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

// ViewIcon is a top bar's sliders: it opens a view's View sheet (Agents,
// Now).
export const ViewIcon = html`<svg class="phone-agents-view-icon" viewBox="0 0 24 24" width="22" height="22" aria-hidden="true" fill="none"
  stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M4 6h10M18 6h2M4 12h4M12 12h8M4 18h12" />
  <circle cx="16" cy="6" r="2" /><circle cx="10" cy="12" r="2" /><circle cx="18" cy="18" r="2" /></svg>`;

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
// as="ul" makes the box a list itself, for a list of rows in a card.
export function Card({ accent = '', as = 'div', class: extra, children, ...rest }) {
  const edge = CARD_ACCENTS.includes(accent) ? accent : '';
  const tag = as === 'ul' || as === 'ol' ? as : 'div';
  return html`<${tag} ...${rest} class=${join('phone-box', edge && `phone-box-${edge}`, extra)}>${children}<//>`;
}

// Empty is a view's or a section's quiet state: nothing there yet, still
// loading, or gone. One muted line, the same on every phone page.
export function Empty({ children, class: extra }) {
  return html`<p class=${join('ui-empty phone-empty', extra)}>${children}</p>`;
}

// errorText is the one wording of a read that failed: "Could not read X:
// why", or "Could not read X." when there is no reason to give.
export function errorText(what, why = '') {
  const reason = String(why?.message ?? why ?? '').trim();
  return reason ? `Could not read ${what}: ${reason}` : `Could not read ${what}.`;
}

// ErrorNote is an error on a phone page or sheet: a red-edged note the
// screen reader announces (role=alert), with Retry when there is a way to
// try again.
export function ErrorNote({ children, onRetry = null, class: extra }) {
  return html`<div role="alert" class=${join('ui-error-box phone-error', extra)}><span class="phone-error-text">${children}</span>${onRetry
    && html`<${Button} size="sm" class="phone-error-retry" onClick=${onRetry}>Retry<//>`}</div>`;
}

// Section is a titled block of a view: an eyebrow heading with an optional
// count and a link at its end (more). A div with role="group", not a
// <section>, for the same reason as Card.
// fold ({ open, onToggle }) makes the whole head one tap target that opens
// and closes the section, its chevron at the end; a `more` beside a folding
// head sits after it, a tap target of its own.
export function Section({ title, count = null, children, more = null, fold = null, class: extra }) {
  const label = html`<span>${title}${count !== null && html` · ${count}`}</span>`;
  return html`<div class=${join('phone-section', extra)} role="group" aria-label=${title}>
    <h2 class=${join('phone-section-head ui-eyebrow', fold && more && 'phone-section-head-more')}>
      ${fold
        ? html`<button data-ui-exempt type="button" class="phone-section-fold" aria-expanded=${fold.open ? 'true' : 'false'}
            onClick=${fold.onToggle}>${label}<span class="phone-section-chev" aria-hidden="true">${fold.open ? '⌄' : '›'}</span></button>${more}`
        : html`${label}${more}`}
    </h2>
    ${children}
  </div>`;
}

// SheetHead is a sub-head inside a sheet ("Move to", "Actions", "Session"):
// the same eyebrow as a Section's head, with one margin everywhere. A sheet
// has its own title above, so it is an h3.
export function SheetHead({ class: extra, children, ...rest }) {
  return html`<h3 ...${rest} class=${join('ui-eyebrow phone-sheet-subhead', extra)}>${children}</h3>`;
}

// ChipRow is the phone's one row of rounded chips that scrolls sideways
// (phone.css .phone-chips): a view's sub-views, filters or groups. items:
// [{ key, label, count?, badge?, badgeTone?, title?, disabled?, href?,
// class?, attrs? }]; count is muted after the label, badge a bubble
// (something waiting there), blue unless badgeTone says danger or warn.
//
// By default one chip is picked (value) and the row is a tab row
// (ui-tabs.js), so the keyboard and screen reader see the same tabs as
// everywhere else. toggle makes the chips pressed buttons in a group
// instead, for filters over one list rather than views (Mail's folders,
// Agents' groups, several of which may be on): an item is pressed when its
// own pressed is true, and onSelect(key) toggles it.
export function ChipRow({ items, value, onSelect, label, toggle = false, class: extra }) {
  const text = (item) => html`${item.label}${item.count !== undefined && item.count !== null && html` <span class="phone-chip-count">${item.count}</span>`}${item.badge > 0 && html` <span class=${`ui-badge ui-badge-${item.badgeTone || 'accent'}`}>${item.badge}</span>`}`;
  if (toggle) {
    return html`<div class=${join('phone-chips', extra)} role="group" aria-label=${label}>
      ${items.map((item) => html`<button data-ui-exempt key=${item.key} type="button" ...${item.attrs}
        class=${join('ui-tab', item.class, item.pressed && 'active')} aria-pressed=${item.pressed ? 'true' : 'false'}
        title=${item.title} disabled=${item.disabled} onClick=${() => onSelect?.(item.key)}>${text(item)}</button>`)}
    </div>`;
  }
  return html`<${Tabs} class=${join('phone-chips', extra)} label=${label} value=${value} onSelect=${onSelect}
    items=${items.map((item) => ({ ...item, label: text(item) }))} />`;
}

// SCOPE_KINDS say how much of a view a phone offers: ro (read-only, blue),
// simp (simplified, green) and desk (desktop only, dashed).
export const SCOPE_KINDS = Object.freeze(['ro', 'simp', 'desk']);

// ScopeBadge labels a menu entry with what the phone can do there.
export function ScopeBadge({ kind = 'ro', children }) {
  return html`<span class=${`phone-scope phone-scope-${SCOPE_KINDS.includes(kind) ? kind : 'ro'}`}>${children}</span>`;
}

// RowMore is a row's ⋯: an IconButton, muted, that opens the row's menu.
// popup says what it opens ('dialog', a sheet, by default); null when the ⋯
// goes to a page instead (a group's ⋯).
export function RowMore({ label, onClick, popup = 'dialog', class: extra, ...rest }) {
  return html`<${IconButton} ...${rest} icon="⋯" label=${label} aria-haspopup=${popup || undefined}
    class=${join('phone-row-more', extra)} onClick=${onClick} />`;
}

// IconButton is an app-bar action: a 44px square with a glyph, its label for
// screen readers and as a tooltip. badge is a count in its corner; dot a
// plain dot there instead (something is off its defaults).
export function IconButton({ icon, label, onClick, disabled = false, danger = false, badge = 0, dot = false, class: extra, ...rest }) {
  return html`<button data-ui-exempt type="button" ...${rest} class=${join('phone-ib', danger && 'danger', extra)}
    aria-label=${label} title=${label} disabled=${disabled} onClick=${onClick}>
    <span aria-hidden="true">${icon}</span>${badge > 0 && html`<span class="ui-badge ui-badge-accent phone-ib-badge">${badge}</span>`}${!badge && dot && html`<span class="phone-ib-dot" aria-hidden="true"></span>`}
  </button>`;
}

// SheetTitle is a sheet's title, the same in every sheet (phone.css
// .phone-sheet-title; tk_81b4849c). clip keeps it to one line, ending in …
// (an agent's name, a message's recipient); in a head row beside other
// controls it takes the room left.
export function SheetTitle({ children, clip = false, class: extra, ...rest }) {
  return html`<h2 ...${rest} class=${join('phone-sheet-title', clip && 'phone-sheet-title-clip', extra)}>${children}</h2>`;
}

// ButtonRow lays out a card's or a sheet's actions: buttons of equal width
// (grow) in one row, wrapping on a narrow phone. It is the phone's one
// button row: no view writes its own .phone-btns (tk_81b4849c). buttons:
// [{ key, label, onClick, tone?: 'pri'|'ok'|'no', grow?: false, disabled?,
// title?, ariaLabel?, type?: 'submit', class?, attrs? }]; grow defaults to
// true, a short "⋯" can opt out; attrs spreads onto the button (data-*
// hooks, aria-pressed, aria-haspopup). label makes the row a group named so
// (an answer's options).
export function ButtonRow({ buttons, label = '', class: extra }) {
  return html`<div class=${join('phone-btns', extra)} role=${label ? 'group' : undefined} aria-label=${label || undefined}>
    ${buttons.map((b) => html`<${Button} key=${b.key || b.label} type=${b.type} ...${b.attrs}
      class=${join('phone-btn', b.tone && `phone-btn-${b.tone}`, b.grow !== false && 'phone-btn-grow', b.class)}
      disabled=${!!b.disabled} title=${b.title} aria-label=${b.ariaLabel} onClick=${b.onClick}>${b.label}<//>`)}
  </div>`;
}
