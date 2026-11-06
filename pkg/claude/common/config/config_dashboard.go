package config

import (
	"regexp"
	"slices"
	"strings"
)

// Activity-bot style values — the per-mode choices in ActivityBotsConfig.
// "emoji" is the lightweight emoji/glyph+CSS bot row (fantasy glyphs in wizard
// mode); "sprites" is the pixel-art animation (robots in slop mode,
// spellcasters in wizard mode); "off" hides the indicator. An empty / unknown
// value falls back to the per-mode default (see ActivityBotsRegular /
// ActivityBotsSlop / ActivityBotsWizard).
const (
	ActivityBotsEmoji   = "emoji"
	ActivityBotsSprites = "sprites"
	ActivityBotsOff     = "off"
)

// Group quick-options display modes — config dashboard.group_quick_options.
// The "quick options" are the editable chips packed into each group's
// <summary> header (📝 description, 📁 default dir, 🧠 default profile, 🔗
// links). They grow the header wide, so the dashboard can auto-fold them:
//
//	"hover"    — icon-only at rest; the chip text slides open when the
//	             pointer is over the group header (a CSS horizontal
//	             accordion). The activity-bot row, group name and 👥 member
//	             chip always stay visible. This is the default.
//	"expanded" — always show the full chips (the pre-fold behaviour).
//
// An empty / unknown value falls back to the default (see GroupQuickOptions).
// Folding is a hover affordance, so it's gated to hover-capable pointers in
// CSS — touch devices always see the full chips. A per-group "pin" (a
// per-browser dashboard pref) opts a single group out of folding regardless
// of this mode.
const (
	GroupQuickOptionsHover    = "hover"
	GroupQuickOptionsExpanded = "expanded"
)

// Default-terminal modes — config dashboard.default_terminal. Chooses how the
// dashboard's per-agent "focus" / "open window" / "open terminal" actions open
// a console:
//
//	"native" — pop a native OS terminal window (the historical default),
//	           falling back to an in-browser PTY only when no native window
//	           can be opened.
//	"web"    — open the console as an in-browser terminal pane in the
//	           dashboard's own Terminals tab, without touching the OS windowing
//	           system — the same surface the dedicated "web term" / "web window"
//	           buttons already always use. This is the default.
//
// An empty / unknown value falls back to the default (see DefaultTerminal).
const (
	DefaultTerminalNative = "native"
	DefaultTerminalWeb    = "web"
)

// Default-directory-picker modes — config dashboard.default_directory_picker.
// Remote dashboard origins always override the local default to web mode.
const (
	DefaultDirectoryPickerNative = "native"
	DefaultDirectoryPickerWeb    = "web"
)

