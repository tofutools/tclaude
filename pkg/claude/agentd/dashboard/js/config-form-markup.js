import { COST_FACTOR_HARNESSES } from './cost-factors.js';
import { createContext, h } from 'preact';
import { useContext, useState } from 'preact/hooks';
import htm from 'htm';
import { browserNotifyPermission, notificationsUnsupportedNote, requestBrowserNotifyPermission } from './browser-notify.js';
import { CLOSE_TAB_EVENTS, CLOSE_TAB_TARGETS } from './terminal-close-tabs.js';
import { configLocation, configPanel, setConfigPanel } from './config-panel.js';
import { FormFillBar } from './form-fill-bar.js';
import { openUsageRulesManager } from './modal-usage-rules.js';
import { KnowledgeSourceRow } from './knowledge-source-island.js';
import { ExternalStewardManager } from './config-external-stewards.js';
import { StewardDefaultPermissions } from './steward-default-permissions.js';
import { dashboardState } from './snapshot-store.js';
import { DICTATED_MARKER } from './companion-model.js';
import { COMMA_PAUSE_DEFAULT, CONTINUOUS_IDLE_DEFAULT, CONTINUOUS_IDLE_MAX, CONTINUOUS_IDLE_MIN, PERIOD_PAUSE_DEFAULT, SILENCE_SEND_DEFAULT, SILENCE_SEND_MAX, SILENCE_SEND_MIN, VOICE_ACTIONS, VOICE_PAUSE_MAX, VOICE_PAUSE_MIN } from './voice-config.js';
import { LocalSpeechRow, syncVoiceSection } from './voice-config-section.js';

const html = htm.bind(h);
const ConfigEventContext = createContext(() => {});

function routedControl(tag, props) {
  const route = useContext(ConfigEventContext);
  const routed = { ...props };
  for (const name of ['onInput', 'onChange', 'onClick', 'onKeyDown']) {
    const local = props[name];
    routed[name] = (event) => {
      local?.(event);
      route(event);
    };
  }
  return h(tag, routed);
}
function ConfigInput(props) { return routedControl('input', props); }
function ConfigSelect(props) { return routedControl('select', props); }
function ConfigTextarea(props) { return routedControl('textarea', props); }
function ConfigButton(props) { return routedControl('button', props); }

// KnowledgeSourceSetting shows what is stored and offers to fetch or remove it.
function KnowledgeSourceSetting() {
  return html`<${Setting} title="Stored source" summary="What this machine holds now.">
    <${KnowledgeSourceRow} snapshot=${dashboardState.snapshot} />
  </${Setting}>`;
}

// BrowserNotifyPermissionButton is the one-per-browser permission grant for
// browser delivery. It is a plain button, NOT a ConfigButton: the permission
// lives in the browser, not in config.json, so clicking it must not route a
// change event and mark the config form dirty.
//
// Browsers only accept requestPermission() from a user gesture, so this
// button — not page load — is the only place tclaude asks.
function BrowserNotifyPermissionButton() {
  const [state, setState] = useState(() => browserNotifyPermission());
  if (state === 'unsupported') {
    return html`<span class="cfg-hint" style="padding-left:0">${notificationsUnsupportedNote()}</span>`;
  }
  if (state === 'granted') return html`<span class="cfg-inline">✓ browser permission granted</span>`;
  const denied = state === 'denied';
  return html`<button type="button" class="cfg-inline" disabled=${denied}
    onClick=${async () => { setState(await requestBrowserNotifyPermission()); }}>
    ${denied ? 'Browser permission blocked — allow it in site settings' : 'Grant browser permission'}
  </button>`;
}

function appendAndFocus(event, id, values, value, onChange) {
  const container = event.currentTarget.parentElement;
  onChange(id, [...values, value]);
  queueMicrotask(() => {
    const inputs = container?.querySelectorAll('.cfg-list-row input');
    inputs?.[inputs.length - 1]?.focus();
  });
}

function StringList({ id, values = [], datalist, placeholder, onChange }) {
  const update = (index, value) => onChange(id, values.map((item, i) => i === index ? value : item));
  return html`<div class="cfg-list" id=${id}>
    ${values.map((value, index) => html`<div class="cfg-list-row" key=${index}>
      <${ConfigInput} type="text" value=${value} placeholder=${placeholder || ''} aria-label=${placeholder || undefined}
        list=${datalist || undefined} autocomplete="off" spellcheck=${false}
        onInput=${event => update(index, event.currentTarget.value)} />
      <${ConfigButton} type="button" class="cfg-row-del" title="Remove"
        onClick=${() => onChange(id, values.filter((_, i) => i !== index))}>×</${ConfigButton}>
    </div>`)}
    <${ConfigButton} type="button" class="cfg-list-add"
      onClick=${event => appendAndFocus(event, id, values, '', onChange)}>+ add</${ConfigButton}>
  </div>`;
}

function TransitionList({ values = [], onChange }) {
  const id = 'cfg-notif-transitions';
  const update = (index, field, value) => onChange(id, values.map((item, i) => i === index ? { ...item, [field]: value } : item));
  return html`<div class="cfg-list" id="cfg-notif-transitions">
    ${values.map((item, index) => html`<div class="cfg-list-row" key=${index}>
      <${ConfigInput} type="text" value=${item.from || ''} placeholder="from state" aria-label="from state"
        data-role="from" list="cfg-state-list" autocomplete="off" spellcheck=${false}
        onInput=${event => update(index, 'from', event.currentTarget.value)} />
      <span class="cfg-arrow">→</span>
      <${ConfigInput} type="text" value=${item.to || ''} placeholder="to state" aria-label="to state"
        data-role="to" list="cfg-state-list" autocomplete="off" spellcheck=${false}
        onInput=${event => update(index, 'to', event.currentTarget.value)} />
      <${ConfigButton} type="button" class="cfg-row-del" title="Remove"
        onClick=${() => onChange(id, values.filter((_, i) => i !== index))}>×</${ConfigButton}>
    </div>`)}
    <${ConfigButton} type="button" class="cfg-list-add"
      onClick=${event => appendAndFocus(event, id, values, { from: '', to: '' }, onChange)}>+ add transition</${ConfigButton}>
  </div>`;
}

function ThresholdList({ values = [], onChange }) {
  const id = 'cfg-precompact-thresholds';
  const update = (index, field, value) => onChange(id, values.map((item, i) => i === index ? { ...item, [field]: value } : item));
  return html`<div class="cfg-list" id="cfg-precompact-thresholds">
    ${values.map((item, index) => html`<div class="cfg-list-row" key=${index}>
      <${ConfigInput} type="number" min="1" value=${item.window_size ?? ''} placeholder="window size (tokens)"
        aria-label="window size (tokens)" data-role="window" autocomplete="off" style="min-width:170px"
        onInput=${event => update(index, 'window_size', event.currentTarget.value)} />
      <span class="cfg-arrow">→</span>
      <${ConfigInput} type="number" min="1" value=${item.min_tokens ?? ''} placeholder="min used tokens before compaction"
        aria-label="min used tokens before compaction" data-role="min" autocomplete="off" style="min-width:170px"
        onInput=${event => update(index, 'min_tokens', event.currentTarget.value)} />
      <${ConfigButton} type="button" class="cfg-row-del" title="Remove"
        onClick=${() => onChange(id, values.filter((_, i) => i !== index))}>×</${ConfigButton}>
    </div>`)}
    <${ConfigButton} type="button" class="cfg-list-add"
      onClick=${event => appendAndFocus(event, id, values, { window_size: '', min_tokens: '' }, onChange)}>+ add floor</${ConfigButton}>
  </div>`;
}

// ===================================================================
// Settings-app layout. A left rail picks one panel at a time; each panel
// groups settings into .cfg-section cards of .cfg-row rows. Every row has
// the same anatomy: a title (the <label> of a single control), a one-line
// summary, the long operator guidance folded behind "More…", the config.json
// key, and the controls on the right. All panels stay mounted — inactive ones
// are hidden by CSS — so the adapter's id-based load/save keeps reading every
// field, and an edit in a hidden panel still saves.
// ===================================================================

// Their ids are the /config/<section> routes: keep KNOWN_SUBTABS in
// nav-history-core.js in step.
export const CONFIG_PANELS = Object.freeze([
  { id: 'appearance', icon: '🎨', title: 'Appearance', lede: 'How the dashboard looks: activity bots, which tabs show, and header and row chrome.' },
  { id: 'terminals', icon: '🖥️', title: 'Terminals & windows', lede: 'Which terminal opens, how agent windows are focused and tiled, and how web terminals attach.' },
  { id: 'voice', icon: '🎙️', title: 'Voice', lede: 'Spoken phrases and the opt-in hands-free settings for dictating to terminals and chat windows. Hands-free settings are off by default.' },
  { id: 'notifications', icon: '🔔', title: 'Notifications', lede: 'When and how tclaude tells you an agent needs attention. Everything still lands in the Messages tab.' },
  { id: 'agents', icon: '🤖', title: 'Agents & spawning', lede: 'Group joining, spawn guardrails, naming and the permissions every agent starts with.' },
  { id: 'context', icon: '🧠', title: 'Context & sessions', lede: 'Compaction, context nudges, resume prompts and how long Claude Code keeps transcripts.' },
  { id: 'costs', icon: '💲', title: 'Usage & costs', lede: 'Cost display, WHAT-IF estimates, subscription usage bars and rate-limit gating.' },
  { id: 'assistants', icon: '💬', title: 'Ask & scribe', lede: 'Defaults for tclaude ask and for the scribe agents behind the 🤖 Edit with agent buttons.' },
  { id: 'steward', icon: '✦', title: 'Steward', lede: 'What a Steward, your helper in the companion window, may do without asking. Part of the Collab experimental feature.' },
  { id: 'daemon', icon: '🌐', title: 'Daemon & network', lede: 'How agentd starts, where the dashboard listens, operator tokens and remote access.' },
  { id: 'logs', icon: '🧾', title: 'Logs & housekeeping', lede: 'Log verbosity and rotation, and long-horizon cleanup of retired agents.' },
  { id: 'experimental', icon: '🧪', title: 'Experimental', lede: 'Opt-in flags for features under active development.' },
  { id: 'advanced', icon: '{ }', title: 'Advanced', lede: 'Structures without a dedicated form, edited as raw JSON.' },
]);

const BADGES = {
  restart: ['cfg-badge-restart', 'agentd restart', 'Takes effect the next time agentd starts'],
  refresh: ['cfg-badge-refresh', 'refresh', 'Takes effect on the next dashboard refresh'],
  launch: ['cfg-badge-refresh', 'next launch', 'Takes effect on the next agent launch or resume'],
  claude: ['cfg-badge-claude', 'Claude Code', 'Only affects Claude Code sessions'],
  deprecated: ['cfg-badge-danger', 'deprecated', 'Deprecated feature'],
  irreversible: ['cfg-badge-danger', 'irreversible', 'Permanently deletes data'],
};
function Badge({ kind }) {
  const [cls, label, title] = BADGES[kind];
  return html`<span class=${`cfg-badge ${cls}`} title=${title}>${label}</span>`;
}

// Setting is one row. `forId` makes the title the <label> of a single control
// (a switch, a select); rows with several controls leave it unset and label
// each control inline. `wide` stacks the controls under the text for lists
// and tables that need the full width.
function Setting({ title, summary, more, k, badges = [], forId, wide = false, child = false, children }) {
  const cls = ['cfg-field', 'cfg-row', wide && 'cfg-row-wide', child && 'cfg-row-child'].filter(Boolean).join(' ');
  return html`<div class=${cls} data-cfg-key=${k || undefined}>
    <div class="cfg-row-text">
      <div class="cfg-row-title">
        ${forId ? html`<label class="cfg-label" for=${forId}>${title}</label>` : html`<span class="cfg-label">${title}</span>`}
        ${badges.map(kind => html`<${Badge} key=${kind} kind=${kind} />`)}
      </div>
      ${summary && html`<div class="cfg-row-summary">${summary}</div>`}
      ${(more || k) && html`<div class="cfg-row-meta">
        ${k && html`<code class="cfg-key">${k}</code>`}
        ${more && html`<details class="cfg-more"><summary>More…</summary><div class="cfg-hint">${more}</div></details>`}
      </div>`}
    </div>
    <div class="cfg-row-ctl">${children}</div>
  </div>`;
}

