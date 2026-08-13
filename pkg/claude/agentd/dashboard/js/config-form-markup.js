import { createContext, h } from 'preact';
import { useContext, useState } from 'preact/hooks';
import htm from 'htm';
import { browserNotifyPermission, requestBrowserNotifyPermission } from './browser-notify.js';

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
    return html`<span class="cfg-hint" style="padding-left:0">This browser cannot raise notifications here (needs https or localhost).</span>`;
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

// Static form structure mechanically migrated from dashboard.html. State,
// conditional enablement, lists, validation, and actions are owned by the
// Config island; keeping the verbose operator guidance here preserves parity.
export function ConfigFormMarkup({ lists = {}, onListChange = () => {}, onFormEvent = () => {} }) {
  return html`
    <${ConfigEventContext.Provider} value=${onFormEvent}>
    <div class="config-form">

    <h2 class="cfg-wizard-title" aria-hidden="true">📜 The Wizard's Almanac</h2>
    <p class="cfg-intro">
      Visual editor for <code id="cfg-path">~/.tclaude/config.json</code>. It covers the
      settings this version of tclaude recognizes. Edits stay in this form until you press
      <strong>Save changes</strong> — which shows a diff to confirm before anything is
      written. Most settings apply on next use; a few resolved when <code>agentd</code>
      starts (spawn rate-limit, clone cooldown, log rotation) take effect after an
      agentd restart.
    </p>
    <div id="cfg-notice" class="cfg-notice" style="display:none"></div>
    <div id="cfg-errors" class="cfg-errors" style="display:none"></div>


    <div class="filter-bar cfg-filter-bar">
      <${ConfigInput} type="search" id="cfg-filter" placeholder="Filter settings by title or content…"
        aria-label="Filter config settings" autocomplete="off" spellcheck="false" />
      <span class="filter-count" id="cfg-filter-count"></span>
      <${ConfigButton} type="button" class="clear-filter" id="cfg-filter-clear" hidden>clear ×</${ConfigButton}>
      <span class="spacer"></span>
      <span class="cfg-filter-note">Filtering only hides sections — every setting still saves.</span>
    </div>
    <div class="cfg-filter-empty" id="cfg-filter-empty" hidden>No settings match “<span id="cfg-filter-empty-q"></span>”.</div>

    <div class="cfg-section">
      <h3>Terminals & windows</h3>
      <div class="cfg-field">
        <span class="cfg-label">Terminal</span>
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
        <span class="cfg-hint">Terminal emulator the dashboard's spawn auto-focus / shell-attach opens. Empty = auto-detect.</span>
      </div>
      <div class="cfg-field">
        <span class="cfg-label">Default terminal</span>
        <label class="cfg-inline"><${ConfigInput} type="checkbox" id="cfg-dashboard-default-web-terminal" /> open focus / windows / terminals as web terminals in the dashboard</label>
        <span class="cfg-hint">The spawn dialog's <strong>Auto focus</strong>, per-agent <strong>focus</strong> (👁), <strong>open window</strong> and <strong>open terminal</strong> actions — plus bulk focus from the 🪟 <em>windows…</em> modal, the same commands in the ⌘ palette, a click on a CWD path cell, and a message's <em>focus</em> button — normally pop a <em>native</em> OS terminal window. <strong>Checked</strong> routes them all to <em>in-browser</em> terminal panes in the dashboard's <strong>Terminals</strong> tab instead — the same surface the dedicated "web term" / "web window" buttons always use — so you never leave the browser. Bulk unfocus still detaches the selected terminal clients. The dedicated web buttons are already always web. Off by default. Stored as <code>dashboard.default_terminal</code> (the default <code>native</code> is omitted; checked writes <code>web</code>).</span>
      </div>
      <div class="cfg-field">
        <span class="cfg-label">Window focus</span>
        <label class="cfg-inline"><${ConfigInput} type="checkbox" id="cfg-focus-window-title" /> set the <code>tclaude:${'<id>'}</code> window/tab title</label>
        <span class="cfg-hint">tclaude normally stamps a <code>tclaude:${'<id>'}</code> title on each agent's terminal window/tab. That title is how it finds an agent's <em>existing</em> window to <strong>raise</strong> it (window focus) and to <strong>auto-tile</strong>. On a plain desktop terminal some find it ugly — <strong>uncheck</strong> to leave the terminal's own tab title alone. Trade-off: focus/tiling then can't locate the window, so "focus" falls back to opening a <em>new</em> window instead of raising the existing one (affects WSL and native-Linux/X11; the explicit "open window" action still works). <strong>Leave on for WSL</strong>, where window focus depends on it. On (default). Stored as <code>focus.window_title</code> (the default is omitted; unchecked writes <code>false</code>).</span>
        <label class="cfg-inline"><${ConfigInput} type="checkbox" id="cfg-focus-raiseonly" /> raise existing windows only (never open a fresh one)</label>
        <span class="cfg-hint">When on, focusing an agent that has no open window does nothing, instead of opening a new terminal attached to it. Use a row's "open window" action to open one explicitly. Off (default) = open-on-focus.</span>
        <label class="cfg-inline"><${ConfigInput} type="checkbox" id="cfg-focus-tile" /> auto-tile windows after a bulk “focus”</label>
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
        <span class="cfg-hint">When on, using the 🪟 windows… modal / command palette to focus <em>more than one</em> agent's window rearranges them into a tidy layout instead of leaving them where the OS dropped them. All windows are gathered onto <em>one</em> monitor — the one the first window is on — so a multi-monitor setup isn't scattered. By default windows keep their <em>current size</em> and are only repositioned; tick <strong>resize windows to fill the screen</strong> for the older screen-filling grid. Best-effort per platform (macOS AppleScript, Linux xdotool/kdotool, WSL PowerShell); an unsupported desktop leaves windows as-is. <strong>Gap</strong> = pixels between tiles; <strong>margin</strong> = inset from the screen edges. A single focused window is never tiled. Off (default).</span>
      </div>
      <div class="cfg-field cfg-terminal-attach-field">
        <div class="cfg-terminal-attach-intro">
          <strong>Web terminal attachment</strong>
          <span>Choose how the browser establishes terminal geometry when attaching to tmux.</span>
        </div>
        <span class="cfg-label">Resize strategy</span>
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
        <span class="cfg-hint cfg-terminal-attach-hint"><strong>Repair after attach</strong> preserves today's three-step sequence. <strong>Initial resize only</strong> skips the visible nudge and restore. <strong>Resize before attach</strong> waits between sending the fitted grid and starting the tmux attachment. Stored under <code>dashboard.terminal_attach</code>.</span>
      </div>
      <div class="cfg-field">
        <span class="cfg-label">Agent hide button</span>
        <label class="cfg-inline"><${ConfigInput} type="checkbox" id="cfg-dashboard-show-agent-hide-btn" /> show the per-agent "hide window" button next to "focus"</label>
        <span class="cfg-hint">Each agent row opens with two eye icons: <strong>focus</strong> (👁, raise the agent's terminal window) and <strong>hide</strong> (the slashed-eye, detach the window — the agent keeps running). Hiding a window is far less used than focusing it, so the dashboard drops the hide icon <em>by default</em> to keep the quick-control cluster tight — only <strong>focus</strong> shows. <strong>Checked</strong> brings the <strong>hide</strong> icon back for anyone who wants the row shortcut. Detaching is always possible from the terminal itself; this is only the per-row button. Off by default. Stored as <code>dashboard.show_agent_hide_button</code> (the default is omitted; checked writes <code>true</code>).</span>
      </div>
      <div class="cfg-field">
        <span class="cfg-label">TUI color scheme</span>
        <${ConfigSelect} id="cfg-tui-color-scheme" aria-label="Terminal UI color scheme">
          <option value="default">Default</option>
          <option value="dark-high-contrast">Dark mode (high contrast)</option>
        </${ConfigSelect}>
        <span class="cfg-hint">The color palette tclaude's interactive terminal views — <code>tclaude session ls -w</code>, <code>conv ls -w</code>, and <code>agent inbox -w</code> — render with. <strong>Default</strong> is tuned to stay readable on both light and dark terminals (a little dimmer on a dark background). <strong>Dark mode (high contrast)</strong> restores the brighter earlier palette (vivid yellow / green / red and a brighter header) for higher contrast on a dark terminal, at the cost of light-terminal readability. Applies to newly launched watch views. Stored as <code>tui.color_scheme</code> (the default is omitted).</span>
      </div>
    </div>

    <div class="cfg-section">
      <h3>Dashboard</h3>
      <div class="cfg-field">
        <span class="cfg-label">Activity bots — regular mode</span>
        <${ConfigSelect} id="cfg-dashboard-activity-bots-regular" aria-label="Activity bot style in regular mode">
          <option value="emoji">Emoji bots</option>
          <option value="sprites">Pixel sprites</option>
          <option value="off">Off</option>
        </${ConfigSelect}>
        <span class="cfg-hint">Each group header (and the top bar) shows a little row of robot icons — one per <em>status</em> present among its members, deduped: a bot <strong>dancing</strong> when an agent is working, standing still when idle, raising a ❓ when one awaits input, and so on. A <em>folded</em> group rolls up every agent hidden in its nested groups; unfold it and those bots move down to the visible child headers. <strong>Emoji bots</strong> are lightweight emoji+CSS; <strong>Pixel sprites</strong> are animated pixel-art robots; <strong>Off</strong> hides them. This is the plain dashboard's style (default <em>Emoji bots</em>). Stored as <code>dashboard.activity_bots.regular</code>.</span>
      </div>
      <div class="cfg-field">
        <span class="cfg-label">Activity bots — wizard mode 🧙</span>
        <${ConfigSelect} id="cfg-dashboard-activity-bots-wizard" aria-label="Activity bot style in wizard mode">
          <option value="emoji">Wizard glyphs</option>
          <option value="sprites">Pixel sprites</option>
          <option value="off">Off</option>
        </${ConfigSelect}>
        <span class="cfg-hint">The same indicator's style when the dashboard's <strong>wizard</strong> mode (the 🧙 in the header) is on — chosen independently of the other modes. Default <em>Wizard glyphs</em> (the fantasy 🧙🕯️📜💥 row). Switch to <em>Pixel sprites</em> to swap in the animated <strong>wizard</strong> spellcasters (casting when working, pondering when awaiting, a backfiring spell on error, an impatient staff-tap when idle) instead, or turn them <em>Off</em>. (Reduced-motion browsers get the bots <em>without</em> animation either way.) Stored as <code>dashboard.activity_bots.wizard</code>.</span>
      </div>
      <div class="cfg-field">
        <span class="cfg-label">Plugins tab</span>
        <label class="cfg-inline"><${ConfigInput} type="checkbox" id="cfg-dashboard-always-show-plugins" /> always show the Plugins tab, even with no plugins installed</label>
        <span class="cfg-hint">The <strong>Plugins</strong> tab manages human-defined integration bundles (e.g. a docker-backed MCP server). By default it <em>auto-hides</em> when you have no plugins installed — most setups never define one, so the empty tab is just clutter. Enable this to keep it visible regardless, e.g. to reach the built-in catalog and install one from there. (A broken <code>plugins.json</code> always shows the tab so the error isn't hidden.) Off by default. Stored as <code>dashboard.always_show_plugins_tab</code>.</span>
      </div>
      <div class="cfg-field">
        <span class="cfg-label">Open PRs indicator</span>
        <label class="cfg-inline"><${ConfigInput} type="checkbox" id="cfg-dashboard-always-show-open-prs" /> always show the footer's Open PRs indicator, even at zero</label>
        <label class="cfg-inline">show pull requests merged or closed in the last <${ConfigInput} type="number" id="cfg-dashboard-recent-pr-window-days" min="0" max="30" step="1" placeholder="3" aria-label="Recently closed pull request window (days)" style="width:6em" /> days</label>
        <span class="cfg-hint">The bottom-right <strong>Open PRs</strong> indicator opens a popover listing the pull requests <em>you</em> have open across every repository, with CI state and the agent working each one. <strong>Checked</strong> (default) keeps it in the footer permanently, so it stays a fixed entry point (and reaches the recently-closed list) even when nothing is open; unchecked hides it whenever you have no open PRs. It is always hidden until the daemon has resolved a GitHub identity — with no <code>gh</code> login there is nothing to count. The <strong>days</strong> field adds a separate <em>Closed Nd</em> filter in the popover listing your recently merged and closed PRs; it never mixes into the open list or the count. Default 3 days, max 30; <strong>0</strong> hides the filter, while the shared dashboard PR poll still checks a one-day terminal window to keep Groups badges current. Stored as <code>dashboard.always_show_open_prs</code> (default true, so only <code>false</code> is written) and <code>dashboard.recent_pr_window_days</code> (the default is omitted).</span>
      </div>
      <div class="cfg-field">
        <span class="cfg-label">Horizontal scroll</span>
        <label class="cfg-inline"><${ConfigInput} type="checkbox" id="cfg-dashboard-hscroll-follow" /> keep the header & tabs pinned while scrolled sideways (follow mode)</label>
        <span class="cfg-hint">On a window too narrow for a wide member table, the page scrolls sideways. The full-bleed bars (header, tabs, slop marquee) always stretch to fill the width so they never look ragged — this controls their <em>content</em>. <strong>Follow</strong> (default, checked) pins the header controls and tab strip to the viewport and makes them sticky-left, so they stay put and usable while you're scrolled right. <strong>Static</strong> (unchecked) lets that content scroll off with the page — the bars still fill the width, but the controls aren't reachable until you scroll back. Stored as <code>dashboard.hscroll_follow</code> (the default is omitted; static writes <code>false</code>).</span>
      </div>
      <div class="cfg-field">
        <span class="cfg-label">Group quick options</span>
        <label class="cfg-inline"><${ConfigInput} type="checkbox" id="cfg-dashboard-group-quick-fold" /> auto-fold quick options to icons, expand on hover</label>
        <span class="cfg-hint">Each group header carries a row of editable "quick option" chips — 📝 description, 📁 default dir, 🧠 default profile, 🔗 links. They can grow the header wide. <strong>Checked</strong> (default) folds them to <em>icon-only</em> at rest and slides the text open when you hover the group header — a horizontal accordion that reclaims the space. The group name, activity bots and 👥 member count always stay visible. <strong>Unchecked</strong> keeps the full chips always shown (the original look). Touch devices (which can't hover) always see the full chips regardless. You can also <em>pin</em> a single group's chips open from its ⚙ menu (📌 pin quick options) so it stays expanded even with auto-fold on. Stored as <code>dashboard.group_quick_options</code> (the default <code>hover</code> is omitted; unchecked writes <code>expanded</code>).</span>
      </div>
      <div class="cfg-field">
        <span class="cfg-label">Directory picker</span>
        <label class="cfg-inline"><${ConfigInput} type="checkbox" id="cfg-dashboard-default-web-directory-picker" /> use the in-dashboard directory picker on localhost too</label>
        <span class="cfg-hint">Directory fields automatically open an in-browser navigator when the dashboard is reached remotely, because a native chooser would appear on the agentd host where it cannot be operated. <strong>Checked</strong> uses that same web picker for loopback/localhost dashboards; unchecked keeps the native OS chooser locally. Stored as <code>dashboard.default_directory_picker</code> (the default <code>native</code> is omitted; checked writes <code>web</code>).</span>
      </div>
      <div class="cfg-field">
        <span class="cfg-label">Group descriptions</span>
        <label class="cfg-inline"><${ConfigInput} type="checkbox" id="cfg-dashboard-show-group-description" /> show the 📝 description chip in each group header</label>
        <span class="cfg-hint">Every group can carry a short <strong>description</strong> — the 📝 chip beside the group name, click-to-edit. It's a <em>display-only</em> label: nothing in tclaude reads it, so it's a <strong>deprecated</strong> feature that mostly just clutters the header. The dashboard hides the chip <em>by default</em>; existing descriptions are kept in the database but not shown. <strong>Checked</strong> brings the chip back (and with it the only way to view or edit a group's description). Off by default. Stored as <code>dashboard.show_group_description</code> (the default is omitted; checked writes <code>true</code>).</span>
      </div>
      <div class="cfg-field">
        <span class="cfg-label">Debug tab</span>
        <label class="cfg-inline"><${ConfigInput} type="checkbox" id="cfg-dashboard-show-debug-tab" /> show the Debug tab (daemon poll-timing diagnostics)</label>
        <span class="cfg-hint">The <strong>Debug</strong> tab shows how long the dashboard's own background polls take inside the daemon — a latency sparkline + p50/p90/p99/max per endpoint, and the <code>/api/snapshot</code> handler's per-phase breakdown — for chasing a slow dashboard. It is a troubleshooting surface, so the nav hides it <em>by default</em>. <strong>Checked</strong> brings it back. The timing recorder runs either way (a few hundred KB of daemon memory), so the history is already there when you switch the tab on. Off by default. Stored as <code>dashboard.show_debug_tab</code> (the default is omitted; checked writes <code>true</code>).</span>
      </div>
    </div>

    <div class="cfg-section">
      <h3>Usage, costs & rate limits</h3>
      <div class="cfg-field">
        <span class="cfg-label">Cost display multiplier</span>
        <${ConfigInput} type="number" id="cfg-cost-factor" min="0" max="10" step="0.01" placeholder="1.0" aria-label="Cost display multiplier" style="min-width:120px" />
        <span class="cfg-hint">Scales every <em>displayed</em> cost figure — the per-agent badge, the Costs tab, and the top-bar month-to-date / today figures. Use a value such as <code>1.1</code> only when you intentionally want a 10% display compensation. Recorded data is never changed, so resetting to <code>1</code> restores raw figures. Empty or <code>1</code> = no adjustment. Also editable live on the Costs tab.</span>
      </div>
      <div class="cfg-field">
        <span class="cfg-label">Show cost on subscription</span>
        <label class="cfg-inline"><${ConfigInput} type="checkbox" id="cfg-cost-show-on-subscription" /> show the Costs tab + per-agent cost on a subscription (WHAT-IF)</label>
        <span class="cfg-hint">On a subscription there's no pay-per-token charge, so hypothetical costs are hidden by default. Enable this to show harness-provided <strong>WHAT-IF</strong> estimates of equivalent pay-per-token pricing, clearly separated from real billed spend. A mixed installation can show real and WHAT-IF rows together; pay-per-token spend is always shown regardless of this setting.</span>
      </div>
      <div class="cfg-field">
        <span class="cfg-label">OpenCode legacy long-context cutoff</span>
        <${ConfigInput} type="number" id="cfg-opencode-legacy-long-context-pricing-cutoff" min="1" step="1" placeholder="272000" aria-label="OpenCode legacy long-context pricing cutoff" style="min-width:140px" />
        <span>context tokens per model call</span>
        <span class="cfg-hint">Controls when OpenCode's legacy <code>experimentalOver200K</code> catalog price starts applying to WHAT-IF costs. The legacy rate is selected only when a model call exceeds this boundary; exactly at the boundary stays on the base rate. Explicit context tiers from OpenCode's provider catalog always take precedence. Blank = <code>272000</code>. Stored as <code>opencode.legacy_long_context_pricing_cutoff</code>.</span>
      </div>
      <div class="cfg-field">
        <span class="cfg-label">Poll Anthropic usage API</span>
        <label class="cfg-inline"><${ConfigInput} type="checkbox" id="cfg-usage-poll-anthropic-api" /> refresh Claude usage in the background</label>
        <span class="cfg-hint">Off by default. The top-bar <strong>Claude</strong> subscription usage bars are normally refreshed by Claude Code's statusline callback while sessions run, then kept from the last cached reading. Enable this only if you want agentd to periodically call Anthropic's usage API while no statusline callback is active. Stored as <code>usage.poll_anthropic_api</code>.</span>
      </div>
      <div class="cfg-field">
        <span class="cfg-label">Idle timeout</span>
        <${ConfigInput} type="text" id="cfg-usage-idle-timeout" placeholder="72h" aria-label="Usage readout idle timeout" style="min-width:120px" />
        <span class="cfg-hint">How long the top-bar <strong>Claude</strong> subscription usage bars (5h / 7d) keep showing their last-known reading after fresh data stops arriving. Fresh data comes from Claude Code's statusline callback, and optionally from the Anthropic usage API poll above. A Go duration — e.g. <code>72h</code> (3 days, the default), <code>30m</code>, <code>2h30m</code>. Blank = default. Stored as <code>usage.idle_timeout</code>.</span>
      </div>
      <div class="cfg-field">
        <span class="cfg-label">Rate-limit gating</span>
        <label class="cfg-inline"><${ConfigInput} type="checkbox" id="cfg-ratelimit-enabled" /> enabled</label>
        <span class="cfg-hint">When off, no rate-limit thresholds are configured.</span>
      </div>
      <div class="cfg-field">
        <span class="cfg-label">5-hour window</span>
        <${ConfigInput} type="number" id="cfg-ratelimit-5h" min="0" max="100" step="0.1" placeholder="99" aria-label="Rate limit 5-hour window percent" />
        <span>% max used</span>
      </div>
      <div class="cfg-field">
        <span class="cfg-label">7-day window</span>
        <${ConfigInput} type="number" id="cfg-ratelimit-7d" min="0" max="100" step="0.1" placeholder="99.9" aria-label="Rate limit 7-day window percent" />
        <span>% max used</span>
      </div>
    </div>

    <div class="cfg-section">
      <h3>Context & compaction</h3>
      <div class="cfg-field">
        <span class="cfg-label">Pre-compact guard</span>
        <label class="cfg-inline"><${ConfigInput} type="checkbox" id="cfg-precompact-enabled" /> enabled</label>
        <label class="cfg-inline"><${ConfigInput} type="checkbox" id="cfg-precompact-blockmanual" /> also block manual <code>/compact</code></label>
        <span class="cfg-hint">Refuses Claude Code's <em>automatic</em> compaction until used context reaches a per-window floor — so a 1M session doesn't compact at the 200K boundary (~20% of the 1M bar). Lets context grow (and you reincarnate) first. Fails open when usage is unknown. By default only auto-compaction is blocked, never a <code>/compact</code> you type yourself.</span>
        <span class="cfg-hint" style="padding-left:0">Floors — for each context-window size (tokens), the minimum used context (tokens) required before compaction is allowed. Empty = built-in defaults (150k of 200k, 800k of 1M).</span>
        <${ThresholdList} values=${lists['cfg-precompact-thresholds']} onChange=${onListChange} />
      </div>
      <div class="cfg-field">
        <span class="cfg-label">Resume-from-summary prompt</span>
        <label class="cfg-inline">show after
          <${ConfigInput} type="number" id="cfg-resume-minutes" min="0" placeholder="70" aria-label="Resume threshold minutes" style="width:11em" /> min idle</label>
        <label class="cfg-inline">and
          <${ConfigInput} type="number" id="cfg-resume-tokens" min="0" placeholder="100000" aria-label="Resume token threshold" style="width:11em" /> tokens</label>
        <span class="cfg-hint">Claude Code shows an interactive "Resume from summary" chooser when you resume a session that's both old <em>and</em> large — which hangs tclaude's scripted resume, since a detached pane can't answer it. These override the age / size thresholds CC uses to decide; the prompt fires only when <strong>both</strong> are exceeded, so raising <em>either</em> high enough suppresses it. <code>tclaude setup --install-resume-threshold-override</code> sets minutes to <code>525600000</code> (≈1000 years) to switch it off. Empty = Claude Code's defaults (70 min / 100k tokens); <code>0</code> always shows it. Injected as <code>CLAUDE_CODE_RESUME_*</code> env vars on tclaude-spawned <code>claude</code> panes only — never written to <code>~/.claude/settings.json</code>, so your manual <code>claude</code> runs are untouched. <strong>Note:</strong> these are <em>undocumented</em>, version-specific Claude Code env vars (verified against CC 2.1.187), so tclaude treats them as best-effort — if a future Claude Code build renames or drops them, this quietly becomes a no-op rather than an error.</span>
      </div>
      <div class="cfg-field">
        <span class="cfg-label">Context nudge</span>
        <label class="cfg-inline"><${ConfigInput} type="checkbox" id="cfg-nudge-enabled" /> enabled</label>
        <span>start</span>
        <${ConfigInput} type="number" id="cfg-nudge-min" min="0" max="100" placeholder="30" aria-label="Context nudge start percent" />
        <span>%, every</span>
        <${ConfigInput} type="number" id="cfg-nudge-interval" min="1" max="100" placeholder="10" aria-label="Context nudge interval percent" />
        <span>%</span>
        <span class="cfg-hint">"Consider reincarnating" nudge as a long-running agent's context fills.</span>
      </div>
    </div>

    <div class="cfg-section">
      <h3>Notifications</h3>
      <div class="cfg-field">
        <span class="cfg-label">Desktop notifications</span>
        <label class="cfg-inline"><${ConfigInput} type="checkbox" id="cfg-notif-enabled" /> enabled</label>
      </div>
      <div class="cfg-field">
        <span class="cfg-label">Deliver via</span>
        <${ConfigSelect} id="cfg-notif-delivery" aria-label="Notification delivery channel">
          <option value="os">Desktop (this machine)</option>
          <option value="browser">Browser (this dashboard)</option>
          <option value="both">Both</option>
        </${ConfigSelect}>
        <${BrowserNotifyPermissionButton} />
        <span class="cfg-hint">Where an already-decided notification is raised — every setting below still applies either way. <strong>Desktop</strong> is the platform notifier on the machine agentd runs on. <strong>Browser</strong> queues the banner for any open dashboard tab to raise through the Web Notification API, which is what reaches you when you are working <em>remotely</em>, and what still works when the notifying process is sandboxed away from the session D-Bus. Browser delivery needs a dashboard tab open, the permission granted (button above, once per browser), and a secure context — https or localhost. Stored as <code>notifications.delivery</code>.</span>
      </div>
      <div class="cfg-field">
        <span class="cfg-label">Cooldown</span>
        <${ConfigInput} type="number" id="cfg-notif-cooldown" min="0" placeholder="5" aria-label="Notification cooldown seconds" />
        <span>seconds between notifications</span>
      </div>
      <div class="cfg-field">
        <span class="cfg-label">Notify me when an agent…</span>
        <div class="cfg-notif-types" id="cfg-notif-types">
          <label class="cfg-inline"><${ConfigInput} type="checkbox" data-cfg-notify-type="idle" /> goes idle / finishes</label>
          <label class="cfg-inline"><${ConfigInput} type="checkbox" data-cfg-notify-type="awaiting_permission" /> needs permission</label>
          <label class="cfg-inline"><${ConfigInput} type="checkbox" data-cfg-notify-type="awaiting_input" /> awaits input</label>
          <label class="cfg-inline"><${ConfigInput} type="checkbox" data-cfg-notify-type="error" /> errors</label>
          <label class="cfg-inline"><${ConfigInput} type="checkbox" data-cfg-notify-type="exited" /> exits</label>
        </div>
        <span class="cfg-hint" style="padding-left:0">Which state changes raise a desktop banner. tclaude still records every transition — this only controls whether you get a notification.</span>
      </div>
      <div class="cfg-field">
        <span class="cfg-label">Human messages</span>
        <label class="cfg-inline"><${ConfigInput} type="checkbox" id="cfg-notif-human" /> a <code>notify-human</code> message also raises a desktop banner</label>
        <span class="cfg-hint">It always lands in the dashboard Messages tab regardless.</span>
      </div>
      <div class="cfg-field">
        <span class="cfg-label">Access requests</span>
        <label class="cfg-inline"><${ConfigInput} type="checkbox" id="cfg-agent-access-autoopen" /> open a browser when an agent asks for access</label>
        <label class="cfg-inline"><${ConfigInput} type="checkbox" id="cfg-agent-access-notify" /> raise a desktop banner when an agent asks for access</label>
        <span class="cfg-hint">Requests always appear in the dashboard Messages tab. Both extra alerts are off by default; the desktop banner also requires notifications to be enabled.</span>
      </div>
      <div class="cfg-field">
        <details class="cfg-advanced">
          <summary>Advanced: raw transition rules</summary>
          <span class="cfg-hint" style="padding-left:0">The checklist above toggles <code>*${'\u00a0'}→${'\u00a0'}state</code> rules. Add from-specific or extra rules here (use <code>*</code> as a wildcard for any state). The checklist and this list edit the same setting; an empty list means no state change notifies.</span>
          <${TransitionList} values=${lists['cfg-notif-transitions']} onChange=${onListChange} />
        </details>
      </div>
      <div class="cfg-field">
        <span class="cfg-label">Notification command</span>
        <span class="cfg-hint" style="padding-left:0">Custom command to run instead of the OS default — one argument per row. Empty = OS default.</span>
        <${StringList} id="cfg-notif-command" values=${lists['cfg-notif-command']} placeholder="argument" onChange=${onListChange} />
      </div>
    </div>

    <div class="cfg-section">
      <h3>Spawn & clone policy</h3>
      <div class="cfg-field cfg-checkbox-stack-field">
        <span class="cfg-label">Terminal startup groups</span>
        <div class="cfg-checkbox-stack">
          <label class="cfg-inline"><${ConfigInput} type="checkbox" id="cfg-session-auto-join-group" /> a bare <code>tclaude</code> joins the group configured for its directory</label>
          <label class="cfg-inline"><${ConfigInput} type="checkbox" id="cfg-session-auto-join-or-create-group" /> create and join a directory group when none exists</label>
        </div>
        <span class="cfg-hint">The launch directory and every group's <strong>default dir</strong> are compared as canonical absolute paths (including symlink resolution). Auto-join is on by default; auto-create is off. CLI flags <code>--auto-join-group[=false]</code> and <code>--auto-join-or-create-group[=false]</code> override these settings for one launch. An explicit <code>--join-group</code> always wins. Stored under <code>session</code>.</span>
      </div>
      <div class="cfg-field">
        <span class="cfg-label">Spawn restriction</span>
        <label class="cfg-inline"><${ConfigInput} type="checkbox" id="cfg-agent-spawnrestrict" /> agents may only spawn into groups they belong to</label>
        <span class="cfg-hint">Only affects spawn-capable agents; humans always bypass spawn guardrails.</span>
      </div>
      <div class="cfg-field">
        <span class="cfg-label">Normalize spawn names</span>
        <label class="cfg-inline"><${ConfigInput} type="checkbox" id="cfg-agent-spawnnormalize" /> auto-fix invalid characters in an agent name instead of rejecting it</label>
        <span class="cfg-hint">On (default): a name like <code>code reviewer!</code> becomes <code>code-reviewer</code> at spawn. Off: a name outside <code>[A-Za-z0-9_-]</code> is rejected. Applies to the spawn dialog, <code>tclaude agent spawn</code>, and the daemon.</span>
      </div>
      <div class="cfg-field">
        <span class="cfg-label">Name the tmux session</span>
        <label class="cfg-inline"><${ConfigInput} type="checkbox" id="cfg-agent-spawnlabelname" /> derive a spawned agent's session label from its name</label>
        <span class="cfg-hint">Off (default): a random <code>spwn-XXXXXX</code> label. On: the label is built from the agent's name, so <code>code-reviewer</code> attaches as <code>tclaude session attach code-reviewer</code> and appears under that name in <code>tmux ls</code>. The name is normalized and length-capped first, and a base already taken by another session gets a <code>-2</code>, <code>-3</code>, … suffix (then a random one) — so the label is derived from the name, not always identical to it. Suffixes climb while the older namesake's session row survives. Unnamed spawns keep the random label.</span>
      </div>
      <div class="cfg-field">
        <span class="cfg-label">Spawn rate limit</span>
        <label class="cfg-inline"><${ConfigInput} type="checkbox" id="cfg-agent-spawnmax-enabled" /> cap</label>
        <${ConfigInput} type="number" id="cfg-agent-spawnmax" min="0" placeholder="10" aria-label="Spawn rate limit per hour" />
        <span>spawns / agent / hour</span>
        <span class="cfg-hint">0 = unlimited. Off = built-in default (10).</span>
      </div>
      <div class="cfg-field">
        <span class="cfg-label">Clone cooldown</span>
        <${ConfigInput} type="text" id="cfg-agent-clonecooldown" placeholder="1m" aria-label="Clone cooldown" autocomplete="off" spellcheck="false" style="min-width:120px" />
        <span class="cfg-hint">Minimum gap between two agent-initiated clones of the same agent (Go duration: <code>1m</code>, <code>30s</code>, <code>0</code> to disable). Empty = built-in default.</span>
      </div>
      <div class="cfg-field">
        <span class="cfg-label">Default permissions</span>
        <span class="cfg-hint" style="padding-left:0">Permission slugs granted to every agent.</span>
        <${StringList} id="cfg-agent-permissions" values=${lists['cfg-agent-permissions']} datalist="cfg-slug-list" placeholder="permission slug" onChange=${onListChange} />
      </div>
      <div class="cfg-field">
        <span class="cfg-label">Spawn allowed groups</span>
        <span class="cfg-hint" style="padding-left:0">Extra groups a spawn-capable agent may always spawn into, on top of the restriction above.</span>
        <${StringList} id="cfg-agent-allowedgroups" values=${lists['cfg-agent-allowedgroups']} datalist="cfg-group-list" placeholder="group name" onChange=${onListChange} />
      </div>
    </div>

    <div class="cfg-section">
      <h3>Ask & scribe defaults</h3>
      <div class="cfg-field">
        <span class="cfg-label">Ask — profile</span>
        <${ConfigSelect} id="ask-profile" aria-label="Ask default profile" style="min-width:160px"></${ConfigSelect}>
        <span class="cfg-hint">Optional. Pick a saved <strong>spawn profile</strong> (from the Groups tab) to run fresh <code>tclaude ask</code> questions on its harness — the harness-independent way to ask <strong>Codex</strong> as well as Claude. When set, the profile's harness / model / effort are used and the Model / Effort below are ignored. Leave as <em>(none)</em> to use Claude with the Model / Effort below. <span id="ask-profile-state" class="cfg-hint" style="display:block"></span></span>
      </div>
      <div class="cfg-field">
        <span class="cfg-label">Ask — model</span>
        <${ConfigSelect} id="ask-model" aria-label="Ask default model" style="min-width:160px"></${ConfigSelect}>
      </div>
      <div class="cfg-field">
        <span class="cfg-label">Ask — effort</span>
        <${ConfigSelect} id="ask-effort" aria-label="Ask default effort" style="min-width:160px"></${ConfigSelect}>
        <span class="cfg-hint">The model and reasoning effort <code>tclaude ask</code> uses for ad-hoc questions when you don't pass <code>-m</code>/<code>--effort</code> <em>and no Profile above is selected</em>. Defaults to <code>sonnet</code> at <code>medium</code> — a balanced, capable default for ad-hoc terminal answers. <em>Built-in default</em> leaves the field unpinned so it tracks that default. Stored in the <code>ask</code> block of config.json (the same file the CLI reads); a per-call flag always overrides it. Saved with <strong>Save changes</strong> below, like the rest of this page.</span>
      </div>
      <div class="cfg-field">
        <span class="cfg-label">Scribe — profile</span>
        <${ConfigSelect} id="scribe-profile" aria-label="Scribe default profile" style="min-width:160px"></${ConfigSelect}>
        <span class="cfg-hint">Optional. Pick a saved <strong>spawn profile</strong> (from the Groups tab) that a freshly <em>summoned</em> scribe — the chat agent behind the <strong>🤖 Edit with agent</strong> buttons — launches with: its harness / model / effort / sandbox. This is the harness-independent way to run scribes on <strong>Codex</strong> as well as Claude, or to pin a cheaper model for their light editing. Leave as <em>(default)</em> to use the harness default (Claude Code). Every click summons a fresh, independently named scribe, so the current setting applies without disturbing scribes already working. Stored in the <code>scribe</code> block of config.json; saved with <strong>Save changes</strong> below, like the rest of this page.</span>
      </div>
    </div>

    <div class="cfg-section">
      <h3>agentd daemon & server</h3>
      <div class="cfg-field">
        <span class="cfg-label">Auto-launch dashboard</span>
        <label class="cfg-inline"><${ConfigInput} type="checkbox" id="cfg-agent-autolaunch" /> open the dashboard when <code>agentd serve</code> starts</label>
      </div>
      <div class="cfg-field">
        <span class="cfg-label">Dashboard port</span>
        <${ConfigInput} type="number" id="cfg-agent-dashboardport" min="0" max="65535" placeholder="(random)" aria-label="Dashboard port" autocomplete="off" style="min-width:120px" />
        <span class="cfg-hint">Fixed port for the dashboard + approval popup. Empty / <code>0</code> = a random free port each start. A stable port gives a bookmarkable URL; takes effect on the next <code>agentd serve</code>.</span>
      </div>
      <div class="cfg-field">
        <span class="cfg-label">Dashboard bind</span>
        <${ConfigInput} type="text" id="cfg-agent-dashboardbind" list="cfg-agent-dashboardbind-list" placeholder="127.0.0.1" aria-label="Dashboard bind host" autocomplete="off" spellcheck="false" style="min-width:160px" />
        <datalist id="cfg-agent-dashboardbind-list">
          <option value="127.0.0.1">loopback only — default (safe)</option>
          <option value="0.0.0.0">all interfaces — behind your OWN auth</option>
          <option value="::">all interfaces (IPv6)</option>
        </datalist>
        <span class="cfg-hint">Host/interface the <strong>local</strong> dashboard binds to (host only — the port is above). Empty / <code>127.0.0.1</code> = loopback only. Set <code>0.0.0.0</code> / <code>::</code> to expose it on the network — <strong>only behind your own auth</strong> (reverse proxy / VPN / IAP), since its own gate is just a cookie + operator token. This is <em>not</em> the “Remote access” section below — that's a separate mTLS + passphrase listener. Takes effect on the next <code>agentd serve</code>.</span>
      </div>
      <div class="cfg-field">
        <span class="cfg-label">System tray</span>
        <label class="cfg-inline"><${ConfigInput} type="checkbox" id="cfg-agent-notray" /> hide the tray icon (same as <code>--no-tray</code>)</label>
        <span class="cfg-hint">Skips the system tray icon <code>agentd serve</code> shows by default. Takes effect next time <code>agentd serve</code> starts.</span>
      </div>
      <div class="cfg-field">
        <span class="cfg-label">Persist operator token</span>
        <label class="cfg-inline"><${ConfigInput} type="checkbox" id="cfg-agent-persisttoken" /> keep the same operator token across restarts in the private file (same as <code>--persist-operator-token</code>)</label>
        <label class="cfg-inline"><${ConfigInput} type="checkbox" id="cfg-agent-persisttoken-keychain" /> explicitly use the OS keychain instead (same as <code>--persist-operator-token-keychain</code>; implies persistence)</label>
        <span class="cfg-hint">Default off: a fresh token is minted every <code>agentd serve</code>. File persistence stores it at <code>0600 ~/.tclaude/data/operator_token</code>, which the human CLI reads automatically and agent sandboxes deny. Keychain storage is an explicit opt-in because keychain access is platform-dependent and may prompt; the CLI does not retrieve it automatically. Existing tokens are not copied between stores. The secret is never written to <code>config.json</code>. Takes effect on the next <code>agentd serve</code>.</span>
      </div>
      <div class="cfg-field">
        <span class="cfg-label">Broker limits</span>
        <label class="cfg-inline"><${ConfigInput} type="checkbox" id="cfg-broker-enforce" /> actually reject callers over the limit</label>
        <span class="cfg-hint">Ceilings on the brokered endpoints a sandboxed (<code>tclaude-layer</code>) agent uses to reach the database — <strong>20 requests/second per agent</strong> and <strong>10 MiB per request</strong>. Default off is <em>shadow mode</em>: excess is still measured and logged (saying what it <em>would</em> have refused) but nothing is refused, so you can see real traffic against the ceilings before enforcing. A denial-of-service backstop, not traffic shaping.</span>
      </div>
    </div>

    <div class="cfg-section">
      <h3>Logging</h3>
      <div class="cfg-field">
        <span class="cfg-label">Log level</span>
        <${ConfigSelect} id="cfg-log-level" aria-label="Log level">
          <option value="debug">debug</option>
          <option value="info">info</option>
          <option value="warn">warn</option>
          <option value="error">error</option>
        </${ConfigSelect}>
      </div>
      <div class="cfg-field">
        <span class="cfg-label">Record hooks</span>
        <label class="cfg-inline"><${ConfigInput} type="checkbox" id="cfg-record-hooks" /> log every hook callback payload</label>
        <span class="cfg-hint">Debugging aid — verbose; leave off for normal use.</span>
      </div>
      <div class="cfg-field">
        <span class="cfg-label">Max log size</span>
        <${ConfigInput} type="text" id="cfg-logrot-maxsize" placeholder="10MiB" aria-label="Log rotation max size" autocomplete="off" spellcheck="false" style="min-width:120px" />
        <span class="cfg-hint">Size cap for <code>~/.tclaude/output.log</code> before <code>agentd</code> rotates it (<code>10MiB</code>, <code>50m</code>, <code>500k</code>). Empty = built-in default (10 MiB). <code>0</code> disables rotation.</span>
      </div>
      <div class="cfg-field">
        <span class="cfg-label">Keep</span>
        <${ConfigInput} type="number" id="cfg-logrot-keep" min="0" placeholder="5" aria-label="Log rotation keep count" />
        <span>rotated files</span>
        <span class="cfg-hint">How many rotated logs (<code>output.log.1</code> … <code>.N</code>) <code>agentd</code> retains. Empty or <code>0</code> = built-in default (5).</span>
      </div>
    </div>

    <div class="cfg-section">
      <h3>Retention & cleanup</h3>
      <div class="cfg-field">
        <span class="cfg-label">Transcript retention</span>
        <label class="cfg-inline">keep transcripts for
          <${ConfigInput} type="number" id="cfg-claude-cleanup-days" min="1" placeholder="30" aria-label="Claude Code transcript retention (days)" style="width:11em" /> days</label>
        <span class="cfg-hint">Claude Code deletes a conversation transcript (and other stale session data / orphaned worktrees) once it's been <em>inactive</em> this many days — swept at Claude Code startup. Claude Code's own default is <strong>30 days</strong>, so transcripts you haven't touched in a month vanish. Raise this to keep them longer; set a large value like <code>99999</code> to effectively keep them forever (Claude Code rejects <code>0</code> and has no "never" option). Empty = leave Claude Code's default (or whatever you set by hand) untouched. Unlike the resume-threshold overrides under <em>Context & compaction</em>, this <strong>is</strong> written to <code>~/.claude/settings.json</code> (as <code>cleanupPeriodDays</code>) on every session start, so it also protects transcripts from your own plain <code>claude</code> runs. Stored as <code>claude_cleanup_period_days</code>.</span>
      </div>
      <p class="cfg-hint" style="padding-left:0">
        Optional long-horizon housekeeping that <strong>permanently deletes</strong> agents/conversations
        once they have been <em>retired</em> for a long time — reclaiming the Retired tab, mailbox queries
        and disk that retired entities (e.g. export summary-writer clones) otherwise hold forever.
        <strong>Off by default</strong>; retire stays the non-destructive half of cleanup. Deleting a
        conversation does <strong>not</strong> lose its cost — spend totals survive deletion.
      </p>
      <div class="cfg-field">
        <span class="cfg-label">Retired-agent auto-cleanup</span>
        <label class="cfg-inline"><${ConfigInput} type="checkbox" id="cfg-agent-retired-cleanup-enabled" /> delete agents retired longer than</label>
        <${ConfigInput} type="number" id="cfg-agent-retired-cleanup-days" min="1" max="36525" placeholder="365" aria-label="Days retired before deletion" style="min-width:90px" />
        <span>days</span>
        <span class="cfg-hint">agentd sweeps every 30${'\u00a0'}min (and at startup). Empty = built-in default (365 ≈ 1 year). Irreversible — choose a window long enough that anything still wanted has been reinstated.</span>
      </div>
    </div>

    <div class="cfg-section">
      <h3>Slop mode 🎰</h3>
      <div class="cfg-field">
        <span class="cfg-label">Vegas music in regular mode</span>
        <label class="cfg-inline"><${ConfigInput} type="checkbox" id="cfg-slop-vegas-regular" /> show the Vegas tab, volume controls & play music outside slop mode</label>
        <span class="cfg-hint">The dashboard's casino "slop" mode (the 🎰 in the header) bundles a <strong>Vegas</strong> tab with a lounge-radio player and a header volume mixer. Enable this to surface those — the tab, the 🔊 sound switch and 🎚️ volume sliders, and the music — on the <em>plain</em> dashboard too, without the slot machines, coins and sound FX. Music starts muted and plays on your first click (browsers block autoplay); the 🔊 button mutes it (remembered per browser). Off by default. Stored in the <code>slop</code> block of config.json alongside the volume/channel settings.</span>
      </div>
      <div class="cfg-field">
        <span class="cfg-label">Hide the pull lever</span>
        <label class="cfg-inline"><${ConfigInput} type="checkbox" id="cfg-slop-hide-lever" /> hide the side pull-lever in slop mode</label>
        <span class="cfg-hint">Slop ("casino") mode pins a chunky <strong>PULL</strong> lever to the right edge of the Groups tab — yank it to spin every machine at once. Enable this to hide that lever if you find it in the way; the rest of slop mode (slot machines, coins, sound FX) stays exactly as it was. Off by default (the lever shows). Stored in the <code>slop</code> block of config.json.</span>
      </div>
      <div class="cfg-field">
        <span class="cfg-label">Activity bots — slop mode 🎰</span>
        <${ConfigSelect} id="cfg-dashboard-activity-bots-slop" aria-label="Activity bot style in slop mode">
          <option value="emoji">Emoji bots</option>
          <option value="sprites">Pixel sprites</option>
          <option value="off">Off</option>
        </${ConfigSelect}>
        <span class="cfg-hint">The group-header activity bots' style when the dashboard's casino <strong>slop</strong> mode (the 🎰 in the header) is on — chosen independently of regular mode, the way slop already swaps the state pill for a slot machine. Default <em>Pixel sprites</em> (the full dancing robots), but you can keep the emoji bots, or turn them off. (Reduced-motion browsers get the bots <em>without</em> animation either way.) Stored as <code>dashboard.activity_bots.slop</code>.</span>
      </div>
    </div>

    <div class="cfg-section">
      <h3>Experimental features</h3>
      <p class="cfg-hint" style="padding-left:0">
        Opt-in flags for features under active development. Such features ship dark on
        <code>main</code> — off by default and invisible in normal use — so they can land in
        small increments without a long-lived feature branch. Expect rough edges while a
        flag is listed here; when a feature graduates, its flag disappears.
      </p>
      <div class="cfg-field">
        <span class="cfg-label">Processes</span>
        <label class="cfg-inline"><${ConfigInput} type="checkbox" id="cfg-feature-processes" /> enable the in-development <strong>Processes</strong> feature</label>
        <span class="cfg-hint">BPMN-lite repeatable process graphs with a drag-and-drop template editor. Runtime execution is temporarily unavailable while the engine is rebuilt. Stored as <code>features.processes</code>.</span>
      </div>
      <div class="cfg-field">
        <span class="cfg-label">Groups Route map</span>
        <label class="cfg-inline"><${ConfigInput} type="checkbox" id="cfg-feature-groups-route-map" /> enable the opt-in <strong>Members | Route map</strong> Groups view</label>
        <span class="cfg-hint">Read-only agent route leases and health for selected groups. Off by default; the route map and its snapshot projection stay absent until enabled. Takes effect on the next dashboard refresh. Stored as <code>features.groups_route_map</code>.</span>
      </div>
      <div class="cfg-field">
        <span class="cfg-label">Group attachments</span>
        <${ConfigSelect} id="cfg-feature-group-attachments" aria-label="Group attachment presentation">
          <option value="off">Off</option>
          <option value="float">Float above group title</option>
          <option value="fixed">Fixed group quick item</option>
        </${ConfigSelect}>
        <span class="cfg-hint">Off by default while this interaction is refined. <strong>Float</strong> shows the paperclip overlay only while the title is hovered. <strong>Fixed</strong> keeps the paperclip and link/ticket label visible as the last group quick item; only its edit pencil appears on hover. Turning the feature off only hides the controls—existing stored attachments remain intact. Takes effect on the next dashboard refresh. Stored as <code>features.group_attachments</code>.</span>
      </div>
      <div class="cfg-field">
        <span class="cfg-label">Command palette in terminals</span>
        <label class="cfg-inline"><${ConfigInput} type="checkbox" id="cfg-feature-terminal-command-palette-shortcut" /> let <strong>Ctrl/Cmd+K</strong> open the command palette while a web terminal has focus</label>
        <span class="cfg-hint">Off by default because Ctrl/Cmd+K clears the current input line in the supported harnesses. When enabled, the dashboard claims that chord inside web terminals instead. Takes effect on the next dashboard refresh. Stored as <code>features.terminal_command_palette_shortcut</code>.</span>
      </div>
      <div class="cfg-field">
        <span class="cfg-label">Recorded sandbox details</span>
        <label class="cfg-inline"><${ConfigInput} type="checkbox" id="cfg-feature-recorded-sandbox-details" /> show the details arrow beside each agent's sandbox lock or warning icon</label>
        <span class="cfg-hint">Off by default to keep the harness line compact. The lock or warning and its summary tooltip remain visible; enabling this adds the adjacent <strong>›</strong> action for the full recorded launch details. Takes effect on the next dashboard refresh. Stored as <code>features.recorded_sandbox_details</code>.</span>
      </div>
      <div class="cfg-field">
        <span class="cfg-label">Agent dirs: mount parent</span>
        <label class="cfg-inline"><${ConfigInput} type="checkbox" id="cfg-feature-agent-dirs-mount-parent" /> mount the shared parent root of <strong>agent-owned directories</strong> read-write</label>
        <span class="cfg-hint">On by default: the shared parent root is granted once, so the agent can create, rewrite, and delete its own env-var'd directories. Uncheck to opt out and restore per-directory grants — the agent can write inside each directory but cannot delete it. Takes effect on the next launch/resume. Stored as <code>features.agent_dirs_mount_parent</code>.</span>
      </div>
    </div>

    <div class="cfg-section">
      <h3>Remote access</h3>
      <p class="cfg-hint" style="padding-left:0">
        Expose this <strong>dashboard</strong> to your phone or another machine over the
        network (LAN / mesh VPN / tunnel), behind <strong>mTLS (a client certificate) +
        a passphrase</strong>. A <em>separate</em> HTTPS listener — the loopback dashboard
        you're using now is never weakened. See <a href="https://github.com/tofutools/tclaude/blob/main/docs/remote-access.md" target="_blank" rel="noopener">docs/remote-access.md</a>.
      </p>
      <div class="cfg-field">
        <span class="cfg-label">Prerequisite</span>
        <span class="cfg-hint" style="padding-left:0">
          First generate the certificates + passphrase on the host (one time) — this also
          writes the first device's <code>.p12</code> to install on your phone:
          <br /><code>tclaude remote-access setup --bind 0.0.0.0:8443</code><br />
          The toggle below only takes effect once that material exists; enabling without it
          is a no-op. Add more devices later with <code>tclaude remote-access add-client ${'<name>'}</code>.
        </span>
      </div>
      <div class="cfg-field">
        <span class="cfg-label">Remote dashboard</span>
        <label class="cfg-inline"><${ConfigInput} type="checkbox" id="cfg-remote-enabled" /> enabled</label>
        <span class="cfg-hint">Off by default. Starting / stopping the listener takes effect after an <strong>agentd restart</strong>.</span>
      </div>
      <div class="cfg-field">
        <span class="cfg-label">Listen interface</span>
        <${ConfigInput} type="text" id="cfg-remote-host" list="cfg-remote-host-list" placeholder="0.0.0.0" aria-label="Remote access listen interface" autocomplete="off" spellcheck="false" style="min-width:160px" />
        <datalist id="cfg-remote-host-list">
          <option value="0.0.0.0">all interfaces — LAN</option>
          <option value="127.0.0.1">loopback only — behind a tunnel</option>
        </datalist>
        <span class="cfg-hint"><code>0.0.0.0</code> = reachable on the LAN; a tailnet interface IP for a mesh VPN; <code>127.0.0.1</code> when a tunnel (Cloudflare / ngrok) terminates the cert. Empty = <code>0.0.0.0</code>.</span>
      </div>
      <div class="cfg-field">
        <span class="cfg-label">HTTPS port</span>
        <${ConfigInput} type="number" id="cfg-remote-port" min="1" max="65535" placeholder="8443" aria-label="Remote access HTTPS port" style="min-width:120px" />
        <span class="cfg-hint">The port the remote dashboard listens on. Dial <code>https://${'<host>'}:${'<port>'}</code> from the device.</span>
      </div>
      <div class="cfg-field">
        <span class="cfg-label">Status</span>
        <span class="cfg-remote-status" id="cfg-remote-status"></span>
      </div>
    </div>

    <datalist id="cfg-slug-list"></datalist>
    <datalist id="cfg-group-list"></datalist>
    <datalist id="cfg-state-list">
      <option value="*"></option><option value="idle"></option>
      <option value="working"></option><option value="awaiting_permission"></option>
      <option value="awaiting_input"></option><option value="error"></option>
      <option value="exited"></option>
    </datalist>

    <details class="cfg-advanced" id="cfg-advanced">
      <summary>Advanced — sudo configuration (raw JSON)</summary>
      <div class="cfg-advanced-body">
        <p class="cfg-hint">
          The <code>agent.sudo</code> block (time-bounded permission elevations, with optional
          per-conversation overrides) is edited as raw JSON — it is the one nested-map structure
          without a dedicated form. Leave blank for none. Validated on save.
        </p>
        <${ConfigTextarea} id="cfg-sudo-json" spellcheck="false" aria-label="Advanced sudo configuration JSON" placeholder="JSON object"></${ConfigTextarea}>
      </div>
    </details>

    <div class="cfg-footer">
      <span class="cfg-status" id="cfg-status">loading…</span>
      <span class="spacer"></span>
      <${ConfigButton} type="button" id="cfg-reload">Reload</${ConfigButton}>
      <${ConfigButton} type="button" id="cfg-save" class="primary">Save changes…</${ConfigButton}>
    </div>

    </div>
    </${ConfigEventContext.Provider}>

  `;
}