// DashboardConfig holds display toggles for the agentd web dashboard.
type DashboardConfig struct {
	// TerminalAttach tunes the browser terminal's resize handshake when it
	// attaches to a tmux client. Absent keeps the historical repair sequence:
	// send the fitted size, then nudge the terminal by one row and restore it
	// after the first screen arrives. See TerminalAttachConfig and
	// (*Config).ResolvedTerminalAttach.
	TerminalAttach *TerminalAttachConfig `json:"terminal_attach,omitempty"`
	// CloseTerminalTabs decides which dashboard terminal tabs close on their
	// own when their tmux side goes away, per event and per kind of terminal.
	// Absent keeps the defaults: shell terminals close when their session
	// ends, agent windows and every detach keep the tab. See
	// TerminalTabCloseConfig and (*Config).CloseTerminalTab.
	CloseTerminalTabs *TerminalTabCloseConfig `json:"close_terminal_tabs,omitempty"`
	// Keybindings rebinds dashboard keyboard shortcuts: shortcut id (from the
	// dashboard's registry, js/keybindings.js) to a chord such as
	// "Ctrl+Alt+KeyK". Absent ids keep their default. The dashboard owns the
	// meaning; the daemon only checks the shape. See (*Config).DashboardKeybindings.
	Keybindings map[string]string `json:"keybindings,omitempty"`
	// ActivityBots selects the style of the per-group + global "activity
	// bot" indicator — the deduped row of robot icons that rides in each
	// group <summary> header (visible even when the group is folded) and in
	// the top bar, summarising member status at a glance. The style is
	// chosen INDEPENDENTLY for the plain dashboard and for slop mode — see
	// ActivityBotsConfig. Absent → defaults (emoji in regular, sprites in
	// slop).
	ActivityBots *ActivityBotsConfig `json:"activity_bots,omitempty"`
	// AlwaysShowPluginsTab forces the dashboard's Plugins tab to stay
	// visible even when no plugins are installed. By default the tab
	// auto-hides when the installed set is empty (most users never define a
	// plugin, and the tab would only show the built-in catalog) — flip this
	// on to keep it around, e.g. to reach the catalog and install one. A
	// broken plugins.json still surfaces the tab regardless, so the error is
	// never hidden. Default false (auto-hide when empty). See
	// (*Config).ShowPluginsTabAlways.
	AlwaysShowPluginsTab bool `json:"always_show_plugins_tab,omitempty"`
	// HScrollFollow selects the dashboard's horizontal-scroll chrome-bar
	// behaviour for when the page is wide enough to need a sideways scrollbar
	// (JOH-313). The full-bleed bars (header / nav / slop marquee) always
	// widen to the content so they never look ragged; this knob is only about
	// their CONTENT:
	//   true  (follow, the default) — the bars' content is pinned to the
	//         viewport and sticky-left, so the header controls + tab strip
	//         stay put and usable while the page is scrolled sideways.
	//   false (static)              — the content scrolls off with the page;
	//         the bar background still fills the width, but the controls
	//         aren't reachable while scrolled right.
	// A *bool so absent (the default) is distinguishable from an explicit
	// false: nil → follow. The dashboard reads the resolved value off the
	// snapshot each poll and toggles body.hscroll-follow; it replaces the old
	// per-browser header toggle button. See (*Config).HScrollFollow.
	HScrollFollow *bool `json:"hscroll_follow,omitempty"`
	// AlwaysShowOpenPRs keeps the fixed footer's "Open PRs" indicator
	// mounted even when the active GitHub identity has no open pull requests
	// at all. Default true: the indicator is a permanent, glanceable entry
	// point to the popover (which also carries the recently closed/merged
	// list), so it should not appear and disappear as the last PR merges.
	// An explicit false restores the older behaviour of showing it only
	// while at least one authored PR is open. Either way the indicator stays
	// hidden until the daemon has actually resolved a GitHub identity — a
	// permanent "Open PRs 0" would be a lie when `gh` is missing or logged
	// out. A *bool so absent (the default) is distinguishable from an
	// explicit false. See (*Config).AlwaysShowOpenPRs.
	AlwaysShowOpenPRs *bool `json:"always_show_open_prs,omitempty"`
	// SidePanelEdgeReveal floats a folded right side panel (the Groups
	// palette, the Collab list) in over the content when the pointer reaches
	// the right edge, and takes it away when the pointer leaves it. Default
	// true; an explicit false keeps a folded panel folded until its handle is
	// clicked. A *bool so absent is distinguishable from an explicit false.
	// See (*Config).SidePanelEdgeReveal.
	SidePanelEdgeReveal *bool `json:"side_panel_edge_reveal,omitempty"`
	// SidePanelPredictivePeek peeks a folded side panel's leading edge in
	// when a pointer flung at the right edge is predicted to land there
	// (tcl-o4nims), and SidePanelGrowingPeek grows that peek towards the
	// panel's full width as the pointer closes in (tcl-wdwbm2). The peek
	// defaults on, its growth off (tcl-bg5qm1: opening is then a move onto
	// the peek); both need SidePanelEdgeReveal. *bool so absent differs from
	// false.
	SidePanelPredictivePeek *bool `json:"side_panel_predictive_peek,omitempty"`
	SidePanelGrowingPeek    *bool `json:"side_panel_growing_peek,omitempty"`
	// The edge reveal's and the peek's tuning, each absent for its default
	// and clamped into range (SidePanelTuningLimits): see
	// (*Config).SidePanelTuning.
	SidePanelEdgeZonePx             *int `json:"side_panel_edge_zone_px,omitempty"`
	SidePanelRetractMs              *int `json:"side_panel_retract_ms,omitempty"`
	SidePanelPredictMinSpeed        *int `json:"side_panel_predict_min_speed,omitempty"`
	SidePanelPredictSpeedDistancePx *int `json:"side_panel_predict_speed_distance_px,omitempty"`
	SidePanelPredictMinSpeedFloor   *int `json:"side_panel_predict_min_speed_floor,omitempty"`
	SidePanelPredictMinReadings     *int `json:"side_panel_predict_min_readings,omitempty"`
	SidePanelMinTravelPx            *int `json:"side_panel_min_travel_px,omitempty"`
	SidePanelBrakePercent           *int `json:"side_panel_brake_percent,omitempty"`
	SidePanelPeekMinPx              *int `json:"side_panel_peek_min_px,omitempty"`
	// RecentPRWindowDays bounds the footer popover's "Recently closed"
	// filter: authored pull requests merged or closed within this many days
	// back. Default 3 (absent / nil). 0 disables the filter entirely and the
	// daemon then stops searching for closed PRs at all; larger values are
	// clamped to RecentPRWindowDaysMax so a hand-edited config cannot ask
	// GitHub for an unbounded history every poll. A *int so an explicit 0
	// (off) is distinguishable from an absent key (default). See
	// (*Config).RecentPRWindowDays.
	RecentPRWindowDays *int `json:"recent_pr_window_days,omitempty"`
	// DictationSendPhrase is the send phrase's earlier home, still read when
	// Voice lists none. A *string so an explicit "" (off) differs from absent.
	DictationSendPhrase *string `json:"dictation_send_phrase,omitempty"`
	// Voice holds the spoken phrases and the opt-in hands-free settings. See
	// (*Config).Voice.
	Voice *VoiceConfig `json:"voice,omitempty"`
	// GroupQuickOptions selects how the editable "quick option" chips in each
	// group <summary> header (📝 description, 📁 default dir, 🧠 default
	// profile, 🔗 links) are displayed — one of GroupQuickOptions{Hover,
	// Expanded}. "hover" (the default) folds them to icon-only at rest and
	// slides the text open on header hover, reclaiming horizontal space;
	// "expanded" keeps the full chips always visible. Empty / unknown →
	// default (hover). The dashboard reads the resolved value off the snapshot
	// each poll and toggles body.group-quick-fold. See
	// (*Config).GroupQuickOptions.
	GroupQuickOptions string `json:"group_quick_options,omitempty"`
	// OverlayBackdrop picks what full-screen previews do to the page behind
	// them: "dim" (default), "blur", "both" or "none". Blur is slow in a
	// browser without GPU acceleration. See (*Config).OverlayBackdrop.
	OverlayBackdrop string `json:"overlay_backdrop,omitempty"`
	// WindowControls picks the look of an overlay window's title bar buttons:
	// "auto" (default) or "standard" for plain − ⇄ × symbols, or the opt-in
	// native looks: "os" (follows the browser's OS), "macos", "windows",
	// "linux", or "classic". See (*Config).WindowControls.
	WindowControls string `json:"window_controls,omitempty"`
	// NoSnapAssist turns off Snap Assist: after the operator snaps an overlay
	// window, the space left empty no longer offers the other windows. See
	// (*Config).SnapAssist.
	NoSnapAssist bool `json:"no_snap_assist,omitempty"`
	// DefaultTerminal selects how the dashboard's spawn auto-focus, per-agent
	// focus / open-window / open-terminal actions open a console — one of
	// DefaultTerminal{Native, Web}. "native" pops a native OS window (falling
	// back to an in-browser PTY only when it can't); "web" (the default) opens
	// an in-browser terminal pane in the dashboard's Terminals tab instead, the
	// same surface the dedicated "web term" / "web window" buttons use. Empty /
	// unknown → default (web). The dashboard reads the resolved value off the
	// snapshot and routes its focus/open actions accordingly.
	// See (*Config).DefaultTerminal.
	DefaultTerminal string `json:"default_terminal,omitempty"`
	// DefaultDirectoryPicker selects the directory chooser used from a local
	// dashboard: "native" opens the host OS dialog, while "web" (the default)
	// uses the dashboard's browser-rendered directory navigator. Remote
	// dashboard origins always use the web picker because a host-side native
	// dialog cannot be operated from the remote browser. Empty / unknown falls
	// back to web. See (*Config).DefaultDirectoryPicker.
	DefaultDirectoryPicker string `json:"default_directory_picker,omitempty"`
	// ShowAgentHideButton keeps the per-agent "hide window" button — the
	// slashed-eye icon beside "focus" in each agent row's quick-control
	// cluster — visible. That button detaches the agent's terminal window;
	// it's far less used than its "focus" (show-window) twin, so by default
	// the dashboard drops it to keep the row tight, leaving just "focus". Flip
	// this on to bring the hide button back. Default false (hidden). The
	// dashboard reads the resolved value off the snapshot each poll and
	// toggles body.show-agent-hide-btn. See (*Config).ShowAgentHideButton.
	ShowAgentHideButton bool `json:"show_agent_hide_button,omitempty"`
	// ShowGroupDescription keeps each group header's 📝 description chip — the
	// click-to-edit blurb beside the group name — visible. The feature is
	// deprecated: group descriptions are display-only, never surfaced anywhere
	// that drives behaviour, so the chip is hidden by default to keep the
	// header uncluttered. Flip this on to bring the chip back (and with it the
	// only way to view/edit a group's description). Default false (hidden). The
	// dashboard reads the resolved value off the snapshot each poll and toggles
	// body.show-group-description. See (*Config).ShowGroupDescription.
	ShowGroupDescription bool `json:"show_group_description,omitempty"`
	// ShowFinalPRChecks keeps the CI checks pill (✓ n/m) beside merged and
	// closed pull requests in the Groups member table. Once a PR is merged or
	// closed its CI result is history, so by default the dashboard only keeps
	// the pill when the final run is failing; flip this on to always show it.
	// Default false. The dashboard reads the resolved value off the snapshot
	// each poll and toggles body.show-final-pr-checks. See
	// (*Config).ShowFinalPRChecks.
	ShowFinalPRChecks bool `json:"show_final_pr_checks,omitempty"`
	// StayOnTerminalsAfterLastClose keeps the dashboard on the Terminals tab
	// when its last open terminal closes. By default the dashboard moves to
	// the Groups tab instead, since an empty Terminals tab has nothing to act
	// on. Flip this on to keep the old stay-put behavior. Default false. See
	// (*Config).StayOnTerminalsAfterLastClose.
	StayOnTerminalsAfterLastClose bool `json:"stay_on_terminals_after_last_close,omitempty"`
	// FusedToolbarFixedNode keeps the fused Groups view's toolbar (global
	// defaults, + new group) on the node picked in its node chip. By default
	// it follows the node of the group last acted on there. Default false.
	// See (*Config).FusedToolbarFixedNode.
	FusedToolbarFixedNode bool `json:"fused_toolbar_fixed_node,omitempty"`
	// UsageCollapseSubviews restores the Usage tab's old auto-collapse: with
	// only one of Quota/Costs having data, the tab shows that view alone and
	// hides the Overview/Quota/Costs switcher. By default all three views are
	// always offered. Default false. See (*Config).UsageCollapseSubviews.
	UsageCollapseSubviews bool `json:"usage_collapse_subviews,omitempty"`
}