const VOICE_PAUSE_ROWS = [
  { id: 'cfg-voice-comma', title: 'Comma at a pause', k: 'dashboard.voice.comma_pause_seconds', fallback: COMMA_PAUSE_DEFAULT, summary: 'add a comma where you pause this long while dictating' },
  { id: 'cfg-voice-period', title: 'Period at a pause', k: 'dashboard.voice.period_pause_seconds', fallback: PERIOD_PAUSE_DEFAULT, summary: 'add a period and capitalize what follows where you pause this long' },
];

function Toggle({ id, label, onChange }) {
  return html`<${ConfigInput} type="checkbox" class="cfg-switch" role="switch" id=${id} aria-label=${label || undefined} onChange=${onChange} />`;
}

// What an assistant outside tclaude can do, shown before it is turned on. The
// list comes from the daemon, so it is the list the MCP server offers. The switch
// is always rendered, because the config adapter binds to it on every load.
function ExternalStewardGroup({ loads }) {
  const info = loads?.value?.external_steward;
  return html`<${Group} title="External stewards" lede=${html`An assistant you run yourself can act as a steward through
    <code>tclaude mcp serve</code>, using a credential only you can make. It can't answer permission questions, and it
    can never hold sudo.`}>
    <${Setting} title="Allow external stewards" forId="cfg-steward-external-enabled" k="steward.external_enabled"
      summary="Off, no credential can be made or used. Check what it can do below before turning this on.">
      <${Toggle} id="cfg-steward-external-enabled" />
    </${Setting}>
    ${info?.tools?.length > 0 && html`<${Setting} title="What it can do" wide summary="These are all the tools it gets.">
      <table class="cfg-tool-list">
        <thead><tr><th scope="col">Tool</th><th scope="col">What it does</th><th scope="col">Who decides</th></tr></thead>
        <tbody>${info.tools.map(tool => html`<tr key=${tool.name}>
          <td><code>${tool.name}</code></td>
          <td>${tool.description}</td>
          <td>${tool.access}</td>
        </tr>`)}</tbody>
      </table>
    </${Setting}>`}
    ${info && html`<${Setting} title="Your external stewards" wide
      summary="A credential is shown once and stored only as a hash. Renewing makes a new one, and the old ones keep working until they expire.">
      <${ExternalStewardManager} enabled=${!!info.enabled} />
    </${Setting}>`}
  </${Group}>`;
}

function Group({ title, lede, children }) {
  return html`<div class="cfg-section">
    <h3>${title}</h3>
    ${lede && html`<p class="cfg-group-lede">${lede}</p>`}
    ${children}
  </div>`;
}

function Panel({ id, active, children }) {
  const panel = CONFIG_PANELS.find(item => item.id === id);
  // A <div>, not a <section>: the dashboard's tab CSS hides inactive sections.
  return html`<div class="cfg-panel" role="region" data-cfg-panel=${id} data-cfg-current=${String(active === id)}
    aria-labelledby=${`cfg-panel-title-${id}`}>
    <div class="cfg-panel-head">
      <h2 id=${`cfg-panel-title-${id}`}><span class="cfg-panel-icon" aria-hidden="true">${panel.icon}</span> ${panel.title}</h2>
      <p>${panel.lede}</p>
    </div>
    ${children}
  </div>`;
}

// Signal-backed bits read their signal inside their own tiny component so a
// dirty-state change re-renders only that node, never the uncontrolled form.
function RailDirtyDot({ dirtyPanels, id }) {
  const dirty = dirtyPanels?.value?.includes(id);
  return html`<span class="cfg-rail-dirty" title="Unsaved changes" hidden=${!dirty}></span>`;
}
function SaveSummary({ dirtyPanels }) {
  const ids = dirtyPanels?.value || [];
  if (!ids.length) return html`<span class="cfg-save-summary"></span>`;
  const names = CONFIG_PANELS.filter(panel => ids.includes(panel.id)).map(panel => panel.title);
  return html`<span class="cfg-save-summary"><span class="cfg-rail-dirty" aria-hidden="true"></span>
    <strong>Unsaved changes</strong>${names.length ? html` · ${names.join(', ')}` : ''}</span>`;
}

function ConfigRail({ active, select, dirtyPanels }) {
  // role=navigation on a <div>: the dashboard styles bare <nav> as the tab strip.
  return html`<div class="cfg-rail" role="navigation" aria-label="Config sections">
    <div class="filter-bar cfg-filter-bar">
      <${ConfigInput} type="search" id="cfg-filter" placeholder="Search settings…"
        aria-label="Filter config settings" autocomplete="off" spellcheck="false" />
      <${ConfigButton} type="button" class="clear-filter" id="cfg-filter-clear" hidden>clear ×</${ConfigButton}>
    </div>
    <span class="filter-count" id="cfg-filter-count"></span>
    <div class="cfg-rail-links">
      ${CONFIG_PANELS.map(panel => html`<${ConfigButton} key=${panel.id} type="button" class="cfg-rail-link"
        data-cfg-panel-link=${panel.id} aria-current=${active === panel.id ? 'page' : undefined}
        onClick=${() => select(panel.id)}>
        <span class="cfg-rail-icon" aria-hidden="true">${panel.icon}</span>
        <span class="cfg-rail-title">${panel.title}</span>
        <${RailDirtyDot} dirtyPanels=${dirtyPanels} id=${panel.id} />
        <span class="cfg-rail-hits" data-cfg-hits=${panel.id}></span>
      </${ConfigButton}>`)}
    </div>
  </div>`;
}

