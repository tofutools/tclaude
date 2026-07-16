import { h } from 'preact';
import htm from 'htm';

const html = htm.bind(h);

export function ActivityBot({ bot }) {
  return html`<span class=${bot.className} title=${bot.title} aria-label=${bot.title}>
    <span class=${bot.faceClassName}>${bot.face}</span>
    ${bot.tag ? html`<span class="actbot-tag">${bot.tag}</span>` : null}
    ${bot.count > 1 ? html`<span class="actbot-count">${bot.count}</span>` : null}
  </span>`;
}

// ActivityModes is shared by the native Groups header and shell/global slot.
// Stable mode and variant keys retain the same animation nodes across polling
// and cosmetic theme changes; callers decide whether each mode row owns a
// local tooltip or inherits one from its surrounding surface.
export function ActivityModes({ modes, modeTitles = false }) {
  return modes.map((mode) => html`<span
    key=${mode.key}
    class=${`${mode.className} level-${mode.level}`}
    title=${modeTitles ? mode.title : null}
  >${mode.bots.map((bot) => html`<${ActivityBot} key=${bot.key} bot=${bot} />`)}</span>`);
}