// ActivityBotsConfig picks the activity-bot visual independently per mode,
// so the lightweight emoji bots can ride the plain dashboard while the full
// pixel-art robots come out for slop ("casino") mode — the defaults — or
// any other mix, or off entirely. Wizard mode defaults to its own fantasy
// glyphs and can opt into the wizard sprite sheets. Each field is one of
// ActivityBots{Emoji,Sprites,Off}; empty / unknown falls back to that
// mode's default. prefers-reduced-motion already drops just the animation
// (keeping the bots); these are the change-the-style / turn-it-off knobs.
type ActivityBotsConfig struct {
	Regular string `json:"regular,omitempty"` // plain dashboard; default emoji
	Slop    string `json:"slop,omitempty"`    // slop mode; default sprites (robots)
	Wizard  string `json:"wizard,omitempty"`  // wizard mode; default emoji (glyphs); "sprites" = wizards
}

// RemoteAccessConfig configures the optional, separately-bound HTTPS listener
// that exposes the agentd dashboard to the network (LAN / mesh / tunnel). It
// is OFF by default and entirely independent of the loopback dashboard, which
// keeps its init-token → session-cookie flow unchanged.
//
// When enabled, agentd starts a SECOND listener on Bind that enforces, before
// any dashboard/API request is served:
//   - mTLS — a client certificate issued by the tclaude remote-access CA
//     (RequireAndVerifyClientCert at the TLS layer; no valid cert ⇒ the
//     connection is refused before any handler runs), AND
//   - a passphrase login (`/login`) that mints a signed, restart-surviving
//     session cookie.
//
// This is a network-exposed agent control plane (it can spawn/kill agents and
// is a send-keys injection sink), so the auth is deliberately built to the
// public-internet bar; LAN is just the zero-infra preset of that hardened
// build. All secret material — the CA/server/client certs, the passphrase
// hash, and the cookie-signing key — lives as 0600 files under
// RemoteAccessDir (~/.tclaude/remote-access/), never in this config file.
// Generate it with `tclaude remote-access setup`.
type RemoteAccessConfig struct {
	// Enabled starts the remote HTTPS listener. Default false: tclaude never
	// exposes the control plane to the network without an explicit opt-in.
	Enabled bool `json:"enabled,omitempty"`

	// Bind is the listen address for the remote listener — e.g.
	// "0.0.0.0:8443" (LAN), a tailnet interface IP (mesh), or
	// "127.0.0.1:8443" (behind a tunnel that terminates a real cert). Empty
	// leaves the listener off even when Enabled is true (there is nothing to
	// bind to), and Validate flags that combination.
	Bind string `json:"bind,omitempty"`
}

// RemoteAccessEnabled reports whether the remote listener should start: the
// block is present, Enabled, and has a non-empty Bind. Nil-safe so callers
// need no guard.
func (c *Config) RemoteAccessEnabled() bool {
	return c != nil && c.RemoteAccess != nil && c.RemoteAccess.Enabled && c.RemoteAccess.Bind != ""
}

// RemoteAccessBind returns the configured remote listen address, or "" when no
// remote-access block is set. Nil-safe.
func (c *Config) RemoteAccessBind() string {
	if c == nil || c.RemoteAccess == nil {
		return ""
	}
	return c.RemoteAccess.Bind
}

// SlopConfig holds the slop-mode audio knobs. Both volumes are percent
// (0–100) of the mode's built-in full level: MusicVolume scales the
// Vegas lounge radio, EffectsVolume scales the synthesized casino FX.
// Pointers so "absent" (music defaults to 50, effects to 100) is
// distinguishable from an explicit 0 (silent but not muted — the master
// 🔇/🔊 switch is a separate localStorage-persisted preference in the
// browser).
//
// Channel is the SomaFM channel id the Vegas radio tunes to (one of
// SlopChannels; absent → DefaultSlopChannel). A pointer + omitempty so an
// untouched config stays clean and an absent value is the default rather
// than the empty string.
//
// VegasInRegularMode, when true, surfaces the Vegas music features — the
// Vegas tab, the header volume mixer (🎚️) and master sound switch (🔊),
// and the lounge radio — on the PLAIN dashboard, not just in slop
// ("casino") mode. It decouples the soundtrack from the full cosmetic
// re-skin: you get music + volume + the tab WITHOUT the slot machines,
// header shimmer, coins and sound FX. A *bool so absent = off (the
// features stay slop-only) and an explicit value round-trips through the
// Config tab.
//
// HidePullLever, when true, hides the slop-mode side pull-lever — the
// casino lever pinned to the right edge of the Groups tab that spins every
// machine at once. Slop mode otherwise stays fully intact; this just drops
// that one ornament for people who find it in the way. A *bool so absent =
// off (the lever shows, the historical default) and an explicit value
// round-trips through the Config tab.
//
// Written by the dashboard's volume sliders via POST /api/slop/volumes and
// the channel picker via POST /api/slop/channel; also round-trips through
// the Config tab like any other field.
type SlopConfig struct {
	MusicVolume        *int    `json:"music_volume,omitempty"`
	EffectsVolume      *int    `json:"effects_volume,omitempty"`
	Channel            *string `json:"channel,omitempty"`
	VegasInRegularMode *bool   `json:"vegas_in_regular_mode,omitempty"`
	HidePullLever      *bool   `json:"hide_pull_lever,omitempty"`
}

// SlopChannels is the allowlist of SomaFM channel ids the dashboard radio
// can tune to. It is the SINGLE SOURCE OF TRUTH shared by config validation
// (here), the now-playing proxy's SSRF gate (agentd), and the browser's
// channel picker (js/vegas.js carries a matching catalog with human labels,
// pinned to this set by TestSlopNowPlaying_ChannelMatchesVegasJS).
//
// The radio is theme-agnostic: this one flat allowlist backs both the 🎰
// slop/Vegas soundtrack and the 🧙 wizard soundtrack. The browser groups
// these ids into "Vegas Lounge" vs "Wizard's Realm" for its two-level
// picker (js/vegas.js's CHANNELS carry the group), but the server only
// cares that a requested id is allowlisted — the group is a pure UI filter.
//
// Adding a channel is a one-line entry here plus a matching {id,label,group}
// in vegas.js. Every other URL (stream, station home, songs feed) derives
// from the id by SomaFM's fixed URL shape, so the id is all that's shared.
var SlopChannels = []string{
	// Vegas Lounge group — the original slop-mode soundtrack.
	"illstreet",   // Illinois Street Lounge — vintage cocktail / Rat-Pack
	"secretagent", // Secret Agent — spy-jazz & surf
	"groovesalad", // Groove Salad — ambient / downtempo
	"lush",        // Lush — mostly vocal, mostly chilled
	"bootliquor",  // Boot Liquor — americana roots
	"u80s",        // Underground 80s — early alternative / new wave
	"defcon",      // DEF CON Radio — music for hacking
	// Wizard's Realm group — fantasy-flavored SomaFM channels for 🧙 mode.
	"thistle",      // ThistleRadio — Celtic roots ("The Tavern", wizard default)
	"folkfwd",      // Folk Forward — indie / alt-folk ("The Bard's Rest")
	"dronezone",    // Drone Zone — atmospheric ambient ("The Astral Plane")
	"darkzone",     // The Dark Zone — dark ambient ("The Dungeon")
	"doomed",       // Doomed — dark industrial ambient ("The Crypt")
	"deepspaceone", // Deep Space One — deep-space ambient ("The Cosmos")
}