// Static form structure. State, conditional enablement, lists, validation,
// and actions are owned by the Config island and adapter; every control keeps
// its historical id so the adapter's load/save round-trip is unchanged.
export function ConfigFormMarkup({ lists = {}, onListChange = () => {}, onFormEvent = () => {}, dirtyPanels = null, loads = null }) {
  // The open section lives in configPanel so the history router can read and
  // restore it; picking one on the rail is a navigation (a history entry).
  const active = configPanel.value;
  const select = (id) => {
    if (!setConfigPanel(id)) return;
    document.dispatchEvent(new CustomEvent('tclaude:navigated', { detail: { location: configLocation(id) } }));
  };
  return html`
    <${ConfigEventContext.Provider} value=${onFormEvent}>
    <div class="config-form" data-capture-form="Config">

    <h2 class="cfg-wizard-title" aria-hidden="true">📜 The Wizard's Almanac</h2>
    <div class="cfg-layout" id="cfg-layout">
    <${ConfigRail} active=${active} select=${select} dirtyPanels=${dirtyPanels} />
    <div class="cfg-main">

    <p class="cfg-intro">
      Editing <code id="cfg-path">~/.tclaude/data/config.json</code>. Edits stay in this form until you
      press <strong>Save changes</strong>, which shows a diff before anything is written. Most
      settings apply on next use; ones marked <${Badge} kind="restart" /> apply after an agentd restart.
    </p>
    <div id="cfg-notice" class="cfg-notice" style="display:none"></div>
    <div id="cfg-errors" class="cfg-errors" style="display:none"></div>
    <div class="cfg-filter-empty" id="cfg-filter-empty" hidden>No settings match “<span id="cfg-filter-empty-q"></span>”.</div>

    <${Panel} id="appearance" active=${active}>
      <${Group} title="Activity bots">
        <${Setting} title="Regular mode" forId="cfg-dashboard-activity-bots-regular" k="dashboard.activity_bots.regular"
          summary="Robot icons in each group header and the top bar, one per status present."
          more=${html`Each group header (and the top bar) shows a little row of robot icons — one per <em>status</em> present among its members, deduped: a bot <strong>dancing</strong> when an agent is working, standing still when idle, raising a ❓ when one awaits input, and so on. A <em>folded</em> group rolls up every agent hidden in its nested groups; unfold it and those bots move down to the visible child headers. <strong>Emoji bots</strong> are lightweight emoji+CSS; <strong>Pixel sprites</strong> are animated pixel-art robots; <strong>Off</strong> hides them. This is the plain dashboard's style (default <em>Emoji bots</em>).`}>
          <${ConfigSelect} id="cfg-dashboard-activity-bots-regular" aria-label="Activity bot style in regular mode">
            <option value="emoji">Emoji bots</option>
            <option value="sprites">Pixel sprites</option>
            <option value="off">Off</option>
          </${ConfigSelect}>
        </${Setting}>
        <${Setting} title="Wizard mode 🧙" forId="cfg-dashboard-activity-bots-wizard" k="dashboard.activity_bots.wizard"
          summary="The same indicator while the 🧙 wizard skin is on."
          more=${html`The same indicator's style when the dashboard's <strong>wizard</strong> mode (the 🧙 in the header) is on — chosen independently of the other modes. Default <em>Wizard glyphs</em> (the fantasy 🧙🕯️📜💥 row). Switch to <em>Pixel sprites</em> to swap in the animated <strong>wizard</strong> spellcasters (casting when working, pondering when awaiting, a backfiring spell on error, an impatient staff-tap when idle) instead, or turn them <em>Off</em>. (Reduced-motion browsers get the bots <em>without</em> animation either way.)`}>
          <${ConfigSelect} id="cfg-dashboard-activity-bots-wizard" aria-label="Activity bot style in wizard mode">
            <option value="emoji">Wizard glyphs</option>
            <option value="sprites">Pixel sprites</option>
            <option value="off">Off</option>
          </${ConfigSelect}>
        </${Setting}>
        <${Setting} title="Slop mode 🎰" forId="cfg-dashboard-activity-bots-slop" k="dashboard.activity_bots.slop"
          summary="The same indicator while the 🎰 casino skin is on."
          more=${html`The group-header activity bots' style when the dashboard's casino <strong>slop</strong> mode (the 🎰 in the header) is on — chosen independently of regular mode, the way slop already swaps the state pill for a slot machine. Default <em>Pixel sprites</em> (the full dancing robots), but you can keep the emoji bots, or turn them off. (Reduced-motion browsers get the bots <em>without</em> animation either way.)`}>
          <${ConfigSelect} id="cfg-dashboard-activity-bots-slop" aria-label="Activity bot style in slop mode">
            <option value="emoji">Emoji bots</option>
            <option value="sprites">Pixel sprites</option>
            <option value="off">Off</option>
          </${ConfigSelect}>
        </${Setting}>
      </${Group}>

      <${Group} title="Overlays">
        <${Setting} title="Preview background" forId="cfg-dashboard-overlay-backdrop" k="dashboard.overlay_backdrop"
          summary="What image and markdown previews do to the page behind them."
          more=${html`Blur is slow in a browser running without hardware acceleration, so it is off by default. Dim keeps the page dark behind the preview. Disconnect screens always stay dim so their message stays readable; blur adds to that when chosen.`}>
          <${ConfigSelect} id="cfg-dashboard-overlay-backdrop" aria-label="Preview background">
            <option value="dim">Dim</option>
            <option value="blur">Blur</option>
            <option value="both">Both</option>
            <option value="none">Nothing</option>
          </${ConfigSelect}>
        </${Setting}>
        <${Setting} title="Window controls" forId="cfg-dashboard-window-controls" k="dashboard.window_controls"
          summary="How the minimize, maximize and close buttons look on the companion window."
          more=${html`Automatic follows the operating system your browser runs on. Linux desktops vary, so Linux uses GNOME-style buttons. Classic keeps the plain minimize button on its own.`}>
          <${ConfigSelect} id="cfg-dashboard-window-controls" aria-label="Window controls">
            <option value="auto">Automatic</option>
            <option value="macos">macOS</option>
            <option value="windows">Windows</option>
            <option value="linux">Linux</option>
            <option value="classic">Classic</option>
          </${ConfigSelect}>
        </${Setting}>
      </${Group}>

      <${Group} title="Tabs">
        <${Setting} title="Plugins tab" forId="cfg-dashboard-always-show-plugins" k="dashboard.always_show_plugins_tab"
          summary="always show the Plugins tab, even with no plugins installed"
          more=${html`The <strong>Plugins</strong> tab manages human-defined integration bundles (e.g. a docker-backed MCP server). By default it <em>auto-hides</em> when you have no plugins installed — most setups never define one, so the empty tab is just clutter. Enable this to keep it visible regardless, e.g. to reach the built-in catalog and install one from there. (A broken <code>plugins.json</code> always shows the tab so the error isn't hidden.) Off by default.`}>
          <${Toggle} id="cfg-dashboard-always-show-plugins" />
        </${Setting}>
        <${Setting} title="Vegas music in regular mode" forId="cfg-slop-vegas-regular" k="slop.vegas_in_regular_mode"
          summary="show the Vegas tab, volume controls & play music outside slop mode"
          more=${html`The dashboard's casino "slop" mode (the 🎰 in the header) bundles a <strong>Vegas</strong> tab with a lounge-radio player and a header volume mixer. Enable this to surface those — the tab, the 🔊 sound switch and 🎚️ volume sliders, and the music — on the <em>plain</em> dashboard too, without the slot machines, coins and sound FX. Music starts muted and plays on your first click (browsers block autoplay); the 🔊 button mutes it (remembered per browser). Off by default. Stored in the <code>slop</code> block of config.json alongside the volume/channel settings.`}>
          <${Toggle} id="cfg-slop-vegas-regular" />
        </${Setting}>
      </${Group}>

      <${Group} title="Header & rows">
        <${Setting} title="Horizontal scroll" forId="cfg-dashboard-hscroll-follow" k="dashboard.hscroll_follow"
          summary="keep the header & tabs pinned while scrolled sideways (follow mode)"
          more=${html`On a window too narrow for a wide member table, the page scrolls sideways. The full-bleed bars (header, tabs, slop marquee) always stretch to fill the width so they never look ragged — this controls their <em>content</em>. <strong>Follow</strong> (default, checked) pins the header controls and tab strip to the viewport and makes them sticky-left, so they stay put and usable while you're scrolled right. <strong>Static</strong> (unchecked) lets that content scroll off with the page — the bars still fill the width, but the controls aren't reachable until you scroll back. Stored as <code>dashboard.hscroll_follow</code> (the default is omitted; static writes <code>false</code>).`}>
          <${Toggle} id="cfg-dashboard-hscroll-follow" />
        </${Setting}>
        <${Setting} title="Group quick options" forId="cfg-dashboard-group-quick-fold" k="dashboard.group_quick_options"
          summary="auto-fold quick options to icons, expand on hover"
          more=${html`Each group header carries a row of editable "quick option" chips — 📝 description, 📁 default dir, 🧠 default profile, 🛡 sandbox profile, 💰 usage profile, 🔗 links. They can grow the header wide. <strong>Checked</strong> (default) folds them to <em>icon-only</em> at rest and slides the text open when you hover the group header — a horizontal accordion that reclaims the space. The group name, activity bots and 👥 member count always stay visible. <strong>Unchecked</strong> keeps the full chips always shown (the original look). Touch devices (which can't hover) always see the full chips regardless. You can also <em>pin</em> a single group's chips open from its ⚙ menu (📌 pin quick options) so it stays expanded even with auto-fold on. Stored as <code>dashboard.group_quick_options</code> (the default <code>hover</code> is omitted; unchecked writes <code>expanded</code>).`}>
          <${Toggle} id="cfg-dashboard-group-quick-fold" />
        </${Setting}>
        <${Setting} title="Group descriptions" forId="cfg-dashboard-show-group-description" k="dashboard.show_group_description" badges=${['deprecated']}
          summary="show the 📝 description chip in each group header"
          more=${html`Every group can carry a short <strong>description</strong> — the 📝 chip beside the group name, click-to-edit. It's a <em>display-only</em> label: nothing in tclaude reads it, so it's a <strong>deprecated</strong> feature that mostly just clutters the header. The dashboard hides the chip <em>by default</em>; existing descriptions are kept in the database but not shown. <strong>Checked</strong> brings the chip back (and with it the only way to view or edit a group's description). Off by default. Stored as <code>dashboard.show_group_description</code> (the default is omitted; checked writes <code>true</code>).`}>
          <${Toggle} id="cfg-dashboard-show-group-description" />
        </${Setting}>
        <${Setting} title="CI on finished PRs" forId="cfg-dashboard-show-final-pr-checks" k="dashboard.show_final_pr_checks"
          summary="always show the CI checks pill on merged & closed PRs"
          more=${html`Agent rows show a CI checks pill (e.g. <code>✓ 26/26</code>) beside each pull request. Once a PR is <strong>merged</strong> or <strong>closed</strong> its CI result is history, so by default the pill is hidden there <em>unless the final run is failing</em>. <strong>Checked</strong> always shows it, like open PRs. Off by default. Stored as <code>dashboard.show_final_pr_checks</code> (the default is omitted; checked writes <code>true</code>).`}>
          <${Toggle} id="cfg-dashboard-show-final-pr-checks" />
        </${Setting}>
        <${Setting} title="Agent hide button" forId="cfg-dashboard-show-agent-hide-btn" k="dashboard.show_agent_hide_button"
          summary=${html`show the per-agent "hide window" button next to "focus"`}
          more=${html`Each agent row opens with two eye icons: <strong>focus</strong> (👁, raise the agent's terminal window) and <strong>hide</strong> (the slashed-eye, detach the window — the agent keeps running). Hiding a window is far less used than focusing it, so the dashboard drops the hide icon <em>by default</em> to keep the quick-control cluster tight — only <strong>focus</strong> shows. <strong>Checked</strong> brings the <strong>hide</strong> icon back for anyone who wants the row shortcut. Detaching is always possible from the terminal itself; this is only the per-row button. Off by default. Stored as <code>dashboard.show_agent_hide_button</code> (the default is omitted; checked writes <code>true</code>).`}>
          <${Toggle} id="cfg-dashboard-show-agent-hide-btn" />
        </${Setting}>
        <${Setting} title="Toolbar follows the clicked group" forId="cfg-dashboard-fused-toolbar-follow" k="dashboard.fused_toolbar_fixed_node"
          summary="with several nodes in Groups, switch the toolbar's defaults and + new group to the node of the group you click"
          more=${html`When Groups shows several nodes together, the toolbar's 🧠 default spawn profile, 🛡 sandbox profile and <strong>+ new group</strong> act on one node at a time, named by the coloured node chip in front of them. <strong>On</strong> (default): clicking a group's header or one of its controls, right-clicking it or dropping onto it switches the toolbar to that group's node, and the chip flashes. <strong>Off</strong>: the toolbar stays on the node you pick in the chip. Stored as <code>dashboard.fused_toolbar_fixed_node</code> (on is the default and omitted; off writes <code>true</code>).`}>
          <${Toggle} id="cfg-dashboard-fused-toolbar-follow" />
        </${Setting}>
        <${Setting} title="Collapse empty Usage views" forId="cfg-dashboard-usage-collapse-subviews" k="dashboard.usage_collapse_subviews"
          summary="when only quota or only costs has data, show that view alone without the Overview / Quota / Costs switcher"
          more=${html`The <strong>Usage</strong> tab has three views: <strong>Overview</strong>, <strong>Quota</strong> (subscription quota forecasts) and <strong>Costs</strong> (pay-per-token spend and WHAT-IF estimates). <strong>Off</strong> (default): all three are always offered once either side has data, and an empty side shows its own empty state — e.g. with only pay-per-token agents, Quota just says there are no samples yet. <strong>On</strong>: with only one side populated, the tab shows that view directly and hides the switcher. The tab itself is hidden when neither side has anything either way. Stored as <code>dashboard.usage_collapse_subviews</code> (off is the default and omitted; on writes <code>true</code>).`}>
          <${Toggle} id="cfg-dashboard-usage-collapse-subviews" />
        </${Setting}>
        <${Setting} title="Open PRs indicator" k="dashboard.always_show_open_prs"
          summary="The footer's Open PRs popover: whether it shows at zero, and how far back the Closed filter reaches."
          more=${html`The bottom-right <strong>Open PRs</strong> indicator opens a popover listing the pull requests <em>you</em> have open across every repository, with CI state and the agent working each one. <strong>Checked</strong> (default) keeps it in the footer permanently, so it stays a fixed entry point (and reaches the recently-closed list) even when nothing is open; unchecked hides it whenever you have no open PRs. It is always hidden until the daemon has resolved a GitHub identity — with no <code>gh</code> login there is nothing to count. The <strong>days</strong> field adds a separate <em>Closed Nd</em> filter in the popover listing your recently merged and closed PRs; it never mixes into the open list or the count. Default 3 days, max 30; <strong>0</strong> hides the filter, while the shared dashboard PR poll still checks a one-day terminal window to keep Groups badges current. Stored as <code>dashboard.always_show_open_prs</code> (default true, so only <code>false</code> is written) and <code>dashboard.recent_pr_window_days</code> (the default is omitted).`}>
          <label class="cfg-inline"><${ConfigInput} type="checkbox" id="cfg-dashboard-always-show-open-prs" /> always show the footer's Open PRs indicator, even at zero</label>
          <label class="cfg-inline">show pull requests merged or closed in the last <${ConfigInput} type="number" id="cfg-dashboard-recent-pr-window-days" min="0" max="30" step="1" placeholder="3" aria-label="Recently closed pull request window (days)" style="width:6em" /> days</label>
        </${Setting}>
      </${Group}>

      <${Group} title="Slop mode 🎰">
        <${Setting} title="Hide the pull lever" forId="cfg-slop-hide-lever" k="slop.hide_pull_lever"
          summary="hide the side pull-lever in slop mode"
          more=${html`Slop ("casino") mode pins a chunky <strong>PULL</strong> lever to the right edge of the Groups tab — yank it to spin every machine at once. Enable this to hide that lever if you find it in the way; the rest of slop mode (slot machines, coins, sound FX) stays exactly as it was. Off by default (the lever shows). Stored in the <code>slop</code> block of config.json.`}>
          <${Toggle} id="cfg-slop-hide-lever" />
        </${Setting}>
      </${Group}>
    </${Panel}>

    <${Panel} id="terminals" active=${active}>
      <${Group} title="Terminal">
        <${Setting} title="Terminal" forId="cfg-terminal" k="terminal"
          summary="Terminal emulator the dashboard's spawn auto-focus / shell-attach opens. Empty = auto-detect.">
          <${ConfigSelect} id="cfg-terminal" aria-label="Terminal emulator">
            <option value="">Auto-detect (default)</option>
            <option value="ghostty">Ghostty</option>
            <option value="kitty">Kitty</option>
            <option value="wezterm">WezTerm</option>
            <option value="alacritty">Alacritty</option>
            <option value="foot">Foot</option>
            <option value="iterm2">iTerm2</option>
            <option value="konsole">Konsole</option>
            <option value="gnome-terminal">GNOME Terminal</option>
            <option value="xfce4-terminal">Xfce Terminal</option>
            <option value="x-terminal-emulator">System terminal (x-terminal-emulator)</option>
            <option value="xterm">XTerm</option>
            <option value="terminal-app">Terminal.app</option>
          </${ConfigSelect}>
        </${Setting}>
        <${Setting} title="Default terminal" forId="cfg-dashboard-default-web-terminal" k="dashboard.default_terminal"
          summary="open focus / windows / terminals as web terminals in the dashboard"
          more=${html`The spawn dialog's <strong>Auto focus</strong>, per-agent <strong>focus</strong> (👁), <strong>open window</strong> and <strong>open terminal</strong> actions — plus bulk focus from the 🪟 <em>windows…</em> modal, the same commands in the ⌘ palette, a click on a CWD path cell, and a message's <em>focus</em> button — open <em>in-browser</em> terminal panes in the dashboard's <strong>Terminals</strong> tab by default, so you never leave the browser. Bulk unfocus still detaches the selected terminal clients. The dedicated web buttons are always web. <strong>Uncheck</strong> to make those general actions pop native OS terminal windows instead. Stored as <code>dashboard.default_terminal</code> (the default <code>web</code> is omitted; unchecked writes <code>native</code>).`}>
          <${Toggle} id="cfg-dashboard-default-web-terminal" />
        </${Setting}>
        <${Setting} title="Directory picker" forId="cfg-dashboard-default-web-directory-picker" k="dashboard.default_directory_picker"
          summary="use the in-dashboard directory picker on localhost too"
          more=${html`Directory fields open an in-browser navigator by default, both locally and remotely. <strong>Uncheck</strong> to use the native OS chooser for loopback/localhost dashboards; remote browsers still use the web picker because a native chooser would appear on the agentd host where it cannot be operated. Stored as <code>dashboard.default_directory_picker</code> (the default <code>web</code> is omitted; unchecked writes <code>native</code>).`}>
          <${Toggle} id="cfg-dashboard-default-web-directory-picker" />
        </${Setting}>
        <${Setting} title="TUI color scheme" forId="cfg-tui-color-scheme" k="tui.color_scheme"
          summary="Palette for tclaude's interactive terminal views (session ls -w, conv ls -w, agent inbox -w)."
          more=${html`The color palette tclaude's interactive terminal views — <code>tclaude session ls -w</code>, <code>conv ls -w</code>, and <code>agent inbox -w</code> — render with. <strong>Default</strong> is tuned to stay readable on both light and dark terminals (a little dimmer on a dark background). <strong>Dark mode (high contrast)</strong> restores the brighter earlier palette (vivid yellow / green / red and a brighter header) for higher contrast on a dark terminal, at the cost of light-terminal readability. Applies to newly launched watch views. Stored as <code>tui.color_scheme</code> (the default is omitted).`}>
          <${ConfigSelect} id="cfg-tui-color-scheme" aria-label="Terminal UI color scheme">
            <option value="default">Default</option>
            <option value="dark-high-contrast">Dark mode (high contrast)</option>
          </${ConfigSelect}>
        </${Setting}>
      </${Group}>

      <${Group} title="Window focus">
        <${Setting} title="Window title" forId="cfg-focus-window-title" k="focus.window_title"
          summary=${html`set the <code>tclaude:${'<id>'}</code> window/tab title`}
          more=${html`tclaude normally stamps a <code>tclaude:${'<id>'}</code> title on each agent's terminal window/tab. That title is how it finds an agent's <em>existing</em> window to <strong>raise</strong> it (window focus) and to <strong>auto-tile</strong>. On a plain desktop terminal some find it ugly — <strong>uncheck</strong> to leave the terminal's own tab title alone. Trade-off: focus/tiling then can't locate the window, so "focus" falls back to opening a <em>new</em> window instead of raising the existing one (affects WSL and native-Linux/X11; the explicit "open window" action still works). <strong>Leave on for WSL</strong>, where window focus depends on it. On (default). Stored as <code>focus.window_title</code> (the default is omitted; unchecked writes <code>false</code>).`}>
          <${Toggle} id="cfg-focus-window-title" />
        </${Setting}>
        <${Setting} title="Raise only" forId="cfg-focus-raiseonly" k="focus.raise_only"
          summary="raise existing windows only (never open a fresh one)"
          more=${html`When on, focusing an agent that has no open window does nothing, instead of opening a new terminal attached to it. Use a row's "open window" action to open one explicitly. Off (default) = open-on-focus.`}>
          <${Toggle} id="cfg-focus-raiseonly" />
        </${Setting}>
        <${Setting} title="Auto-tile" forId="cfg-focus-tile" k="focus.tile.enabled"
          summary="auto-tile windows after a bulk “focus”"
          more=${html`When on, using the 🪟 windows… modal / command palette to focus <em>more than one</em> agent's window rearranges them into a tidy layout instead of leaving them where the OS dropped them. All windows are gathered onto <em>one</em> monitor — the one the first window is on — so a multi-monitor setup isn't scattered. By default windows keep their <em>current size</em> and are only repositioned; tick <strong>resize windows to fill the screen</strong> for the older screen-filling grid. Best-effort per platform (macOS AppleScript, Linux xdotool/kdotool, WSL PowerShell); an unsupported desktop leaves windows as-is. <strong>Gap</strong> = pixels between tiles; <strong>margin</strong> = inset from the screen edges. A single focused window is never tiled. Off (default).`}>
          <${Toggle} id="cfg-focus-tile" />
        </${Setting}>
        <${Setting} title="Tiling layout" child k="focus.tile"
          summary="Layout, sizing and spacing used when auto-tile runs.">
          <label class="cfg-inline">layout
            <${ConfigSelect} id="cfg-focus-tile-layout" aria-label="Tiling layout">
              <option value="grid">grid</option>
              <option value="columns">columns</option>
              <option value="rows">rows</option>
              <option value="cascade">cascade</option>
            </${ConfigSelect}>
          </label>
          <label class="cfg-inline"><${ConfigInput} type="checkbox" id="cfg-focus-tile-resize" /> resize windows to fill the screen</label>
          <label class="cfg-inline">gap <${ConfigInput} type="number" id="cfg-focus-tile-gap" min="0" max="1000" placeholder="8" aria-label="Tiling gap (pixels)" style="width:6em" /> px</label>
          <label class="cfg-inline">margin <${ConfigInput} type="number" id="cfg-focus-tile-margin" min="0" max="1000" placeholder="0" aria-label="Tiling margin (pixels)" style="width:6em" /> px</label>
        </${Setting}>
      </${Group}>

      <${Group} title="Terminals tab">
        <${Setting} title="Stay after last close" forId="cfg-dashboard-stay-on-terminals" k="dashboard.stay_on_terminals_after_last_close"
          summary="stay on the Terminals tab when its last terminal closes"
          more=${html`Closing the last open terminal while the Terminals tab is showing moves the dashboard to the <strong>Groups</strong> tab <em>by default</em>, since an empty Terminals tab has nothing to act on. <strong>Checked</strong> keeps you on the empty Terminals tab instead (the original behavior). Terminals closed while another tab is showing never switch tabs. Off by default. Stored as <code>dashboard.stay_on_terminals_after_last_close</code> (the default is omitted; checked writes <code>true</code>).`}>
          <${Toggle} id="cfg-dashboard-stay-on-terminals" />
        </${Setting}>
      </${Group}>

      <${Group} title="Web terminal attachment">
        <div class="cfg-field cfg-row cfg-row-wide cfg-terminal-attach-field" data-cfg-key="dashboard.terminal_attach">
          <div class="cfg-row-text">
            <div class="cfg-row-title"><label class="cfg-label" for="cfg-terminal-attach-mode">Resize strategy</label></div>
            <div class="cfg-row-summary">Choose how the browser establishes terminal geometry when attaching to tmux.</div>
            <div class="cfg-row-meta">
              <code class="cfg-key">dashboard.terminal_attach</code>
              <details class="cfg-more"><summary>More…</summary>
                <div class="cfg-hint cfg-terminal-attach-hint"><strong>Repair after attach</strong> preserves today's three-step sequence. <strong>Initial resize only</strong> skips the visible nudge and restore. <strong>Resize before attach</strong> waits between sending the fitted grid and starting the tmux attachment. Stored under <code>dashboard.terminal_attach</code>.</div>
              </details>
            </div>
          </div>
          <div class="cfg-row-ctl">
            <${ConfigSelect} id="cfg-terminal-attach-mode" aria-label="Web terminal attach resize strategy">
              <option value="repair">Repair after attach (default)</option>
              <option value="initial">Initial resize only</option>
              <option value="pre_attach">Resize before attach</option>
            </${ConfigSelect}>
            <div class="cfg-terminal-attach-timings">
              <strong>Timing overrides</strong>
              <label><span>Before initial resize</span><${ConfigInput} type="number" id="cfg-terminal-attach-initial-delay" min="0" max="10000" placeholder="0" aria-label="Initial terminal resize delay (milliseconds)" /><span class="cfg-terminal-attach-unit">ms</span></label>
              <label><span>Post-attach repair wait</span><${ConfigInput} type="number" id="cfg-terminal-attach-repair-delay" min="0" max="10000" placeholder="250" aria-label="Post-attach terminal repair delay (milliseconds)" /><span class="cfg-terminal-attach-unit">ms</span></label>
              <label><span>Resize-to-attach wait</span><${ConfigInput} type="number" id="cfg-terminal-attach-pre-delay" min="0" max="10000" placeholder="250" aria-label="Resize before attach delay (milliseconds)" /><span class="cfg-terminal-attach-unit">ms</span></label>
              <span class="cfg-terminal-attach-note">Blank uses the shown default; explicit <code>0</code> means no wait.</span>
            </div>
          </div>
        </div>
      </${Group}>

      <${Group} title="Web terminal tabs">
        <${Setting} title="Auto-close tabs" k="dashboard.close_terminal_tabs" wide
          summary="Close a terminal tab by itself when its tmux side goes away."
          more=${html`<strong>Session ended</strong>: the tmux session is gone — the shell exited (<code>exit</code> / Ctrl-D), the agent exited, or the session was killed. Reconnecting a shell would only start a fresh one, so shell tabs close by default; agent windows stay open so the exit stays visible. <strong>Detached</strong>: the tmux client left but the session lives on — <kbd>prefix</kbd>+<kbd>d</kbd>, or the session was opened elsewhere. Kept by default so Reconnect can bring it back. Closing a tab detaches every viewer of that session, so with <em>Detached</em> on, the other open tabs on it close too. Applies to tabs already open. Stored under <code>dashboard.close_terminal_tabs</code> (defaults are omitted).`}>
          <table class="cfg-close-tabs" aria-label="Auto-close terminal tabs">
            <thead><tr><th></th>${CLOSE_TAB_TARGETS.map(([, label]) => html`<th scope="col">${label}</th>`)}</tr></thead>
            <tbody>${CLOSE_TAB_EVENTS.map(([event, label]) => html`<tr>
              <th scope="row">${label}</th>
              ${CLOSE_TAB_TARGETS.map(([target, targetLabel]) => html`<td>
                <${ConfigInput} type="checkbox" id=${`cfg-close-tabs-${event}-${target}`} aria-label=${`Close ${targetLabel} tabs when ${label.toLowerCase()}`} />
              </td>`)}
            </tr>`)}</tbody>
          </table>
        </${Setting}>
      </${Group}>
    </${Panel}>

    <${Panel} id="voice" active=${active}>
      <${Group} title="Phrases" lede="What you can say. Dictation listens in your browser's language, so pick phrases in that language: an English phrase may not be heard on another one.">
        ${VOICE_ACTIONS.map(({ action, title, summary, more }) => html`<${Setting} key=${action} title=${title} forId=${`cfg-voice-on-${action}`} k="dashboard.voice.phrases"
          summary=${summary} more=${more}>
          <${ConfigInput} type="checkbox" id=${`cfg-voice-on-${action}`} aria-label=${`${title} on`} onChange=${() => syncVoiceSection()} />
          <${ConfigInput} type="text" id=${`cfg-voice-phrase-${action}`} data-voice-action=${action} maxLength="60" aria-label=${title} style="width:14em" />
        </${Setting}>`)}
        <div class="cfg-hint" id="cfg-voice-wake-no-listener" style="padding-left:0" hidden>A wake phrase won't start anything until the wake phrase listener below is on too.</div>
      </${Group}>
      <${Group} title="Dictation">
        <${Setting} title="Mark dictated messages" forId="cfg-voice-mark" k="dashboard.voice.mark_dictated"
          summary=${html`start messages dictated to an agent with <code>${DICTATED_MARKER}</code>, so it knows why sensible symbols may be missing`}
          more="A draft with dictated words in it stays marked after you edit it. Typed messages aren't marked, and neither is anything put in a plain shell.">
          <${Toggle} id="cfg-voice-mark" />
        </${Setting}>
        ${VOICE_PAUSE_ROWS.map(({ id, title, k, fallback, summary }) => html`<${Setting} key=${id} title=${title} forId=${id} k=${k} summary=${summary}
          more="Timing comes from the gaps between recognition results, so it is approximate and a mark can land a word off. Marks the recognizer added aren't doubled. Pauses add nothing in a plain shell, or in languages written without spaces or with marks of their own, such as Japanese, Chinese, Arabic or Hindi. Both must be shorter than send after silence.">
          <${Toggle} id=${id} />
          <label class="cfg-inline">after <${ConfigInput} type="number" id=${`${id}-seconds`} min=${VOICE_PAUSE_MIN} max=${VOICE_PAUSE_MAX} step="0.1" placeholder=${String(fallback)} aria-label=${`Seconds of pause for ${title.toLowerCase()}`} style="width:5em" /> seconds</label>
        </${Setting}>`)}
      </${Group}>
      <${Group} title="Hands-free" lede="Off unless you turn them on. Neither runs on plain HTTP pages away from localhost, or sends by itself in a plain shell.">
        <${Setting} title="Wake phrase listener" forId="cfg-voice-wake" k="dashboard.voice.wake_enabled" badges=${['refresh']}
          summary=${html`<strong>Keeps a microphone open while a dashboard tab is showing</strong>, so saying the wake phrase starts dictation with no click. Without on-device speech, <strong>everything said in the room goes to your browser's speech service, Google's in Chrome, for as long as it listens</strong>, not only what you dictate. On-device speech is recommended here.`}
          more="It dictates into the terminal or chat window with focus, or the only one showing; otherwise it stays idle and says so. An indicator shows whenever it is on, with a Pause button. It stops while the tab is hidden, runs in one tab at a time, and waits while a reply is read aloud. Plain HTTP pages can't use it, because browsers refuse the microphone there.">
          <${Toggle} id="cfg-voice-wake" onChange=${() => syncVoiceSection()} />
        </${Setting}>
        <div class="cfg-hint" id="cfg-voice-wake-no-phrase" style="padding-left:0" hidden>The listener has nothing to listen for until a wake phrase above is on too.</div>
        <${Setting} title="On-device speech" summary="Recognition that stays on this machine, for dictation and the wake listener. It's a language pack Chrome downloads and manages, not part of tclaude. Its size isn't known here. Nothing downloads without your click, and accuracy may differ."
          more="Once installed it is used automatically. If it later stops working, the wake listener stops and says why rather than switching to a speech service; a dictation carries on through the service and says so. This row is about this browser, so it isn't saved with the settings below.">
          <${LocalSpeechRow} />
        </${Setting}>
        <${Setting} title="Send after silence" forId="cfg-voice-silence" k="dashboard.voice.silence_send_seconds"
          summary="send a dictated message to an agent after this many seconds with no new speech"
          more="The draft counts down first. Press Esc, press Cancel or speak again to stop it. An empty draft never sends, and it never runs in a plain shell.">
          <${Toggle} id="cfg-voice-silence" onChange=${() => syncVoiceSection()} />
          <label class="cfg-inline">after <${ConfigInput} type="number" id="cfg-voice-silence-seconds" min=${SILENCE_SEND_MIN} max=${SILENCE_SEND_MAX} step="1" placeholder=${String(SILENCE_SEND_DEFAULT)} aria-label="Seconds of silence before sending" style="width:5em" /> seconds</label>
        </${Setting}>
        <${Setting} title="Continuous mode" forId="cfg-voice-continuous" k="dashboard.voice.continuous_enabled"
          summary=${html`after a voice message sends, the next thing you say sends another, with no wake phrase, while the agent is still working. <strong>The microphone stays open, so without on-device speech everything said in the room goes to your browser's speech service, Google's in Chrome, for as long as it runs.</strong> On-device speech is recommended here.`}
          more="Each message after the first sends on send after silence, or after 3 seconds if that is off, and queues while the agent is busy. It stays on the agent it started on: changing focus, closing that terminal or hiding the tab stops it. So do Esc, Stop, typing in the draft, the stop phrase and a failed send. An indicator shows on the draft while it runs. It stops rather than sends when the agent is blocked on you, at a prompt or held on an approval request, so it can't answer one. An open decision doesn't stop it. It needs an agent on this node whose harness takes messages mid-turn: Gemini CLI, pi, peers' agents, detached terminal windows and plain shells get one message at a time. Replies aren't read aloud while it listens.">
          <${Toggle} id="cfg-voice-continuous" />
          <label class="cfg-inline">stop after <${ConfigInput} type="number" id="cfg-voice-continuous-idle" min=${CONTINUOUS_IDLE_MIN} max=${CONTINUOUS_IDLE_MAX} step="1" placeholder=${String(CONTINUOUS_IDLE_DEFAULT)} aria-label="Seconds of silence before continuous mode stops" style="width:6em" /> seconds of silence</label>
        </${Setting}>
        <div class="cfg-hint" id="cfg-voice-hands-free-warning" style="padding-left:0" role="alert" hidden>⚠ With both on, <strong>sound in the room can send mail to an agent</strong>: anything that says the wake phrase starts a dictation, and silence sends it.</div>
      </${Group}>
    </${Panel}>
    <${Panel} id="notifications" active=${active}>
      <${Group} title="Delivery">
        <${Setting} title="Desktop notifications" forId="cfg-notif-enabled" k="notifications.enabled"
          summary="Master switch for notification banners.">
          <${Toggle} id="cfg-notif-enabled" />
        </${Setting}>
        <${Setting} title="Deliver via" forId="cfg-notif-delivery" k="notifications.delivery" child
          summary="Desktop = the notifier on the agentd host. Browser = any open dashboard tab, which reaches you remotely."
          more=${html`Where an already-decided notification is raised — every setting below still applies either way. <strong>Desktop</strong> is the platform notifier on the machine agentd runs on. <strong>Browser</strong> queues the banner for any open dashboard tab to raise through the Web Notification API, which is what reaches you when you are working <em>remotely</em>, and what still works when the notifying process is sandboxed away from the session D-Bus. Browser delivery needs a dashboard tab open, the permission granted (button above, once per browser), and a secure context — https or localhost. Stored as <code>notifications.delivery</code>.`}>
          <${ConfigSelect} id="cfg-notif-delivery" aria-label="Notification delivery channel">
            <option value="os">Desktop (this machine)</option>
            <option value="browser">Browser (this dashboard)</option>
            <option value="both">Both</option>
          </${ConfigSelect}>
          <${BrowserNotifyPermissionButton} />
        </${Setting}>
        <${Setting} title="Cooldown" k="notifications.cooldown_seconds" child summary="Minimum gap between two banners.">
          <${ConfigInput} type="number" id="cfg-notif-cooldown" min="0" placeholder="5" aria-label="Notification cooldown seconds" />
          <span>seconds between notifications</span>
        </${Setting}>
      </${Group}>

      <${Group} title="What raises a banner">
        <${Setting} title="Notify me when an agent…" k="notifications.transitions"
          summary="Which state changes raise a desktop banner. tclaude still records every transition — this only controls whether you get a notification.">
          <div class="cfg-notif-types" id="cfg-notif-types">
            <label class="cfg-inline"><${ConfigInput} type="checkbox" data-cfg-notify-type="idle" /> goes idle / finishes</label>
            <label class="cfg-inline"><${ConfigInput} type="checkbox" data-cfg-notify-type="awaiting_permission" /> needs permission</label>
            <label class="cfg-inline"><${ConfigInput} type="checkbox" data-cfg-notify-type="awaiting_input" /> awaits input</label>
            <label class="cfg-inline"><${ConfigInput} type="checkbox" data-cfg-notify-type="error" /> errors</label>
            <label class="cfg-inline"><${ConfigInput} type="checkbox" data-cfg-notify-type="exited" /> exits</label>
          </div>
        </${Setting}>
        <${Setting} title="Human messages" forId="cfg-notif-human" k="notifications.human_messages"
          summary=${html`a <code>notify-human</code> message also raises a desktop banner`}
          more="It always lands in the dashboard Messages tab regardless.">
          <${Toggle} id="cfg-notif-human" />
        </${Setting}>
        <${Setting} title="Access requests" k="agent.access_request_*"
          summary="When an agent asks for access. Requests always appear in the dashboard Messages tab."
          more="Both extra alerts are off by default; the desktop banner also requires notifications to be enabled.">
          <label class="cfg-inline"><${ConfigInput} type="checkbox" id="cfg-agent-access-autoopen" /> open a browser when an agent asks for access</label>
          <label class="cfg-inline"><${ConfigInput} type="checkbox" id="cfg-agent-access-notify" /> raise a desktop banner when an agent asks for access</label>
        </${Setting}>
        <${Setting} title="Presented PRs" forId="cfg-agent-presentpr-notify" k="agent.present_pr_notification"
          summary="notify when an agent presents a pull request"
          more="The PR always appears on the agent's dashboard row regardless. Off by default; also requires notifications to be enabled, and follows the delivery channel above.">
          <${Toggle} id="cfg-agent-presentpr-notify" />
        </${Setting}>
      </${Group}>

      <${Group} title="Advanced">
        <${Setting} title="Raw transition rules" wide k="notifications.transitions"
          summary=${html`The checklist above toggles <code>*${' '}→${' '}state</code> rules. Add from-specific or extra rules here (use <code>*</code> as a wildcard for any state).`}
          more="The checklist and this list edit the same setting; an empty list means no state change notifies.">
          <${TransitionList} values=${lists['cfg-notif-transitions']} onChange=${onListChange} />
        </${Setting}>
        <${Setting} title="Notification command" wide k="notifications.notification_command"
          summary="Custom command to run instead of the OS default — one argument per row. Empty = OS default.">
          <${StringList} id="cfg-notif-command" values=${lists['cfg-notif-command']} placeholder="argument" onChange=${onListChange} />
        </${Setting}>
      </${Group}>
    </${Panel}>

    <${Panel} id="agents" active=${active}>
      <${Group} title="Terminal startup groups">
        <${Setting} title="Auto-join" forId="cfg-session-auto-join-group" k="session.auto_join_group"
          summary=${html`a bare <code>tclaude</code> joins the group configured for its directory`}
          more=${html`The launch directory and every group's <strong>default dir</strong> are compared as canonical absolute paths (including symlink resolution). Auto-join is on by default; auto-create is off. CLI flags <code>--auto-join-group[=false]</code> and <code>--auto-join-or-create-group[=false]</code> override these settings for one launch. An explicit <code>--join-group</code> always wins. Stored under <code>session</code>.`}>
          <${Toggle} id="cfg-session-auto-join-group" />
        </${Setting}>
        <${Setting} title="Auto-create" forId="cfg-session-auto-join-or-create-group" k="session.auto_join_or_create_group"
          summary="create and join a directory group when none exists">
          <${Toggle} id="cfg-session-auto-join-or-create-group" />
        </${Setting}>
      </${Group}>

      <${Group} title="Spawn guardrails">
        <${Setting} title="Spawn restriction" forId="cfg-agent-spawnrestrict" k="agent.spawn_group_restriction"
          summary="agents may only spawn into groups they belong to"
          more="Only affects spawn-capable agents; humans always bypass spawn guardrails.">
          <${Toggle} id="cfg-agent-spawnrestrict" />
        </${Setting}>
        <${Setting} title="Spawn allowed groups" wide child k="agent.spawn_allowed_groups"
          summary="Extra groups a spawn-capable agent may always spawn into, on top of the restriction above.">
          <${StringList} id="cfg-agent-allowedgroups" values=${lists['cfg-agent-allowedgroups']} datalist="cfg-group-list" placeholder="group name" onChange=${onListChange} />
        </${Setting}>
        <${Setting} title="Spawn rate limit" k="agent.spawn_max_per_hour" badges=${['restart']}
          summary="0 = unlimited. Off = built-in default (10).">
          <label class="cfg-inline"><${ConfigInput} type="checkbox" id="cfg-agent-spawnmax-enabled" /> cap</label>
          <${ConfigInput} type="number" id="cfg-agent-spawnmax" min="0" placeholder="10" aria-label="Spawn rate limit per hour" />
          <span>spawns / agent / hour</span>
        </${Setting}>
        <${Setting} title="Clone cooldown" forId="cfg-agent-clonecooldown" k="agent.clone_cooldown" badges=${['restart']}
          summary=${html`Minimum gap between two agent-initiated clones of the same agent (Go duration: <code>1m</code>, <code>30s</code>, <code>0</code> to disable). Empty = built-in default.`}>
          <${ConfigInput} type="text" id="cfg-agent-clonecooldown" placeholder="1m" aria-label="Clone cooldown" autocomplete="off" spellcheck="false" style="min-width:120px" />
        </${Setting}>
      </${Group}>

      <${Group} title="Naming">
        <${Setting} title="Normalize spawn names" forId="cfg-agent-spawnnormalize" k="agent.spawn_name_normalize"
          summary="auto-fix invalid characters in an agent name instead of rejecting it"
          more=${html`On (default): a name like <code>code reviewer!</code> becomes <code>code-reviewer</code> at spawn. Off: a name outside <code>[A-Za-z0-9_-]</code> is rejected. Applies to the spawn dialog, <code>tclaude agent spawn</code>, and the daemon.`}>
          <${Toggle} id="cfg-agent-spawnnormalize" />
        </${Setting}>
        <${Setting} title="Name the tmux session" forId="cfg-agent-spawnlabelname" k="agent.spawn_label_from_name"
          summary="derive a spawned agent's session label from its name"
          more=${html`Off (default): a random <code>spwn-XXXXXX</code> label. On: the label is built from the agent's name, so <code>code-reviewer</code> attaches as <code>tclaude session attach code-reviewer</code> and appears under that name in <code>tmux ls</code>. The name is normalized and length-capped first, and a base already taken by another session gets a <code>-2</code>, <code>-3</code>, … suffix (then a random one) — so the label is derived from the name, not always identical to it. Suffixes climb while the older namesake's session row survives. Unnamed spawns keep the random label.`}>
          <${Toggle} id="cfg-agent-spawnlabelname" />
        </${Setting}>
      </${Group}>

      <${Group} title="Permissions">
        <${Setting} title="Default permissions" wide k="agent.default_permissions"
          summary="Permission slugs granted to every agent.">
          <${StringList} id="cfg-agent-permissions" values=${lists['cfg-agent-permissions']} datalist="cfg-slug-list" placeholder="permission slug" onChange=${onListChange} />
        </${Setting}>
      </${Group}>
    </${Panel}>

    <${Panel} id="context" active=${active}>
      <${Group} title="Compaction">
        <${Setting} title="Pre-compact guard" wide k="pre_compact_guard" badges=${['claude']}
          summary="Refuse automatic compaction until used context reaches a per-window floor."
          more=${html`Refuses Claude Code's <em>automatic</em> compaction until used context reaches a per-window floor — so a 1M session doesn't compact at the 200K boundary (~20% of the 1M bar). Lets context grow (and you reincarnate) first. Fails open when usage is unknown. By default only auto-compaction is blocked, never a <code>/compact</code> you type yourself. Floors — for each context-window size (tokens), the minimum used context (tokens) required before compaction is allowed. Empty = built-in defaults (150k of 200k, 800k of 1M).`}>
          <label class="cfg-inline"><${ConfigInput} type="checkbox" id="cfg-precompact-enabled" /> enabled</label>
          <label class="cfg-inline"><${ConfigInput} type="checkbox" id="cfg-precompact-blockmanual" /> also block manual <code>/compact</code></label>
          <${ThresholdList} values=${lists['cfg-precompact-thresholds']} onChange=${onListChange} />
        </${Setting}>
        <${Setting} title="Context nudge" k="agent.context_nudge"
          summary=${html`"Consider reincarnating" nudge as a long-running agent's context fills.`}>
          <label class="cfg-inline"><${ConfigInput} type="checkbox" id="cfg-nudge-enabled" /> enabled</label>
          <span>start</span>
          <${ConfigInput} type="number" id="cfg-nudge-min" min="0" max="100" placeholder="30" aria-label="Context nudge start percent" />
          <span>%, every</span>
          <${ConfigInput} type="number" id="cfg-nudge-interval" min="1" max="100" placeholder="10" aria-label="Context nudge interval percent" />
          <span>%</span>
        </${Setting}>
      </${Group}>

      <${Group} title="Sessions">
        <${Setting} title="Resume-from-summary prompt" k="claude_resume" badges=${['claude']}
          summary="Age and size thresholds for Claude Code's resume chooser, which hangs scripted resumes."
          more=${html`Claude Code shows an interactive "Resume from summary" chooser when you resume a session that's both old <em>and</em> large — which hangs tclaude's scripted resume, since a detached pane can't answer it. These override the age / size thresholds CC uses to decide; the prompt fires only when <strong>both</strong> are exceeded, so raising <em>either</em> high enough suppresses it. <code>tclaude setup --install-resume-threshold-override</code> sets minutes to <code>525600000</code> (≈1000 years) to switch it off. Empty = Claude Code's defaults (70 min / 100k tokens); <code>0</code> always shows it. Injected as <code>CLAUDE_CODE_RESUME_*</code> env vars on tclaude-spawned <code>claude</code> panes only — never written to <code>~/.claude/settings.json</code>, so your manual <code>claude</code> runs are untouched. <strong>Note:</strong> these are <em>undocumented</em>, version-specific Claude Code env vars (verified against CC 2.1.187), so tclaude treats them as best-effort — if a future Claude Code build renames or drops them, this quietly becomes a no-op rather than an error.`}>
          <label class="cfg-inline">show after
            <${ConfigInput} type="number" id="cfg-resume-minutes" min="0" placeholder="70" aria-label="Resume threshold minutes" style="width:8em" /> min idle</label>
          <label class="cfg-inline">and
            <${ConfigInput} type="number" id="cfg-resume-tokens" min="0" placeholder="100000" aria-label="Resume token threshold" style="width:8em" /> tokens</label>
        </${Setting}>
        <${Setting} title="Transcript retention" k="claude_cleanup_period_days" badges=${['claude']}
          summary="How long Claude Code keeps inactive transcripts before deleting them (its default is 30 days)."
          more=${html`Claude Code deletes a conversation transcript (and other stale session data / orphaned worktrees) once it's been <em>inactive</em> this many days — swept at Claude Code startup. Claude Code's own default is <strong>30 days</strong>, so transcripts you haven't touched in a month vanish. Raise this to keep them longer; set a large value like <code>99999</code> to effectively keep them forever (Claude Code rejects <code>0</code> and has no "never" option). Empty = leave Claude Code's default (or whatever you set by hand) untouched. Unlike the resume-threshold overrides above, this <strong>is</strong> written to <code>~/.claude/settings.json</code> (as <code>cleanupPeriodDays</code>) on every session start, so it also protects transcripts from your own plain <code>claude</code> runs. Stored as <code>claude_cleanup_period_days</code>.`}>
          <label class="cfg-inline">keep transcripts for
            <${ConfigInput} type="number" id="cfg-claude-cleanup-days" min="1" placeholder="30" aria-label="Claude Code transcript retention (days)" style="width:8em" /> days</label>
        </${Setting}>
      </${Group}>
    </${Panel}>

    <${Panel} id="costs" active=${active}>
      <${Group} title="Usage limits">
        <${Setting} title="Limits & budgets"
          summary="Spend caps, quota guards and per-agent usage profiles live in their own manager."
          more=${html`Global and group limits (for example "$500 a month, alert at 50%, wrap everyone up at 100%"), usage profiles that budget each agent, and per-agent extensions. The former daily/monthly spend limits were converted into global limits there.`}>
          <button type="button" class="tool" id="cfg-open-usage-rules" onClick=${() => openUsageRulesManager()}>Open usage limits & budgets…</button>
        </${Setting}>
      </${Group}>
      <${Group} title="Cost display">
        <${Setting} title="Cost display multipliers" wide k="cost.estimate_factor"
          summary="Scale displayed dollar amounts across the dashboard, per harness. Recorded data stays unchanged."
          more=${html`Scales displayed dollar amounts across the dashboard, including WHAT-IF estimates. Harness overrides <strong>replace</strong> the default; blank inherits it, and an explicit <code>1</code> means no adjustment. Recorded data stays unchanged. Also editable live in the Costs tab’s Multipliers menu.`}>
          <div class="cfg-cost-factors">
            <div class="cost-factor-row"><label for="cfg-cost-factor">Default<small>Used unless overridden</small></label><span></span><span>×</span>
              <${ConfigInput} type="number" id="cfg-cost-factor" min="0.01" max="10" step="0.01" placeholder="1" aria-label="Cost display multiplier" /></div>
            ${COST_FACTOR_HARNESSES.map(({ key, label }) => html`<div class="cost-factor-row" key=${key}>
              <label for=${`cfg-cost-factor-${key}`}>${label}<small>${key === 'opencode' ? 'All providers in OpenCode' : 'Blank uses default'}</small></label><span></span><span>×</span>
              <${ConfigInput} type="number" id=${`cfg-cost-factor-${key}`} min="0.01" max="10" step="0.01" placeholder="Default" aria-label=${`${label} cost display multiplier`} />
            </div>`)}
          </div>
        </${Setting}>
        <${Setting} title="Show cost on subscription" forId="cfg-cost-show-on-subscription" k="cost.show_on_subscription"
          summary="show the Costs tab + per-agent cost on a subscription (WHAT-IF)"
          more=${html`On a subscription there's no pay-per-token charge, so hypothetical costs are hidden by default. Enable this to show harness-provided <strong>WHAT-IF</strong> estimates of equivalent pay-per-token pricing, clearly separated from real billed spend. A mixed installation can show real and WHAT-IF rows together; pay-per-token spend is always shown regardless of this setting.`}>
          <${Toggle} id="cfg-cost-show-on-subscription" />
        </${Setting}>
        <${Setting} title="Gemini billing" forId="cfg-cost-gemini-billing" k="cost.gemini_billing"
          summary="Whether Gemini CLI cost shows as real API spend or a WHAT-IF estimate."
          more=${html`Gemini CLI records tokens but no price, so tclaude estimates its cost at Gemini API rates. <strong>Auto</strong> reads Gemini's <code>security.auth.selectedType</code>: an API key or Vertex AI is shown as real API spend, a Google sign-in as a <strong>WHAT-IF</strong> estimate. Force a side if auto guesses wrong, e.g. a Google sign-in on a paid Cloud project. The amount is always an estimate, not Google's invoice. Applies to running and resumed sessions; an exited conversation keeps the side it was recorded under. Stored as <code>cost.gemini_billing</code> (auto is omitted).`}>
          <${ConfigSelect} id="cfg-cost-gemini-billing" aria-label="Gemini billing">
            <option value="auto">Auto (from Gemini's auth type)</option>
            <option value="api">API key — real spend</option>
            <option value="subscription">Google sign-in — WHAT-IF</option>
          </${ConfigSelect}>
        </${Setting}>
        <${Setting} title="OpenCode legacy long-context cutoff" k="opencode.legacy_long_context_pricing_cutoff"
          summary="Context size above which OpenCode's legacy long-context price applies to WHAT-IF costs."
          more=${html`Controls when OpenCode's legacy <code>experimentalOver200K</code> catalog price starts applying to WHAT-IF costs. The legacy rate is selected only when a model call exceeds this boundary; exactly at the boundary stays on the base rate. Explicit context tiers from OpenCode's provider catalog always take precedence. Blank = <code>272000</code>. Stored as <code>opencode.legacy_long_context_pricing_cutoff</code>.`}>
          <${ConfigInput} type="number" id="cfg-opencode-legacy-long-context-pricing-cutoff" min="1" step="1" placeholder="272000" aria-label="OpenCode legacy long-context pricing cutoff" style="min-width:110px" />
          <span>context tokens per model call</span>
        </${Setting}>
      </${Group}>

      <${Group} title="Subscription usage">
        <${Setting} title="Poll Anthropic usage API" forId="cfg-usage-poll-anthropic-api" k="usage.poll_anthropic_api"
          summary="refresh Claude usage in the background"
          more=${html`Off by default. The top-bar <strong>Claude</strong> subscription usage bars are normally refreshed by Claude Code's statusline callback while sessions run, then kept from the last cached reading. Enable this only if you want agentd to periodically call Anthropic's usage API while no statusline callback is active. Stored as <code>usage.poll_anthropic_api</code>.`}>
          <${Toggle} id="cfg-usage-poll-anthropic-api" />
        </${Setting}>
        <${Setting} title="Idle timeout" forId="cfg-usage-idle-timeout" k="usage.idle_timeout"
          summary="How long the Claude usage bars keep their last reading after fresh data stops (Go duration, default 72h)."
          more=${html`How long the top-bar <strong>Claude</strong> subscription usage bars (5h / 7d) keep showing their last-known reading after fresh data stops arriving. Fresh data comes from Claude Code's statusline callback, and optionally from the Anthropic usage API poll above. A Go duration — e.g. <code>72h</code> (3 days, the default), <code>30m</code>, <code>2h30m</code>. Blank = default. Stored as <code>usage.idle_timeout</code>.`}>
          <${ConfigInput} type="text" id="cfg-usage-idle-timeout" placeholder="72h" aria-label="Usage readout idle timeout" style="min-width:110px" />
        </${Setting}>
      </${Group}>

      <${Group} title="Rate limits">
        <${Setting} title="Rate-limit gating" forId="cfg-ratelimit-enabled" k="ratelimit"
          summary="When off, no rate-limit thresholds are configured.">
          <${Toggle} id="cfg-ratelimit-enabled" />
        </${Setting}>
        <${Setting} title="5-hour window" child k="ratelimit.five_hour_percent_max_used" summary="Maximum share of the 5-hour window used.">
          <${ConfigInput} type="number" id="cfg-ratelimit-5h" min="0" max="100" step="0.1" placeholder="99" aria-label="Rate limit 5-hour window percent" />
          <span>% max used</span>
        </${Setting}>
        <${Setting} title="7-day window" child k="ratelimit.seven_day_percent_max_used" summary="Maximum share of the 7-day window used.">
          <${ConfigInput} type="number" id="cfg-ratelimit-7d" min="0" max="100" step="0.1" placeholder="99.9" aria-label="Rate limit 7-day window percent" />
          <span>% max used</span>
        </${Setting}>
      </${Group}>
    </${Panel}>

    <${Panel} id="assistants" active=${active}>
      <${Group} title="tclaude ask">
        <${Setting} title="Ask — profile" forId="ask-profile" k="ask.profile"
          summary="Run fresh tclaude ask questions on a saved spawn profile's harness, model and effort."
          more=${html`Optional. Pick a saved <strong>spawn profile</strong> (from the Groups tab) to run fresh <code>tclaude ask</code> questions on its harness — the harness-independent way to ask <strong>Codex</strong> as well as Claude. When set, the profile's harness / model / effort are used and the Model / Effort below are ignored. Leave as <em>(none)</em> to use Claude with the Model / Effort below.`}>
          <${ConfigSelect} id="ask-profile" aria-label="Ask default profile" style="min-width:160px"></${ConfigSelect}>
          <span id="ask-profile-state" class="cfg-hint"></span>
        </${Setting}>
        <${Setting} title="Ask — sandbox" forId="ask-sandbox" k="ask.sandbox"
          summary="Apply the current sandbox policy to each ask turn. Dashboard global default is the default."
          more=${html`<strong>Auto</strong> adds the exact directory group's sandbox rules to the dashboard global policy. Ambiguous groups require a directory default or another selection. A named library profile applies exactly that profile and its includes. <strong>Off</strong> keeps the harness's ordinary ask behavior. Unsupported restrictions refuse the ask with guidance.`}>
          <${ConfigSelect} id="ask-sandbox" aria-label="Ask sandbox policy" style="min-width:160px">
            <option value="auto">Auto (dashboard global + group rules for this dir)</option>
            <option value="global">Dashboard global default</option>
            <option value="off">Off</option>
          </${ConfigSelect}>
        </${Setting}>
        <${Setting} title="Ask — model" forId="ask-model" k="ask.model" child
          summary="Used when no profile is selected and no -m flag is passed.">
          <${ConfigSelect} id="ask-model" aria-label="Ask default model" style="min-width:160px"></${ConfigSelect}>
        </${Setting}>
        <${Setting} title="Ask — effort" forId="ask-effort" k="ask.effort" child
          summary="Defaults to sonnet at medium; a per-call flag always overrides it."
          more=${html`The model and reasoning effort <code>tclaude ask</code> uses for ad-hoc questions when you don't pass <code>-m</code>/<code>--effort</code> <em>and no Profile above is selected</em>. Defaults to <code>sonnet</code> at <code>medium</code> — a balanced, capable default for ad-hoc terminal answers. <em>Built-in default</em> leaves the field unpinned so it tracks that default. Stored in the <code>ask</code> block of config.json (the same file the CLI reads); a per-call flag always overrides it.`}>
          <${ConfigSelect} id="ask-effort" aria-label="Ask default effort" style="min-width:160px"></${ConfigSelect}>
        </${Setting}>
      </${Group}>
      <${Group} title="Scribes">
        <${Setting} title="Scribe — profile" forId="scribe-profile" k="scribe.profile"
          summary="The spawn profile a freshly summoned scribe launches with."
          more=${html`Optional. Pick a saved <strong>spawn profile</strong> (from the Groups tab) that a freshly <em>summoned</em> scribe — the chat agent behind the <strong>🤖 Edit with agent</strong> buttons — launches with: its harness / model / effort / sandbox. This is the harness-independent way to run scribes on <strong>Codex</strong> as well as Claude, or to pin a cheaper model for their light editing. Leave as <em>(default)</em> to use the harness default (Claude Code). Every click summons a fresh, independently named scribe, so the current setting applies without disturbing scribes already working. Stored in the <code>scribe</code> block of config.json.`}>
          <${ConfigSelect} id="scribe-profile" aria-label="Scribe default profile" style="min-width:160px"></${ConfigSelect}>
        </${Setting}>
      </${Group}>
    </${Panel}>

    <${Panel} id="daemon" active=${active}>
      <${Group} title="Startup">
        <${Setting} title="Auto-launch dashboard" forId="cfg-agent-autolaunch" k="agent.auto_launch_dashboard"
          summary=${html`open the dashboard when <code>agentd serve</code> starts`}>
          <${Toggle} id="cfg-agent-autolaunch" />
        </${Setting}>
        <${Setting} title="System tray" forId="cfg-agent-notray" k="agent.disable_tray" badges=${['restart']}
          summary=${html`hide the tray icon (same as <code>--no-tray</code>)`}
          more=${html`Skips the system tray icon <code>agentd serve</code> shows by default. Takes effect next time <code>agentd serve</code> starts.`}>
          <${Toggle} id="cfg-agent-notray" />
        </${Setting}>
      </${Group}>

      <${Group} title="Local dashboard">
        <${Setting} title="Dashboard port" forId="cfg-agent-dashboardport" k="agent.dashboard_port" badges=${['restart']}
          summary=${html`Fixed port for the dashboard + approval popup. Empty / <code>0</code> = a random free port each start.`}
          more=${html`A stable port gives a bookmarkable URL; takes effect on the next <code>agentd serve</code>.`}>
          <${ConfigInput} type="number" id="cfg-agent-dashboardport" min="0" max="65535" placeholder="(random)" aria-label="Dashboard port" autocomplete="off" style="min-width:110px" />
        </${Setting}>
        <${Setting} title="Dashboard bind" forId="cfg-agent-dashboardbind" k="agent.dashboard_bind" badges=${['restart']}
          summary=${html`Host/interface the <strong>local</strong> dashboard binds to. Empty / <code>127.0.0.1</code> = loopback only.`}
          more=${html`Host/interface the <strong>local</strong> dashboard binds to (host only — the port is above). Empty / <code>127.0.0.1</code> = loopback only. Set <code>0.0.0.0</code> / <code>::</code> to expose it on the network — <strong>only behind your own auth</strong> (reverse proxy / VPN / IAP), since its own gate is just a cookie + operator token. This is <em>not</em> the “Remote access” section below — that's a separate mTLS + passphrase listener. Takes effect on the next <code>agentd serve</code>.`}>
          <${ConfigInput} type="text" id="cfg-agent-dashboardbind" list="cfg-agent-dashboardbind-list" placeholder="127.0.0.1" aria-label="Dashboard bind host" autocomplete="off" spellcheck="false" style="min-width:140px" />
          <datalist id="cfg-agent-dashboardbind-list">
            <option value="127.0.0.1">loopback only — default (safe)</option>
            <option value="0.0.0.0">all interfaces — behind your OWN auth</option>
            <option value="::">all interfaces (IPv6)</option>
          </datalist>
        </${Setting}>
      </${Group}>

      <${Group} title="Security">
        <${Setting} title="Persist operator token" k="agent.persist_operator_token" badges=${['restart']} wide
          summary="Keep the same operator token across agentd restarts, in a private file or the OS keychain."
          more=${html`Default off: a fresh token is minted every <code>agentd serve</code>. File persistence stores it at <code>0600 ~/.tclaude/data/operator_token</code>, which the human CLI reads automatically and agent sandboxes deny. Keychain storage is an explicit opt-in because keychain access is platform-dependent and may prompt; the CLI does not retrieve it automatically. Existing tokens are not copied between stores. The secret is never written to <code>config.json</code>. Takes effect on the next <code>agentd serve</code>.`}>
          <label class="cfg-inline"><${ConfigInput} type="checkbox" id="cfg-agent-persisttoken" /> keep the same operator token across restarts in the private file (same as <code>--persist-operator-token</code>)</label>
          <label class="cfg-inline"><${ConfigInput} type="checkbox" id="cfg-agent-persisttoken-keychain" /> explicitly use the OS keychain instead (same as <code>--persist-operator-token-keychain</code>; implies persistence)</label>
        </${Setting}>
        <${Setting} title="Broker limits" forId="cfg-broker-enforce" k="broker.enforce_limits"
          summary="actually reject callers over the limit"
          more=${html`Ceilings on the brokered endpoints an agent in tclaude’s built-in sandbox uses to reach the database — <strong>20 requests/second per agent</strong> and <strong>10 MiB per request</strong>. Default off is <em>shadow mode</em>: excess is still measured and logged (saying what it <em>would</em> have refused) but nothing is refused, so you can see real traffic against the ceilings before enforcing. A denial-of-service backstop, not traffic shaping.`}>
          <${Toggle} id="cfg-broker-enforce" />
        </${Setting}>
      </${Group}>

      <${Group} title="Remote access"
        lede=${html`Expose this <strong>dashboard</strong> to your phone or another machine over the
          network (LAN / mesh VPN / tunnel), behind <strong>mTLS (a client certificate) +
          a passphrase</strong>. A <em>separate</em> HTTPS listener — the loopback dashboard
          you're using now is never weakened. See <a href="https://github.com/tofutools/tclaude/blob/main/docs/remote.md" target="_blank" rel="noopener">docs/remote.md</a>.`}>
        <${Setting} title="Prerequisite" wide
          summary=${html`First generate the certificates + passphrase on the host (one time) — this also
            writes the first device's <code>.p12</code> to install on your phone:
            <br /><code>tclaude remote-access setup --bind 0.0.0.0:8443</code><br />
            The toggle below only takes effect once that material exists; enabling without it
            is a no-op. Add more devices later with <code>tclaude remote-access add-client ${'<name>'}</code>.`} />
        <${Setting} title="Remote dashboard" forId="cfg-remote-enabled" k="remote_access.enabled" badges=${['restart']}
          summary="Off by default. Starting / stopping the listener takes effect after an agentd restart.">
          <${Toggle} id="cfg-remote-enabled" />
        </${Setting}>
        <${Setting} title="Listen interface" forId="cfg-remote-host" k="remote_access.bind" child
          summary=${html`<code>0.0.0.0</code> = reachable on the LAN; a tailnet interface IP for a mesh VPN; <code>127.0.0.1</code> when a tunnel (Cloudflare / ngrok) terminates the cert. Empty = <code>0.0.0.0</code>.`}>
          <${ConfigInput} type="text" id="cfg-remote-host" list="cfg-remote-host-list" placeholder="0.0.0.0" aria-label="Remote access listen interface" autocomplete="off" spellcheck="false" style="min-width:140px" />
          <datalist id="cfg-remote-host-list">
            <option value="0.0.0.0">all interfaces — LAN</option>
            <option value="127.0.0.1">loopback only — behind a tunnel</option>
          </datalist>
        </${Setting}>
        <${Setting} title="HTTPS port" forId="cfg-remote-port" k="remote_access.bind" child
          summary=${html`The port the remote dashboard listens on. Dial <code>https://${'<host>'}:${'<port>'}</code> from the device.`}>
          <${ConfigInput} type="number" id="cfg-remote-port" min="1" max="65535" placeholder="8443" aria-label="Remote access HTTPS port" style="min-width:110px" />
        </${Setting}>
        <${Setting} title="Status" child>
          <span class="cfg-remote-status" id="cfg-remote-status"></span>
        </${Setting}>
      </${Group}>
      <div id="cfg-remote-admin-slot"></div>
    </${Panel}>

    <${Panel} id="logs" active=${active}>
      <${Group} title="Logging">
        <${Setting} title="Log level" forId="cfg-log-level" k="log_level" summary="Verbosity of agentd's output.log.">
          <${ConfigSelect} id="cfg-log-level" aria-label="Log level">
            <option value="debug">debug</option>
            <option value="info">info</option>
            <option value="warn">warn</option>
            <option value="error">error</option>
          </${ConfigSelect}>
        </${Setting}>
        <${Setting} title="Startup timings" forId="cfg-startup-timing" k="startup_timing"
          summary="log agent startup timings"
          more="Applies to new timing traces as soon as you save; no restart needed. Traces already running finish. Filter Logs for “startup timing” (info level). Saving this setting overrides TCLAUDE_STARTUP_TIMING.">
          <${Toggle} id="cfg-startup-timing" />
        </${Setting}>
        <${Setting} title="Record hooks" forId="cfg-record-hooks" k="record_hooks"
          summary="log every hook callback payload"
          more="Debugging aid — verbose; leave off for normal use.">
          <${Toggle} id="cfg-record-hooks" />
        </${Setting}>
      </${Group}>

      <${Group} title="Log rotation">
        <${Setting} title="Max log size" forId="cfg-logrot-maxsize" k="log_rotation.max_size" badges=${['restart']}
          summary=${html`Size cap for <code>~/.tclaude/data/output.log</code> before <code>agentd</code> rotates it (<code>10MiB</code>, <code>50m</code>, <code>500k</code>). Empty = built-in default (10 MiB). <code>0</code> disables rotation.`}>
          <${ConfigInput} type="text" id="cfg-logrot-maxsize" placeholder="10MiB" aria-label="Log rotation max size" autocomplete="off" spellcheck="false" style="min-width:110px" />
        </${Setting}>
        <${Setting} title="Keep" k="log_rotation.keep" badges=${['restart']}
          summary=${html`How many rotated logs (<code>output.log.1</code> … <code>.N</code>) <code>agentd</code> retains. Empty or <code>0</code> = built-in default (5).`}>
          <${ConfigInput} type="number" id="cfg-logrot-keep" min="0" placeholder="5" aria-label="Log rotation keep count" />
          <span>rotated files</span>
        </${Setting}>
      </${Group}>

      <${Group} title="Retired-agent cleanup"
        lede=${html`Optional long-horizon housekeeping that <strong>permanently deletes</strong> agents/conversations
          once they have been <em>retired</em> for a long time — reclaiming the Retired tab, mailbox queries
          and disk that retired entities (e.g. export summary-writer clones) otherwise hold forever.
          <strong>Off by default</strong>; retire stays the non-destructive half of cleanup. Deleting a
          conversation does <strong>not</strong> lose its cost — spend totals survive deletion.`}>
        <${Setting} title="Retired-agent auto-cleanup" k="agent.retired_cleanup" badges=${['irreversible']}
          summary=${html`agentd sweeps every 30${' '}min (and at startup). Empty = built-in default (365 ≈ 1 year).`}
          more="Irreversible — choose a window long enough that anything still wanted has been reinstated.">
          <label class="cfg-inline"><${ConfigInput} type="checkbox" id="cfg-agent-retired-cleanup-enabled" /> delete agents retired longer than</label>
          <${ConfigInput} type="number" id="cfg-agent-retired-cleanup-days" min="1" max="36525" placeholder="365" aria-label="Days retired before deletion" style="min-width:80px" />
          <span>days</span>
        </${Setting}>
      </${Group}>
    </${Panel}>

    <${Panel} id="steward" active=${active}>
      <${Group} title="Access" lede="Set when a Steward is summoned, so a running Steward keeps what it was born with: retire it to apply a change. Anything beyond this asks you first. Agents can reply to a Steward that wrote to them; writing to it first needs message.direct or a shared group.">
        <${Setting} title="Reads" forId="cfg-steward-reads" k="steward.reads"
          summary="What a Steward can read."
          more="Every level lists all agents and groups, as you see them, and reads boards and the dashboard view. Operator level adds processes, triggers, usage, costs, host load, federation activity and agents' context state. Reads never include other agents' mail through the API, private setup text, or GitHub, Linear and AWB through your credentials. At every level a Steward can capture your dashboard, so it sees what is on screen, including any open messages, and you see a notice each time.">
          <${ConfigSelect} id="cfg-steward-reads" aria-label="What a Steward can read">
            <option value="operator">Operator level (default)</option>
            <option value="fleet">Fleet and boards only</option>
          </${ConfigSelect}>
        </${Setting}>
        <${Setting} title="Writes" forId="cfg-steward-writes" k="steward.writes"
          summary="What a Steward can change without asking."
          more="Every level can draw on boards, point at the dashboard and notify you. Messaging also lets it message any agent. Starting workers adds spawning agents and setting their task link, and the workers it starts run on your account.">
          <${ConfigSelect} id="cfg-steward-writes" aria-label="What a Steward can change without asking">
            <option value="messaging">Message agents (default)</option>
            <option value="none">Boards and notifying you only</option>
            <option value="workers">Message agents and start workers</option>
          </${ConfigSelect}>
        </${Setting}>
        <${Setting} title="Default permissions"
          summary="What a new Steward gets beyond Reads and Writes, or is denied from them. Saved from the editor, not with this form. A running Steward keeps what it has.">
          <${StewardDefaultPermissions} />
        </${Setting}>
      </${Group}>
      <${Group} title="Launch">
        <${Setting} title="Spawn profile" forId="cfg-steward-profile" k="steward.profile"
          summary="The spawn profile a new Steward launches with. Unset, it uses the global default profile.">
          <${ConfigSelect} id="cfg-steward-profile" aria-label="Steward spawn profile" style="min-width:160px"></${ConfigSelect}>
        </${Setting}>
      </${Group}>
      <${Group} title="Knowledge base">
        <${Setting} title="Source download" forId="cfg-steward-source-download" k="steward.source_download"
          summary="Whether tclaude's source is downloaded for a Steward to read. The docs are always there, built in."
          more="Comes from github.com/tofutools/tclaude, in the background with a progress bar, and never without your say. Only the running version is kept, and a version already held is not fetched again. Ask puts the question once for each new version. An agent can't change this or start a download.">
          <${ConfigSelect} id="cfg-steward-source-download" aria-label="Source download">
            <option value="ask">Ask for each new version</option>
            <option value="each_version">Download each new version</option>
            <option value="never">Never</option>
          </${ConfigSelect}>
        </${Setting}>
        <${KnowledgeSourceSetting} />
      </${Group}>
      <${Group} title="Name and startup">
        <${Setting} title="Name" forId="cfg-steward-name" k="steward.name"
          summary="What you call your Steward. Shown in the companion, and agents can address it by this name as well as by steward. A running Steward is told when it changes.">
          <${ConfigInput} type="text" id="cfg-steward-name" maxlength="32" spellcheck="false" aria-label="Steward name" placeholder="Steward" />
        </${Setting}>
        <${Setting} title="Startup prompt" forId="cfg-steward-startup-prompt" wide k="steward.startup_prompt"
          summary="Your own instructions, added to what every new Steward is told when it starts. A running Steward keeps the prompt it started with.">
          <${ConfigTextarea} id="cfg-steward-startup-prompt" class="cfg-json" rows="6" maxlength="10000" aria-label="Steward startup prompt"
            placeholder="Optional. For example: Reply in Swedish. Start every brief with open pull requests."></${ConfigTextarea}>
        </${Setting}>
      </${Group}>
      <${ExternalStewardGroup} loads=${loads} />
    </${Panel}>

    <${Panel} id="experimental" active=${active}>
      <${Group} title="Feature flags"
        lede=${html`Opt-in flags for features under active development. Most ship dark on
          <code>main</code> — off by default and invisible in normal use — so they can land in
          small increments without a long-lived feature branch, and a few stay listed while on by default. Expect rough edges while a
          flag is listed here; when a feature graduates, its flag disappears.`}>
        <${Setting} title="Processes" forId="cfg-feature-processes" k="features.processes"
          summary=${html`enable the in-development <strong>Processes</strong> feature`}
          more="Repeatable process graphs run by agents, programs and you: a template editor with an AI scribe, runs scoped to a group or an agent, and run views in Groups and Terminals. Off by default while it is tested and iterated on; the tab, the Groups and Terminals process views, the process API and the run engine all stay absent until enabled.">
          <${Toggle} id="cfg-feature-processes" />
        </${Setting}>
        <${Setting} title="Collab" forId="cfg-feature-collab" k="features.collab" badges=${['refresh']}
          summary=${html`enable the in-development <strong>Collab</strong> surfaces: the companion window, the Steward and shared boards`}
          more="The floating companion window for talking to agents from any view, with the Steward as your helper for all of tclaude; shared boards you draw on with agents; the fleet brief and map; decisions agents put to you with options; and agents pointing at the dashboard for you. Off by default while it is tested and iterated on; these views stay hidden and their API and CLI commands answer with a disabled message until enabled.">
          <${Toggle} id="cfg-feature-collab" />
        </${Setting}>
        <${Setting} title="Triggers" forId="cfg-feature-triggers" k="features.triggers" badges=${['refresh']}
          summary=${html`enable the in-development <strong>Triggers</strong> automation feature`}
          more="PR-opened rules with bounded spawn and message actions. Off by default; the Automations subview, Groups projection, engine, and API stay absent until enabled.">
          <${Toggle} id="cfg-feature-triggers" />
        </${Setting}>
        <${Setting} title="Groups Route map" forId="cfg-feature-groups-route-map" k="features.groups_route_map" badges=${['refresh']}
          summary=${html`enable the opt-in <strong>Members | Route map</strong> Groups view`}
          more="Read-only agent route leases and health for selected groups. Off by default; the route map and its snapshot projection stay absent until enabled.">
          <${Toggle} id="cfg-feature-groups-route-map" />
        </${Setting}>
        <${Setting} title="Group attachments" forId="cfg-feature-group-attachments" k="features.group_attachments" badges=${['refresh']}
          summary="How the group paperclip (attachment link) is presented, if at all."
          more=${html`Off by default while this interaction is refined. <strong>Float</strong> shows the paperclip overlay only while the title is hovered. <strong>Fixed</strong> keeps the paperclip as the last group quick item, marks a saved link with a small dot while its label is folded, and folds that label with the other quick details; only its edit pencil appears on hover. Turning the feature off only hides the controls—existing stored attachments remain intact.`}>
          <${ConfigSelect} id="cfg-feature-group-attachments" aria-label="Group attachment presentation">
            <option value="off">Off</option>
            <option value="float">Float above group title</option>
            <option value="fixed">Fixed group quick item</option>
          </${ConfigSelect}>
        </${Setting}>
        <${Setting} title="Command palette in terminals" forId="cfg-feature-terminal-command-palette-shortcut" k="features.terminal_command_palette_shortcut" badges=${['refresh']}
          summary=${html`let <strong>Ctrl/Cmd+K</strong> open the command palette while a web terminal has focus`}
          more="Off by default because Ctrl/Cmd+K clears the current input line in the supported harnesses. When enabled, the dashboard claims that chord inside web terminals instead.">
          <${Toggle} id="cfg-feature-terminal-command-palette-shortcut" />
        </${Setting}>
        <${Setting} title="Recorded sandbox details" forId="cfg-feature-recorded-sandbox-details" k="features.recorded_sandbox_details" badges=${['refresh']}
          summary="show the details arrow beside each agent's sandbox lock or warning icon"
          more=${html`Off by default to keep the harness line compact. The lock or warning and its summary tooltip remain visible; enabling this adds the adjacent <strong>›</strong> action for the full recorded launch details.`}>
          <${Toggle} id="cfg-feature-recorded-sandbox-details" />
        </${Setting}>
        <${Setting} title="Agent dirs: mount parent" forId="cfg-feature-agent-dirs-mount-parent" k="features.agent_dirs_mount_parent" badges=${['launch']}
          summary=${html`mount the shared parent root of <strong>agent-owned directories</strong> read-write`}
          more="On by default: the shared parent root is granted once, so the agent can create, rewrite, and delete its own env-var'd directories. Uncheck to opt out and restore per-directory grants — the agent can write inside each directory but cannot delete it.">
          <${Toggle} id="cfg-feature-agent-dirs-mount-parent" />
        </${Setting}>
      </${Group}>
      <${Group} title="Processes">
        <${Setting} title="Refuse unsigned workflows" forId="cfg-processes-refuse-unsigned" k="processes.refuse_unsigned_imports"
          summary=${html`refuse to import a <code>.pwf</code> workflow file without a valid signature`}
          more="The import review still shows such a file and says why it is refused. A valid signature from a key this node does not know passes: the file is unchanged since signing, but the signer is not known. Off by default.">
          <${Toggle} id="cfg-processes-refuse-unsigned" />
        </${Setting}>
      </${Group}>
    </${Panel}>

    <${Panel} id="advanced" active=${active}>
      <${Group} title="Sudo configuration">
        <${Setting} title="Sudo (raw JSON)" wide k="agent.sudo"
          summary=${html`The <code>agent.sudo</code> block (time-bounded permission elevations, with optional
            per-conversation overrides) is edited as raw JSON — it is the one nested-map structure
            without a dedicated form. Leave blank for none. Validated on save.`}>
          <${ConfigTextarea} id="cfg-sudo-json" class="cfg-json" spellcheck="false" aria-label="Advanced sudo configuration JSON" placeholder="JSON object"></${ConfigTextarea}>
        </${Setting}>
      </${Group}>
    </${Panel}>

    <datalist id="cfg-slug-list"></datalist>
    <datalist id="cfg-group-list"></datalist>
    <datalist id="cfg-state-list">
      <option value="*"></option><option value="idle"></option>
      <option value="working"></option><option value="awaiting_permission"></option>
      <option value="awaiting_input"></option><option value="error"></option>
      <option value="exited"></option>
    </datalist>

    <${FormFillBar} form="config" generation=${loads} />
    <div class="cfg-footer">
      <${SaveSummary} dirtyPanels=${dirtyPanels} />
      <span class="cfg-status" id="cfg-status">loading…</span>
      <span class="spacer"></span>
      <${ConfigButton} type="button" id="cfg-reload">Reload</${ConfigButton}>
      <${ConfigButton} type="button" id="cfg-save" class="primary">Save changes…</${ConfigButton}>
    </div>

    </div>
    </div>
    </div>
    </${ConfigEventContext.Provider}>

  `;
}