// DefaultWizardChannel is the station the wizard soundtrack tunes to for a
// fresh listener (one who has never explicitly picked a channel). It is the
// Celtic "Tavern" — the closest thing SomaFM has to a fantasy tavern. The
// server treats it like any other allowlisted id; only the browser knows it
// is the wizard group's default (see js/vegas.js). Kept here beside the
// allowlist so the two never drift.
const DefaultWizardChannel = "thistle"

// DefaultSlopChannel is the channel the Vegas radio plays when none is
// configured — the original vintage lounge, so a fresh config keeps the
// historical soundtrack.
const DefaultSlopChannel = "illstreet"

// IsKnownSlopChannel reports whether id is in the SlopChannels allowlist.
func IsKnownSlopChannel(id string) bool {
	return slices.Contains(SlopChannels, id)
}

// HasExplicitSlopChannel reports whether the config carries a real, known
// channel choice — as opposed to falling back to DefaultSlopChannel because
// nothing was set. The dashboard radio uses this to tell a fresh listener
// (who should hear the active theme's default station — the Tavern in wizard
// mode) apart from someone who deliberately picked a station. Nil-safe on the
// receiver.
func (c *Config) HasExplicitSlopChannel() bool {
	if c == nil || c.Slop == nil || c.Slop.Channel == nil {
		return false
	}
	return IsKnownSlopChannel(strings.TrimSpace(*c.Slop.Channel))
}

// ResolvedSlopChannel returns the effective channel id: the configured one
// when it's a known channel, else DefaultSlopChannel. A hand-edited unknown
// id degrades to the default here (Validate reports it to the Config tab),
// so readers always get a streamable channel. Nil-safe on the receiver.
func (c *Config) ResolvedSlopChannel() string {
	if c == nil || c.Slop == nil || c.Slop.Channel == nil {
		return DefaultSlopChannel
	}
	id := strings.TrimSpace(*c.Slop.Channel)
	if IsKnownSlopChannel(id) {
		return id
	}
	return DefaultSlopChannel
}

// DefaultMusicVolume is the effective music volume (the Vegas/wizard lounge
// radio) for an absent slop.music_volume key: 50% — full-volume slop and
// wizard mode startled users on first entry, so the soundtrack defaults to
// half. `tclaude setup` also writes this value explicitly so it's visible in
// the config / Config tab (see setup.installDefaultMusicVolume); readers fall
// back to it here for any config that predates that write or was hand-cleared.
const DefaultMusicVolume = 50

// defaultEffectsVolume is the effective volume for an absent
// slop.effects_volume key: 100%. The synthesized casino FX only fire on
// interaction (not a continuous stream like the radio), so full volume there
// isn't the part that startled anyone — only the music default was lowered.
const defaultEffectsVolume = 100

// ResolvedSlopVolumes returns the effective (music, effects) volumes in
// percent: an absent music volume defaults to DefaultMusicVolume (50) and an
// absent effects volume to 100. A hand-edited out-of-range value is clamped
// to 0–100 — Validate reports it to the Config tab, but readers must still
// get a usable volume rather than handing 500% to the browser. Nil-safe on
// the receiver so callers need no guard.
func (c *Config) ResolvedSlopVolumes() (music, effects int) {
	music, effects = DefaultMusicVolume, defaultEffectsVolume
	if c == nil || c.Slop == nil {
		return music, effects
	}
	if c.Slop.MusicVolume != nil {
		music = min(100, max(0, *c.Slop.MusicVolume))
	}
	if c.Slop.EffectsVolume != nil {
		effects = min(100, max(0, *c.Slop.EffectsVolume))
	}
	return music, effects
}

// ShowVegasInRegularMode reports whether the Vegas music features (the
// Vegas tab, the header volume mixer + sound switch, and the lounge
// radio) should appear on the plain dashboard — config
// slop.vegas_in_regular_mode. Off by default (absent / nil / explicit
// false); only an explicit true opts in. Nil-safe on the receiver so
// callers need no guard.
func (c *Config) ShowVegasInRegularMode() bool {
	if c == nil || c.Slop == nil || c.Slop.VegasInRegularMode == nil {
		return false
	}
	return *c.Slop.VegasInRegularMode
}

// HidePullLever reports whether the slop-mode side pull-lever (the casino
// lever pinned to the right edge of the Groups tab) should be hidden —
// config slop.hide_pull_lever. Off by default (absent / nil / explicit
// false), so the lever shows as it historically did; only an explicit true
// hides it. Nil-safe on the receiver so callers need no guard.
func (c *Config) HidePullLever() bool {
	if c == nil || c.Slop == nil || c.Slop.HidePullLever == nil {
		return false
	}
	return *c.Slop.HidePullLever
}

// normalizeActivityBotsStyle returns s when it's a known style, else ""
// (so resolvers fall back to their per-mode default for a blank or a
// hand-edited garbage value).
func normalizeActivityBotsStyle(s string) string {
	switch s {
	case ActivityBotsEmoji, ActivityBotsSprites, ActivityBotsOff:
		return s
	default:
		return ""
	}
}

// ActivityBotsRegular reports the activity-bot style for the plain
// (non-slop) dashboard — config dashboard.activity_bots.regular. Default
// "emoji" (absent block/key or an unknown value). Nil-safe on the receiver.
func (c *Config) ActivityBotsRegular() string {
	if c != nil && c.Dashboard != nil && c.Dashboard.ActivityBots != nil {
		if s := normalizeActivityBotsStyle(c.Dashboard.ActivityBots.Regular); s != "" {
			return s
		}
	}
	return ActivityBotsEmoji
}

// ActivityBotsSlop reports the activity-bot style for slop ("casino") mode
// — config dashboard.activity_bots.slop. Default "sprites" (absent block/key
// or an unknown value). Nil-safe on the receiver.
func (c *Config) ActivityBotsSlop() string {
	if c != nil && c.Dashboard != nil && c.Dashboard.ActivityBots != nil {
		if s := normalizeActivityBotsStyle(c.Dashboard.ActivityBots.Slop); s != "" {
			return s
		}
	}
	return ActivityBotsSprites
}

// ActivityBotsWizard reports the activity-bot style for wizard mode — config
// dashboard.activity_bots.wizard. Default "emoji", which the wizard wrapper
// renders as its fantasy-glyph row; "sprites" opts into the WIZARD spellcaster
// sheets instead, and "off" hides it. Absent block/key or an unknown value
// falls back to the default. Nil-safe on the receiver.
func (c *Config) ActivityBotsWizard() string {
	if c != nil && c.Dashboard != nil && c.Dashboard.ActivityBots != nil {
		if s := normalizeActivityBotsStyle(c.Dashboard.ActivityBots.Wizard); s != "" {
			return s
		}
	}
	return ActivityBotsEmoji
}

// ShowPluginsTabAlways reports whether the dashboard should keep the Plugins
// tab visible even with no plugins installed — config
// dashboard.always_show_plugins_tab. Default false (the tab auto-hides when
// the installed set is empty). Nil-safe on the receiver.
func (c *Config) ShowPluginsTabAlways() bool {
	return c != nil && c.Dashboard != nil && c.Dashboard.AlwaysShowPluginsTab
}

// ShowAgentHideButton reports whether each agent row's "hide window" button
// (the slashed-eye beside "focus") should be shown — config
// dashboard.show_agent_hide_button. Default false: the button is hidden to
// keep the quick-control cluster tight ("focus" stays); only an explicit
// true brings it back. Nil-safe on the receiver.
func (c *Config) ShowAgentHideButton() bool {
	return c != nil && c.Dashboard != nil && c.Dashboard.ShowAgentHideButton
}

// ShowGroupDescription reports whether each group header's 📝 description chip
// should be shown — config dashboard.show_group_description. Default false: the
// group-description feature is deprecated (display-only, drives nothing), so
// the chip is hidden to keep headers tight; only an explicit true brings it
// back. Nil-safe on the receiver.
func (c *Config) ShowGroupDescription() bool {
	return c != nil && c.Dashboard != nil && c.Dashboard.ShowGroupDescription
}

// ShowFinalPRChecks reports whether merged/closed PRs keep their CI checks
// pill even when the final run is not failing — config
// dashboard.show_final_pr_checks. Default false. Nil-safe on the receiver.
func (c *Config) ShowFinalPRChecks() bool {
	return c != nil && c.Dashboard != nil && c.Dashboard.ShowFinalPRChecks
}

// keybindingIDPattern and keybindingChordPattern bound dashboard.keybindings
// to the registry's id shape and to modifier+KeyboardEvent.code chords.
var (
	keybindingIDPattern    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,47}$`)
	keybindingChordPattern = regexp.MustCompile(`^((Ctrl|Meta|Alt|Shift|Mod)\+){0,5}[A-Za-z0-9-]{1,32}$`)
)

// maxKeybindings caps dashboard.keybindings well above the registry's size.
const maxKeybindings = 256

func validKeybinding(id, chord string) bool {
	return keybindingIDPattern.MatchString(id) && keybindingChordPattern.MatchString(chord)
}

// DashboardKeybindings returns the operator's shortcut overrides from
// dashboard.keybindings, dropping any entry whose id or chord is malformed.
// Nil when there are none. Nil-safe on the receiver.
func (c *Config) DashboardKeybindings() map[string]string {
	if c == nil || c.Dashboard == nil || len(c.Dashboard.Keybindings) == 0 {
		return nil
	}
	out := make(map[string]string, len(c.Dashboard.Keybindings))
	for id, chord := range c.Dashboard.Keybindings {
		if validKeybinding(id, chord) {
			out[id] = chord
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// StayOnTerminalsAfterLastClose reports whether the dashboard should stay on
// the Terminals tab when its last open terminal closes — config
// dashboard.stay_on_terminals_after_last_close. Default false: the dashboard
// moves to the Groups tab. Nil-safe on the receiver.
func (c *Config) StayOnTerminalsAfterLastClose() bool {
	return c != nil && c.Dashboard != nil && c.Dashboard.StayOnTerminalsAfterLastClose
}

// FusedToolbarFixedNode reports whether the fused Groups view's toolbar stays
// on the node picked in its node chip — config
// dashboard.fused_toolbar_fixed_node. Default false: it follows the node of
// the group last acted on. Nil-safe on the receiver.
func (c *Config) FusedToolbarFixedNode() bool {
	return c != nil && c.Dashboard != nil && c.Dashboard.FusedToolbarFixedNode
}

// UsageCollapseSubviews reports whether the Usage tab collapses to its only
// populated view (Quota or Costs) and drops the view switcher — config
// dashboard.usage_collapse_subviews. Default false: Overview, Quota and Costs
// are always offered. Nil-safe on the receiver.
func (c *Config) UsageCollapseSubviews() bool {
	return c != nil && c.Dashboard != nil && c.Dashboard.UsageCollapseSubviews
}

// HScrollFollow reports whether the dashboard's full-bleed chrome bars
// should keep their content pinned to the viewport (follow mode) while the
// page is scrolled sideways — config dashboard.hscroll_follow. Default true
// (absent block / nil pointer); only an explicit "hscroll_follow": false
// selects static mode. Nil-safe on the receiver so callers need no guard.
func (c *Config) HScrollFollow() bool {
	if c == nil || c.Dashboard == nil || c.Dashboard.HScrollFollow == nil {
		return true
	}
	return *c.Dashboard.HScrollFollow
}

// RecentPRWindowDaysDefault / RecentPRWindowDaysMax bound the footer
// popover's "recently closed" lookback — config
// dashboard.recent_pr_window_days.
const (
	RecentPRWindowDaysDefault = 3
	RecentPRWindowDaysMax     = 30
)

// VoiceConfig is dashboard.voice. A new spoken command is one more
// VoicePhraseDefaults entry, and its phrase lands in the same list.
type VoiceConfig struct {
	// Phrases lists spoken phrases by action. An action left out keeps its
	// default, and an empty phrase or Enabled false turns that action off.
	Phrases []VoicePhrase `json:"phrases,omitempty"`
	// WakeEnabled opts in to the always-on listener. It has nothing to listen
	// for unless the wake phrase is on too; neither setting changes the other.
	WakeEnabled bool `json:"wake_enabled,omitempty"`
	// SilenceSendSeconds opts in to sending a dictated message after this many
	// seconds with no new speech. 0 is off.
	SilenceSendSeconds int `json:"silence_send_seconds,omitempty"`
	// MarkDictated prefixes dictated messages to agents with a short marker,
	// so the agent knows why punctuation may be missing. Default on.
	MarkDictated *bool `json:"mark_dictated,omitempty"`
	// CommaPauseSeconds and PeriodPauseSeconds opt in to punctuation from
	// pauses in dictation: a pause this long adds a comma, or a period that
	// capitalizes what follows. 0 is off.
	CommaPauseSeconds  float64 `json:"comma_pause_seconds,omitempty"`
	PeriodPauseSeconds float64 `json:"period_pause_seconds,omitempty"`
	// ContinuousEnabled opts in to continuous mode: after a voice message
	// sends, the next spoken sentence sends another with no wake phrase.
	ContinuousEnabled bool `json:"continuous_enabled,omitempty"`
	// ContinuousIdleSeconds ends continuous mode after this much silence.
	ContinuousIdleSeconds int `json:"continuous_idle_seconds,omitempty"`
}

// VoicePhrase is one spoken phrase and what it does.
type VoicePhrase struct {
	Action string `json:"action"`
	Phrase string `json:"phrase"`
	// Enabled is whether the phrase is active. Nil keeps the action's default.
	// It is stored apart from Phrase so turning a phrase off keeps its text.
	Enabled *bool `json:"enabled,omitempty"`
}

// Voice phrase actions.
const (
	VoiceActionSend = "send"
	VoiceActionWake = "wake"
	VoiceActionStop = "stop"
	// VoiceActionSteward wakes the Steward, whatever has focus.
	VoiceActionSteward = "steward"
)

// VoicePhraseDefaults are the known actions in display order, with neutral
// defaults. The wake phrase avoids words people say to their agents.
var VoicePhraseDefaults = []VoicePhrase{
	{Action: VoiceActionSend, Phrase: "send now", Enabled: new(true)},
	{Action: VoiceActionWake, Phrase: "hey agent", Enabled: new(false)},
	{Action: VoiceActionSteward, Phrase: "hey stewie", Enabled: new(false)},
	{Action: VoiceActionStop, Phrase: "stop listening", Enabled: new(true)},
}

// DictationSendPhraseDefault, the phrase cap and the silence-send range bound
// config dashboard.voice.
const (
	DictationSendPhraseDefault   = "send now"
	DictationSendPhraseMaxRunes  = 60
	SilenceSendSecondsMin        = 2
	SilenceSendSecondsMax        = 30
	VoicePauseSecondsMin         = 0.3
	VoicePauseSecondsMax         = 10.0
	ContinuousIdleSecondsDefault = 120
	ContinuousIdleSecondsMin     = 30
	ContinuousIdleSecondsMax     = 3600
)

func cleanVoicePhrase(phrase string) string {
	runes := []rune(strings.Join(strings.Fields(phrase), " "))
	return strings.TrimSpace(string(runes[:min(len(runes), DictationSendPhraseMaxRunes)]))
}

// Voice resolves dashboard.voice for its readers: every known action with its
// cleaned phrase, "" when it is off, the wake listener only when it has an
// active wake phrase of its own, and the silence send clamped into range. A
// wake phrase equal to an active send phrase would send and wake on the same
// words, so it counts as off, as does a Steward phrase equal to either and a
// stop phrase equal to any of them. Nil-safe on the receiver.
func (c *Config) Voice() VoiceConfig {
	var stored VoiceConfig
	if c != nil && c.Dashboard != nil && c.Dashboard.Voice != nil {
		stored = *c.Dashboard.Voice
	}
	out := VoiceConfig{
		WakeEnabled:           stored.WakeEnabled,
		MarkDictated:          new(stored.MarkDictated == nil || *stored.MarkDictated),
		ContinuousEnabled:     stored.ContinuousEnabled,
		ContinuousIdleSeconds: ContinuousIdleSecondsDefault,
	}
	if stored.ContinuousIdleSeconds > 0 {
		out.ContinuousIdleSeconds = max(ContinuousIdleSecondsMin, min(stored.ContinuousIdleSeconds, ContinuousIdleSecondsMax))
	}
	if stored.SilenceSendSeconds > 0 {
		out.SilenceSendSeconds = max(SilenceSendSecondsMin, min(stored.SilenceSendSeconds, SilenceSendSecondsMax))
	}
	if stored.CommaPauseSeconds > 0 {
		out.CommaPauseSeconds = max(VoicePauseSecondsMin, min(stored.CommaPauseSeconds, VoicePauseSecondsMax))
	}
	if stored.PeriodPauseSeconds > 0 {
		out.PeriodPauseSeconds = max(VoicePauseSecondsMin, min(stored.PeriodPauseSeconds, VoicePauseSecondsMax))
	}
	phrases := map[string]string{}
	enabled := map[string]bool{}
	for _, known := range VoicePhraseDefaults {
		phrases[known.Action] = known.Phrase
		enabled[known.Action] = *known.Enabled
	}
	phrases[VoiceActionSend] = c.DictationSendPhrase()
	for _, phrase := range stored.Phrases {
		if _, known := phrases[phrase.Action]; known {
			phrases[phrase.Action] = cleanVoicePhrase(phrase.Phrase)
			if phrase.Enabled != nil {
				enabled[phrase.Action] = *phrase.Enabled
			}
		}
	}
	for action, on := range enabled {
		if !on {
			phrases[action] = ""
		}
	}
	if strings.EqualFold(phrases[VoiceActionWake], phrases[VoiceActionSend]) {
		phrases[VoiceActionWake] = ""
	}
	if steward := phrases[VoiceActionSteward]; strings.EqualFold(steward, phrases[VoiceActionSend]) || strings.EqualFold(steward, phrases[VoiceActionWake]) {
		phrases[VoiceActionSteward] = ""
	}
	stop := phrases[VoiceActionStop]
	if strings.EqualFold(stop, phrases[VoiceActionSend]) || strings.EqualFold(stop, phrases[VoiceActionWake]) || strings.EqualFold(stop, phrases[VoiceActionSteward]) {
		phrases[VoiceActionStop] = ""
	}
	for _, known := range VoicePhraseDefaults {
		out.Phrases = append(out.Phrases, VoicePhrase{Action: known.Action, Phrase: phrases[known.Action]})
	}
	if phrases[VoiceActionWake] == "" && phrases[VoiceActionSteward] == "" {
		out.WakeEnabled = false
	}
	return out
}

// DictationSendPhrase reports the spoken send phrase, with its whitespace
// collapsed. Default DictationSendPhraseDefault (absent block / nil pointer),
// "" means spoken send is off, and anything past the rune cap is cut so a
// hand-edited config stays displayable. Nil-safe on the receiver.
func (c *Config) DictationSendPhrase() string {
	if c == nil || c.Dashboard == nil || c.Dashboard.DictationSendPhrase == nil {
		return DictationSendPhraseDefault
	}
	return cleanVoicePhrase(*c.Dashboard.DictationSendPhrase)
}

// AlwaysShowOpenPRs reports whether the dashboard footer's Open PRs
// indicator stays mounted with zero open pull requests — config
// dashboard.always_show_open_prs. Default true (absent block / nil pointer);
// only an explicit "always_show_open_prs": false restores the show-only-when-
// non-empty behaviour. Nil-safe on the receiver so callers need no guard.
func (c *Config) AlwaysShowOpenPRs() bool {
	if c == nil || c.Dashboard == nil || c.Dashboard.AlwaysShowOpenPRs == nil {
		return true
	}
	return *c.Dashboard.AlwaysShowOpenPRs
}

// SidePanelEdgeReveal reports whether a folded right side panel floats in
// when the pointer reaches the right edge — config
// dashboard.side_panel_edge_reveal. Default true (absent block / nil
// pointer); only an explicit false turns it off. Nil-safe on the receiver.
func (c *Config) SidePanelEdgeReveal() bool {
	if c == nil || c.Dashboard == nil || c.Dashboard.SidePanelEdgeReveal == nil {
		return true
	}
	return *c.Dashboard.SidePanelEdgeReveal
}

// SidePanelTuning is the resolved edge reveal and predictive peek tuning the
// dashboard runs with (side-panel-reveal.js setSidePanelTuning): every value
// defaulted and clamped. Speeds are px/s, the braking threshold a percent.
type SidePanelTuning struct {
	PredictivePeek         bool `json:"predictive_peek"`
	GrowingPeek            bool `json:"growing_peek"`
	EdgeZonePx             int  `json:"edge_zone_px"`
	RetractMs              int  `json:"retract_ms"`
	PredictMinSpeed        int  `json:"predict_min_speed"`
	PredictSpeedDistancePx int  `json:"predict_speed_distance_px"`
	PredictMinSpeedFloor   int  `json:"predict_min_speed_floor"`
	PredictMinReadings     int  `json:"predict_min_readings"`
	MinTravelPx            int  `json:"min_travel_px"`
	BrakePercent           int  `json:"brake_percent"`
	PeekMinPx              int  `json:"peek_min_px"`
}

// SidePanelTuningLimit is one tuning value's default and range.
type SidePanelTuningLimit struct{ Default, Min, Max int }

// SidePanelTuningLimits holds each tuning value's default (the dashboard's
// own, side-panel-reveal.js / side-panel-predict.js) and the range a value is
// clamped to, keyed by its config key without the dashboard.side_panel_
// prefix. The Config tab's fields carry the same ranges.
var SidePanelTuningLimits = map[string]SidePanelTuningLimit{
	"edge_zone_px":              {32, 4, 200},
	"retract_ms":                {500, 50, 5000},
	"predict_min_speed":         {1200, 100, 10000},
	"predict_speed_distance_px": {600, 1, 4000},
	"predict_min_speed_floor":   {600, 100, 10000},
	"predict_min_readings":      {4, 2, 20},
	"min_travel_px":             {80, 0, 1000},
	"brake_percent":             {70, 0, 100},
	"peek_min_px":               {48, 8, 400},
}

func sidePanelTuned(key string, v *int) int {
	l := SidePanelTuningLimits[key]
	if v == nil {
		return l.Default
	}
	return min(l.Max, max(l.Min, *v))
}

// SidePanelTuning resolves config dashboard.side_panel_* into the tuning the
// dashboard runs with: absent values take their defaults, out-of-range ones
// are clamped (SidePanelTuningLimits), and the speed floor never exceeds the
// full-distance minimum speed. Nil-safe on the receiver.
func (c *Config) SidePanelTuning() SidePanelTuning {
	d := &DashboardConfig{}
	if c != nil && c.Dashboard != nil {
		d = c.Dashboard
	}
	t := SidePanelTuning{
		PredictivePeek:         d.SidePanelPredictivePeek == nil || *d.SidePanelPredictivePeek,
		GrowingPeek:            d.SidePanelGrowingPeek != nil && *d.SidePanelGrowingPeek,
		EdgeZonePx:             sidePanelTuned("edge_zone_px", d.SidePanelEdgeZonePx),
		RetractMs:              sidePanelTuned("retract_ms", d.SidePanelRetractMs),
		PredictMinSpeed:        sidePanelTuned("predict_min_speed", d.SidePanelPredictMinSpeed),
		PredictSpeedDistancePx: sidePanelTuned("predict_speed_distance_px", d.SidePanelPredictSpeedDistancePx),
		PredictMinSpeedFloor:   sidePanelTuned("predict_min_speed_floor", d.SidePanelPredictMinSpeedFloor),
		PredictMinReadings:     sidePanelTuned("predict_min_readings", d.SidePanelPredictMinReadings),
		MinTravelPx:            sidePanelTuned("min_travel_px", d.SidePanelMinTravelPx),
		BrakePercent:           sidePanelTuned("brake_percent", d.SidePanelBrakePercent),
		PeekMinPx:              sidePanelTuned("peek_min_px", d.SidePanelPeekMinPx),
	}
	t.PredictMinSpeedFloor = min(t.PredictMinSpeedFloor, t.PredictMinSpeed)
	return t
}

// RecentPRWindowDays reports the lookback, in days, for the footer popover's
// "recently closed" pull-request filter — config
// dashboard.recent_pr_window_days. Default RecentPRWindowDaysDefault (absent
// block / nil pointer), 0 means the filter is off, and anything above
// RecentPRWindowDaysMax (or negative) is clamped into range. Nil-safe on the
// receiver so callers need no guard.
func (c *Config) RecentPRWindowDays() int {
	if c == nil || c.Dashboard == nil || c.Dashboard.RecentPRWindowDays == nil {
		return RecentPRWindowDaysDefault
	}
	days := *c.Dashboard.RecentPRWindowDays
	if days < 0 {
		return 0
	}
	return min(days, RecentPRWindowDaysMax)
}

// normalizeGroupQuickOptions returns s when it's a known mode, else ""
// (so the resolver falls back to its default for a blank or hand-edited
// garbage value).
func normalizeGroupQuickOptions(s string) string {
	switch s {
	case GroupQuickOptionsHover, GroupQuickOptionsExpanded:
		return s
	default:
		return ""
	}
}

// OverlayBackdrop returns dashboard.overlay_backdrop, with "dim" for anything
// unset or unknown.
func (c *Config) OverlayBackdrop() string {
	if c != nil && c.Dashboard != nil {
		switch v := c.Dashboard.OverlayBackdrop; v {
		case "blur", "both", "none":
			return v
		}
	}
	return "dim"
}

// SnapAssist reports whether a space a snap leaves empty offers the other
// overlay windows. Default true; dashboard.no_snap_assist turns it off.
func (c *Config) SnapAssist() bool {
	return c == nil || c.Dashboard == nil || !c.Dashboard.NoSnapAssist
}

// WindowControls returns dashboard.window_controls, with "auto" for anything
// unset or unknown.
func (c *Config) WindowControls() string {
	if c != nil && c.Dashboard != nil {
		switch v := c.Dashboard.WindowControls; v {
		case "standard", "os", "macos", "windows", "linux", "classic":
			return v
		}
	}
	return "auto"
}

// GroupQuickOptions reports the display mode for the group <summary> quick-
// option chips — config dashboard.group_quick_options. Default "hover"
// (absent block / key or an unknown value): the chips fold to icon-only at
// rest and expand on header hover. "expanded" keeps them always visible.
// Nil-safe on the receiver so callers need no guard.
func (c *Config) GroupQuickOptions() string {
	if c != nil && c.Dashboard != nil {
		if s := normalizeGroupQuickOptions(c.Dashboard.GroupQuickOptions); s != "" {
			return s
		}
	}
	return GroupQuickOptionsHover
}

// normalizeDefaultTerminal returns s when it's a known mode, else "" (so the
// resolver falls back to its default for a blank or hand-edited garbage value).
func normalizeDefaultTerminal(s string) string {
	switch s {
	case DefaultTerminalNative, DefaultTerminalWeb:
		return s
	default:
		return ""
	}
}

// DefaultTerminal reports how the dashboard's spawn auto-focus, per-agent
// focus / open-window / open-terminal actions open a console — config
// dashboard.default_terminal.
// Default "web" (absent block / key or an unknown value): route actions to an
// in-browser terminal pane in the dashboard's Terminals tab. "native" opts
// back into an OS window (with the usual in-browser fallback when none can be
// opened). Nil-safe on the receiver so callers need no guard.
func (c *Config) DefaultTerminal() string {
	if c != nil && c.Dashboard != nil {
		if s := normalizeDefaultTerminal(c.Dashboard.DefaultTerminal); s != "" {
			return s
		}
	}
	return DefaultTerminalWeb
}

// DefaultDirectoryPicker reports the configured directory chooser for local
// dashboard connections. Remote browser connections override this to "web"
// client-side because a native dialog would appear on the agentd host.
func (c *Config) DefaultDirectoryPicker() string {
	if c != nil && c.Dashboard != nil {
		switch c.Dashboard.DefaultDirectoryPicker {
		case DefaultDirectoryPickerNative, DefaultDirectoryPickerWeb:
			return c.Dashboard.DefaultDirectoryPicker
		}
	}
	return DefaultDirectoryPickerWeb
}

// FocusConfig holds window-focus behavior knobs.
//
// RaiseOnly, when true, makes window-focus RAISE an existing terminal
// window only — it never opens a fresh one as a side effect. Default
// (false) keeps the historical behavior: focusing an agent that has no
// attached client opens a new terminal running `tclaude session attach`
// (what macOS does too). Opt-in for permissive compositors where the
// open-on-focus fallback pops up / raises a window unexpectedly on every
// dashboard "show" that resolves to a detached agent. The explicit
// dashboard "open window" action opens a console regardless of this flag.
//
// Tile configures the opt-in auto-tiling pass that runs after a bulk
// window "focus" op (the 🪟 windows… modal, the command palette, or a
// group's focus button) — see TileConfig. Absent / disabled (the
// default) leaves each terminal wherever the OS placed it.
//
// WindowTitle gates whether tclaude stamps the `tclaude:<id>` window/tab
// title on each pane (the tmux set-titles pair in session.runNew + the OSC
// escape in AttachToSession). That title is how the WSL and native-Linux/X11
// focus + tiling paths locate an agent's existing window to raise it; it's
// also what some users find "ugly" on a plain desktop terminal. A *bool so
// absent is distinguishable from an explicit false: nil / true → stamp the
// title (the default, keeps focus-by-title working); explicit false → skip
// both emit sites entirely, so the terminal keeps its own title. Turning it
// off degrades "focus/raise the existing window" to "open a new window"
// wherever focus is title-based (WSL, native-Linux/X11) and disables
// auto-tiling; the explicit dashboard "open window" action is unaffected.
// See WindowTitleEnabled.
type FocusConfig struct {
	RaiseOnly   bool        `json:"raise_only,omitempty"`
	Tile        *TileConfig `json:"tile,omitempty"`
	WindowTitle *bool       `json:"window_title,omitempty"`
}

// Tile layout modes — config focus.tile.layout. "grid" packs windows
// into a near-square grid (the default); "columns" lays them out as
// full-height side-by-side columns; "rows" as full-width stacked rows;
// "cascade" overlaps them with a fixed diagonal step (macOS-style stagger).
// An empty / unknown value falls back to the default (grid) — see TileLayout.
const (
	TileLayoutGrid    = "grid"
	TileLayoutColumns = "columns"
	TileLayoutRows    = "rows"
	TileLayoutCascade = "cascade"
)

// TileConfig configures the auto-tiling pass. When Enabled, a bulk
// window "focus" op that raises/opens more than one window follows up by
// arranging just that focused set into the chosen Layout, so the desktop
// is neatly tiled instead of leaving every window where the OS dropped
// it. All focused windows are gathered onto ONE monitor — the monitor the
// first window is on — so a multi-monitor setup doesn't scatter them or
// straddle the gap. It is best-effort and platform-specific (AppleScript
// on macOS, xdotool/kdotool on native Linux, PowerShell on WSL); an
// unsupported desktop simply no-ops. A single-window focus is never tiled
// (there is nothing to arrange).
//
// Resize controls whether windows are RESIZED to fill the layout. The
// default (false) keeps each window at its current size and only
// repositions it so the set no longer overlaps — the least-intrusive
// "just line them up" behaviour. Set it true for the older screen-filling
// grid, where each window is stretched to fill its layout cell.
//
// Gap is the pixel spacing left between adjacent tiles; Margin is the
// pixel inset kept from the screen work-area edges (useful to clear a
// menu bar / panel the platform's screen query doesn't already exclude).
// Both are pointers so "absent" is distinguishable from an explicit 0:
// nil falls back to the built-in default (defaultTileGap / defaultTileMargin),
// an explicit 0 means flush. See ResolvedTileGeometry.
type TileConfig struct {
	Enabled bool   `json:"enabled,omitempty"`
	Layout  string `json:"layout,omitempty"`
	Resize  bool   `json:"resize,omitempty"`
	Gap     *int   `json:"gap,omitempty"`
	Margin  *int   `json:"margin,omitempty"`
}

// Tiling geometry defaults + the sanity cap Validate/ResolvedTileGeometry
// enforce. An 8px gap gives a visible seam between tiled terminals; the
// default margin is 0 (the platform screen query already excludes the
// dock/taskbar in the common cases). maxTilePixels bounds a hand-edited
// gap/margin so a fat-finger can't shrink every tile to nothing or push
// the whole grid off-screen.
const (
	defaultTileGap    = 8
	defaultTileMargin = 0
	maxTilePixels     = 1000
)

// RaiseOnlyFocus reports whether window focus should be raise-only (raise
// an existing window but never open a fresh one). Nil-safe so callers
// need no guard; the absent default is false (open-on-focus).
func (c *Config) RaiseOnlyFocus() bool {
	if c == nil || c.Focus == nil {
		return false
	}
	return c.Focus.RaiseOnly
}

// WindowTitleEnabled reports whether tclaude should stamp the `tclaude:<id>`
// window/tab title on its panes — config focus.window_title. Default true
// (absent block / key, or an explicit true): the title is on, so the WSL and
// native-Linux/X11 focus-by-title + tiling paths can find an agent's window.
// An explicit false skips the title so a plain desktop terminal keeps its own
// tab title (at the cost of title-based focus/tiling). Nil-safe so callers
// need no guard.
func (c *Config) WindowTitleEnabled() bool {
	if c == nil || c.Focus == nil || c.Focus.WindowTitle == nil {
		return true
	}
	return *c.Focus.WindowTitle
}

// TileOnFocus reports whether a bulk window "focus" op should follow up
// by auto-tiling the focused windows — config focus.tile.enabled. Off by
// default (absent block / key). Nil-safe on the receiver.
func (c *Config) TileOnFocus() bool {
	return c != nil && c.Focus != nil && c.Focus.Tile != nil && c.Focus.Tile.Enabled
}

// normalizeTileLayout returns s when it's a known layout, else "" (so the
// resolver falls back to its default for a blank or hand-edited garbage
// value).
func normalizeTileLayout(s string) string {
	switch s {
	case TileLayoutGrid, TileLayoutColumns, TileLayoutRows, TileLayoutCascade:
		return s
	default:
		return ""
	}
}

// TileResize reports whether the tiling pass should RESIZE windows to
// fill their layout cells — config focus.tile.resize. Default false
// (absent block/key): windows keep their current size and are only
// repositioned. Nil-safe on the receiver.
func (c *Config) TileResize() bool {
	return c != nil && c.Focus != nil && c.Focus.Tile != nil && c.Focus.Tile.Resize
}

// TileLayout reports the tiling layout mode — config focus.tile.layout.
// Default "grid" (absent block/key or an unknown value). Nil-safe on the
// receiver.
func (c *Config) TileLayout() string {
	if c != nil && c.Focus != nil && c.Focus.Tile != nil {
		if l := normalizeTileLayout(c.Focus.Tile.Layout); l != "" {
			return l
		}
	}
	return TileLayoutGrid
}

// ResolvedTileGeometry returns the effective (gap, margin) in pixels for
// the tiling pass, defaulting each absent value to the built-in default
// and clamping a hand-edited out-of-range value to [0, maxTilePixels] so
// readers always get a usable geometry (Validate reports the out-of-range
// value to the Config tab). Nil-safe on the receiver.
func (c *Config) ResolvedTileGeometry() (gap, margin int) {
	gap, margin = defaultTileGap, defaultTileMargin
	if c == nil || c.Focus == nil || c.Focus.Tile == nil {
		return gap, margin
	}
	t := c.Focus.Tile
	if t.Gap != nil {
		gap = min(maxTilePixels, max(0, *t.Gap))
	}
	if t.Margin != nil {
		margin = min(maxTilePixels, max(0, *t.Margin))
	}
	return gap, margin
}
