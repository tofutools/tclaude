package agentd

import (
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// snapshotPayload is the wire shape for /api/snapshot. One round-trip
// gives the page everything it needs to render every tab; the page
// re-fetches on a 2s timer.
type snapshotPayload struct {
	GeneratedAt string `json:"generated_at"`
	Version     string `json:"version"`
	// AssetsVersion fingerprints the embedded dashboard tree this daemon
	// serves (dashboard_assets_version.go). The page compares it against the
	// fingerprint stamped into its own HTML and reloads itself when the
	// daemon starts serving a different frontend — the "agentd upgraded under
	// an open dashboard" case.
	AssetsVersion string               `json:"assets_version"`
	AuthSession   dashboardAuthSession `json:"auth_session"`
	// StaticVersion fingerprints the registry-shaped payload fields that rarely
	// change. The browser echoes it on the next poll; when it still matches,
	// StaticUnchanged is true and those fields are sent as null then restored
	// from the previous snapshot client-side.
	StaticVersion   string           `json:"static_version"`
	StaticUnchanged bool             `json:"static_unchanged,omitempty"`
	Groups          []dashboardGroup `json:"groups"`
	// Names and assignments only: environment values remain behind the
	// sandbox-profile management API. Keeping these in the regular snapshot
	// lets global/group quick selectors repaint without extra polling requests.
	SandboxProfiles       []string `json:"sandbox_profiles"`
	SandboxProfileDefault string   `json:"sandbox_profile_default"`
	// SandboxProfilesRev changes whenever any sandbox profile is created,
	// edited, renamed or deleted, so the dock's sandbox section refetches the
	// full profiles from the management API only when they changed.
	SandboxProfilesRev string           `json:"sandbox_profiles_rev"`
	Agents             []dashboardAgent `json:"agents"`
	// AgentRosterAuthoritative is false when the active-actor query failed and
	// Agents may therefore be only the partial set recovered from group/grant
	// rows. Clients that react to roster departures must ignore such a snapshot
	// rather than treating a transient DB read failure as mass retirement.
	AgentRosterAuthoritative bool `json:"agent_roster_authoritative"`
	// Ungrouped: every active agent that is NOT a member of any group,
	// online or offline alike. Surfaces fresh-spawned agents, loose
	// convs and freshly-promoted offline conversations so the
	// dashboard's virtual "Ungrouped" group + the `+ add member`
	// overlay can show them as drag/add sources without a second
	// round-trip. (The overlay applies its own online filter on top.)
	// Same wire shape as Agents — empty when no loose convs exist.
	Ungrouped []dashboardAgent `json:"ungrouped"`
	// The retired-agents, non-agent conversations and replaced-generations
	// lists used to ride on this 2s snapshot in full. They grow unbounded
	// (a user can accumulate hundreds of retired agents), so each moved to
	// its own paginated, server-filtered endpoint — GET /api/retired,
	// /api/conversations, /api/replaced (dashboard_lists.go) — and is no
	// longer carried here.
	//
	// Pending: dashboard spawns whose conv-id has not materialised yet
	// (the pending_spawns table — JOH-205 inc2). A pending Codex agent
	// has a live tmux pane but is stuck behind a startup gate (untrusted
	// dir, new-hooks-config prompt, OpenAI auth modal), so it never took
	// the first turn that exposes its conv-id and is NOT an enrolled
	// agent yet. Surfaced as a distinct list so the operator can SEE it
	// and click its focus button to open the pane and clear the gate;
	// the sweeper then promotes it into Agents. Empty slice (not nil) so
	// JS .map() / .length are safe.
	Pending     []dashboardPending      `json:"pending"`
	Permissions snapshotPermissionsView `json:"permissions"`
	Slugs       []PermSlug              `json:"slugs"`
	Cron        []dashboardCronJob      `json:"cron"`
	// ExportJobsActive counts the per-agent export jobs still in flight
	// (neither ready nor failed) — the Jobs nav tab's badge. The job rows
	// themselves do NOT ride the snapshot: the Jobs tab fetches its unified,
	// paginated window from GET /api/jobs alongside the poll
	// (dashboard_jobs.go), mirroring the retired/conversations/replaced split.
	ExportJobsActive int `json:"export_jobs_active"`
	// RetiredTotal drives cross-tab badges/actions without fetching the full,
	// paginated retired roster outside the Groups tab.
	RetiredTotal int `json:"retired_total"`
	// Sudo: every active grant across all agents, ordered by conv-id
	// then soonest expiry. Powers the dedicated "Sudo" tab. Per-agent
	// active state also surfaces on Agents[*].ActiveSudo so the Groups
	// + Agents tabs can render the 🔓 indicator without a second
	// round-trip.
	Sudo []dashboardSudoEntry `json:"sudo"`
	// Links surfaces every inter-group link in the system. The dashboard
	// renders these in a dedicated panel (read-only in v1) and uses them
	// to annotate group rows with outbound/inbound counts. Empty slice
	// (not nil) so JS .length / .map() are safe.
	Links []dashboardLink `json:"links"`
	// RouteMap projects durable route authority and ephemeral host leases.
	// Only operator host-loopback endpoints are included; publisher targets,
	// agent endpoints, launch generations, and capability material are omitted.
	// It is absent while the opt-in Groups Route map flag is off.
	RouteMap *dashboardRouteMap `json:"route_map,omitempty"`
	// Usage is the account-wide subscription usage readout (5h + 7d
	// rolling windows) rendered in the dashboard's top bar. Always
	// present — Available=false carries the graceful "n/a" state.
	Usage dashboardUsage `json:"usage"`
	// AuthoredOpenPRs is the cached cross-repository list behind the fixed
	// footer's Open PRs popover. The background poller owns GitHub access; the
	// snapshot only performs one bounded cache read and associates matching
	// agent PR URLs in memory.
	AuthoredOpenPRs dashboardAuthoredOpenPRs `json:"authored_open_prs"`
	// TrackedPRAuthors are the colleague tabs of the same popover: one list
	// per tracked GitHub user, in the order they were added (trackedprs.go).
	TrackedPRAuthors []dashboardAuthoredOpenPRs `json:"tracked_pr_authors,omitempty"`
	// AuthoredOpenMRs is the footer's Open MRs list: the token user's open
	// GitLab merge requests on every configured instance (openmrs.go). Absent
	// unless gitlab_hosts names an instance and the token resolved a user.
	AuthoredOpenMRs *dashboardAuthoredOpenMRs `json:"authored_open_mrs,omitempty"`
	// Templates are the group-template blueprints rendered in the
	// Templates tab. Empty slice (not nil) so JS .map() is safe.
	Templates []templateJSON `json:"templates"`
	// Profiles + Roles are the spawn-profile and role-library registries,
	// carried on the poll so the retractable right-side palette dock
	// (JOH-374) renders them off the live snapshot like the rest of the
	// dashboard — no separate fetch, so a manager edit shows up on the next
	// tick. Both are the same wire shapes their /api/{spawn-profiles,roles}
	// endpoints serve. Empty slices (not nil) so JS .map() is safe.
	Profiles []spawnProfileJSON `json:"profiles"`
	// SpawnProfileDefault is the global spawn-profile assignment used after a
	// group's own default. It rides the snapshot with the profile registry and
	// sandbox-profile assignments so open dashboards observe CLI changes without
	// a separate request on every two-second poll.
	SpawnProfileDefault string     `json:"spawn_profile_default"`
	Roles               []roleJSON `json:"roles"`
	// SavedPrompts is the saved-prompt library for the dock and the spawn
	// dialog's prompt picker; same wire shape as /api/saved-prompts.
	SavedPrompts []db.SavedPrompt `json:"saved_prompts"`
	// QueuedSpawns are spawn-dialog drafts prepared but not launched yet, for
	// the Groups toolbar's queue chip and the per-group queued rows. Same wire
	// shape as /api/queued-spawns. Empty slice (not nil) so JS .map() is safe.
	QueuedSpawns []queuedSpawnJSON `json:"queued_spawns"`
	// CapacityQueue is the capacity line and the spawns waiting for capacity
	// (the Spawn queue block); nil, and omitted, when capacity limits are off
	// and nothing waits. Same shape as GET /v1/spawn-queue.
	CapacityQueue *spawnQueueListJSON `json:"capacity_queue,omitempty"`
	// Messages are the human-facing notifications agents have sent via
	// `tclaude agent notify-human`, newest first — the Messages tab.
	// MessagesUnread is the count of unread ones, driving the tab badge.
	Messages       []dashboardHumanMessage `json:"messages"`
	MessagesUnread int                     `json:"messages_unread"`
	// AccessRequests are the in-flight human-approval requests — an agent is
	// BLOCKED waiting for the operator to approve/deny a permission-gated
	// action (formerly the loopback-only browser popup). They ride the poll so
	// the Messages tab's "Access requests" folder + the attention overlay
	// render off the live snapshot and work over the remote listener too.
	// AccessRequestsPending is the count, driving the blinking tab badge +
	// overlay. Empty slice (not nil) so JS .map()/.length are safe.
	AccessRequests        []dashboardAccessRequest `json:"access_requests"`
	AccessRequestsPending int                      `json:"access_requests_pending"`
	// Plugins are the human-managed external integrations on the
	// Plugins tab, with their cached step-check statuses (the snapshot
	// never runs the checks itself — see plugins.go). PluginsCatalog is
	// the built-in set offered for one-click install; PluginsWarn
	// counts plugins with a failing check and drives the nav badge.
	Plugins        []dashboardPlugin `json:"plugins"`
	PluginsCatalog []Plugin          `json:"plugins_catalog"`
	PluginsWarn    int               `json:"plugins_warn"`
	// PluginsError carries a plugins.json read/parse failure so the tab
	// shows "registry broken: …" instead of a silently empty list. The
	// poll itself stays 200 — one bad file must not take down every tab.
	PluginsError string `json:"plugins_error,omitempty"`
	// PluginsTabVisible drives the Plugins tab's auto-hide, mirroring the
	// Costs tab's rule: true when at least one plugin is installed, OR
	// plugins.json failed to read (so the error is never hidden), OR the
	// human opted into always showing it (config
	// dashboard.always_show_plugins_tab). When false the dashboard hides the
	// Plugins nav button + section entirely — the "don't show an empty
	// Plugins tab" rule for the majority of users who never define one.
	PluginsTabVisible bool `json:"plugins_tab_visible"`
	// ProcessesEnabled and TriggersEnabled are always true. They once carried
	// the retired features.processes / features.triggers flags. The dashboard
	// reads them only from a peer's snapshot (feature-availability.js): an
	// older peer may still have the feature off, and a restricted peer gets
	// them denied (false) because it is shared neither.
	ProcessesEnabled bool `json:"processes_enabled"`
	TriggersEnabled  bool `json:"triggers_enabled"`
	// StewardsDisabled is the steward opt-out (steward.disabled): the
	// companion offers no Steward, though its windows stay. Re-read on
	// every snapshot so a Config-tab change takes effect without a restart.
	StewardsDisabled bool `json:"stewards_disabled,omitempty"`
	// Voice is the resolved dashboard.voice: spoken phrases and the opt-in
	// hands-free settings.
	Voice config.VoiceConfig `json:"voice"`
	// StewardName is what the operator calls their Steward, empty by default.
	StewardName string `json:"steward_name,omitempty"`
	// StewardActive is the agent the operator's "steward" means, and StewardRoster
	// every Steward and standby to switch between.
	StewardActive string               `json:"steward_active,omitempty"`
	StewardRoster []stewardRosterEntry `json:"steward_roster,omitempty"`
	// StewardElevated is what each live Steward holds beyond Config and the
	// defaults, by agent id.
	StewardElevated map[string]stewardElevation `json:"steward_elevated,omitempty"`
	// StewardAccess is where a live Steward's access comes from, for its editors.
	StewardAccess *stewardAccess `json:"steward_access,omitempty"`
	// KnowledgeSource is the source download's question and progress.
	KnowledgeSource *knowledgeSourceView `json:"knowledge_source,omitempty"`
	// GroupsRouteMapEnabled gates the experimental Members | Route map Groups
	// subview and its bounded route projection. Re-read on every snapshot so a
	// Config-tab change takes effect without restarting agentd.
	GroupsRouteMapEnabled bool `json:"groups_route_map_enabled"`
	// GroupAttachmentsMode selects the experimental paperclip presentation on
	// group titles: off, float, or fixed. The attachment data and API remain
	// available while the mode is off, so existing references are not
	// destroyed. Re-read on every snapshot so a Config-tab change takes effect
	// without restarting agentd.
	GroupAttachmentsMode config.GroupAttachmentsMode `json:"group_attachments_mode"`
	// TerminalPaletteShortcut mirrors the opt-in feature switch. The palette
	// island uses it only for requests originating inside xterm; Ctrl/Cmd+K
	// elsewhere remains the normal dashboard shortcut. Re-read on every
	// snapshot so a Config-tab change takes effect without restarting agentd.
	TerminalPaletteShortcut bool `json:"terminal_command_palette_shortcut_enabled"`
	// Keybindings carries dashboard.keybindings, the operator's shortcut
	// overrides by registry id. Re-read on every snapshot so a Config-tab
	// change takes effect without restarting agentd.
	Keybindings map[string]string `json:"keybindings,omitempty"`
	// RecordedSandboxDetails gates the optional details chevron beside each
	// sandbox badge. The compact badge and tooltip remain visible either way.
	// Re-read on every snapshot so a Config-tab change takes effect without
	// restarting agentd.
	RecordedSandboxDetails bool `json:"recorded_sandbox_details_enabled"`
	// UserDefaultModel is the "model" key from the user-level
	// ~/.claude/settings.json — what every claude launched without
	// --model falls back to. "" = unset (claude's built-in default).
	// Shown in the Groups tab header and used by the spawn modal to
	// label what "Default" actually resolves to.
	UserDefaultModel string `json:"user_default_model"`
	// Harnesses is the catalog of spawnable harnesses (claude, codex) with
	// each one's valid model/effort/sandbox menus and capability flags. The
	// spawn dialog drives its harness selector + per-harness model/effort/
	// sandbox menus off this, and the per-row controls gate rename on the
	// agent's harness can_rename (JOH-162). Built from the harness registry
	// so a newly-registered harness appears with no dashboard edit.
	Harnesses []dashboardHarness `json:"harnesses"`
	// SandboxImpl is the sandbox-IMPLEMENTATION catalog: which layer owns
	// OS-level containment for a launch. Host-wide rather than per-harness,
	// because whether this machine can run the tclaude layer has nothing to do
	// with which harness was selected — the per-harness half of the question is
	// dashboardHarness.CanTclaudeLayer.
	SandboxImpl dashboardSandboxImpl `json:"sandbox_impl,omitzero"`
	PopupBase   string               `json:"popup_base"` // for tray-shareable display
	// NotificationsEnabled mirrors config.notifications.enabled — the
	// master OS-notification switch above the per-group / per-agent
	// filters. Drives the top-bar bell toggle.
	NotificationsEnabled bool `json:"notifications_enabled"`
	// SpawnNameNormalize mirrors config.SpawnNameNormalizeEnabled (config
	// agent.spawn_name_normalize, default on). The spawn modal reads it to
	// decide whether to auto-normalize an invalid agent name to the safe
	// branch-token charset (the default) or reject it with the inline error.
	SpawnNameNormalize bool `json:"spawn_name_normalize"`
	// VegasInRegularMode mirrors config slop.vegas_in_regular_mode — the
	// opt-in that surfaces the Vegas music features (the Vegas tab, the
	// header volume mixer + sound switch, the lounge radio) on the PLAIN
	// dashboard, decoupled from the full slop casino re-skin. refresh.js
	// toggles body.vegas off this so the music/tab/volume light up in
	// regular mode without the slot machines and FX. Default false.
	VegasInRegularMode bool `json:"vegas_in_regular_mode"`
	// HidePullLever mirrors config slop.hide_pull_lever — the opt-out that
	// hides the slop-mode side pull-lever (the casino lever on the Groups
	// tab). refresh.js toggles body.hide-slop-lever off this; CSS then drops
	// the lever while leaving the rest of slop mode intact. Default false.
	HidePullLever bool `json:"hide_pull_lever"`
	// ActivityBots mirrors config dashboard.activity_bots — the per-mode
	// STYLE of the deduped "activity bot" indicator in group headers + the
	// top bar. Each field is "emoji" | "sprites" | "off"; the front-end
	// (render.js) emits a regular-mode, a slop-mode and a wizard-mode row and
	// CSS shows the one for the current mode. Defaults: regular + wizard emoji
	// (wizard's opt-in "sprites" resolves to the wizard sheets), slop sprites.
	// See ActivityBotsRegular / ActivityBotsSlop / ActivityBotsWizard.
	ActivityBots activityBotsView `json:"activity_bots"`
	// HScrollFollow mirrors config dashboard.hscroll_follow — whether the
	// full-bleed chrome bars (header / nav / slop marquee) keep their content
	// pinned to the viewport (follow, the default) while the page is scrolled
	// sideways, or let it scroll off (static). refresh.js toggles
	// body.hscroll-follow off this each poll; it replaces the old per-browser
	// header toggle button. Default true. See Config.HScrollFollow.
	HScrollFollow bool `json:"hscroll_follow"`
	// GroupQuickOptions mirrors config dashboard.group_quick_options — the
	// display mode for the editable chips in each group <summary> header
	// ("hover" folds them to icon-only at rest + expands on hover, the
	// default; "expanded" always shows them). refresh.js toggles
	// body.group-quick-fold off this each poll. See Config.GroupQuickOptions.
	GroupQuickOptions string `json:"group_quick_options"`
	// OverlayBackdrop mirrors config dashboard.overlay_backdrop: what
	// full-screen previews do to the page behind them. refresh.js applies it
	// to the root element each poll. See Config.OverlayBackdrop.
	OverlayBackdrop string `json:"overlay_backdrop"`
	// WindowControls mirrors config dashboard.window_controls: the look of the
	// companion window's controls. See Config.WindowControls.
	WindowControls string `json:"window_controls"`
	// SnapAssist mirrors config dashboard.no_snap_assist, inverted: whether a
	// space an overlay snap leaves empty offers the other windows. See
	// Config.SnapAssist.
	SnapAssist bool `json:"snap_assist"`
	// DefaultTerminal mirrors config dashboard.default_terminal — how spawn
	// auto-focus, per-agent focus / open-window / open-terminal actions and bulk
	// windows-modal focus open a console: "native" (pop a native OS window) or
	// "web" (the default; open an in-browser pane in the Terminals tab).
	// The browser reads this off each poll to route those actions; the dedicated
	// "web term" / "web window" buttons ignore it (always web).
	DefaultTerminal string `json:"default_terminal"`
	// TerminalAttach carries the resolved browser-terminal resize strategy.
	// The frontend applies it to newly opened terminal widgets; existing
	// attachments keep the strategy they started with.
	TerminalAttach dashboardTerminalAttach `json:"terminal_attach"`
	// DefaultDirectoryPicker mirrors dashboard.default_directory_picker for
	// local connections. The client additionally forces web mode whenever its
	// hostname is not loopback.
	DefaultDirectoryPicker string `json:"default_directory_picker"`
	// ShowAgentHideButton mirrors config dashboard.show_agent_hide_button —
	// whether each agent row's "hide window" button (the slashed-eye beside
	// "focus", data-act="hide") is shown. Off by default: the button detaches
	// the agent's terminal window and is far less used than "focus", so the
	// row hides it to stay tight. refresh.js toggles body.show-agent-hide-btn
	// off this each poll; CSS drops the button unless the class is present.
	// See Config.ShowAgentHideButton.
	ShowAgentHideButton bool `json:"show_agent_hide_button"`
	// ShowGroupDescription mirrors config dashboard.show_group_description —
	// whether each group header's 📝 description chip is shown. Off by default:
	// group descriptions are a deprecated, display-only feature, so the chip is
	// hidden to keep headers tight. refresh.js toggles body.show-group-description
	// off this each poll; CSS drops the chip unless the class is present. See
	// Config.ShowGroupDescription.
	ShowGroupDescription bool `json:"show_group_description"`
	// SidePanelEdgeReveal mirrors config dashboard.side_panel_edge_reveal —
	// whether a folded right side panel floats in when the pointer reaches the
	// right edge. On by default; refresh.js hands it to side-panel-reveal.js
	// each poll. See Config.SidePanelEdgeReveal.
	SidePanelEdgeReveal bool `json:"side_panel_edge_reveal"`
	// SidePanelTuning mirrors config dashboard.side_panel_* (the predictive
	// peek's switches and the reveal's tuning), defaulted and clamped. See
	// Config.SidePanelTuning.
	SidePanelTuning config.SidePanelTuning `json:"side_panel_tuning"`
	// ShowFinalPRChecks mirrors config dashboard.show_final_pr_checks —
	// whether merged/closed PRs keep a non-failing CI checks pill. Off by
	// default; refresh.js toggles body.show-final-pr-checks off this each
	// poll. See Config.ShowFinalPRChecks.
	ShowFinalPRChecks bool `json:"show_final_pr_checks"`
	// StayOnTerminalsAfterLastClose mirrors config
	// dashboard.stay_on_terminals_after_last_close — whether closing the last
	// open terminal leaves the operator on the Terminals tab (true) or moves
	// them to Groups (false, the default). refresh.js hands it to
	// terminal-close-nav.js each poll. See Config.StayOnTerminalsAfterLastClose.
	StayOnTerminalsAfterLastClose bool `json:"stay_on_terminals_after_last_close"`
	// FusedToolbarFixedNode mirrors config dashboard.fused_toolbar_fixed_node
	// — whether the fused Groups toolbar stays on the node picked in its chip
	// (true) or follows the group last acted on (false, the default).
	// refresh.js hands it to toolbar-node.js each poll.
	FusedToolbarFixedNode bool `json:"fused_toolbar_fixed_node"`
	// UsageCollapseSubviews mirrors config dashboard.usage_collapse_subviews
	// — whether the Usage tab collapses to its only populated view (true) or
	// always offers Overview, Quota and Costs (false, the default).
	// usage-hub-state.js reads it.
	UsageCollapseSubviews bool `json:"usage_collapse_subviews"`
	// CostTabVisible drives the Costs tab's auto-hide: true when there is
	// real pay-per-token spend to show OR a subscription account has opted
	// into the WHAT-IF view (config cost.show_on_subscription). When false
	// the dashboard hides the Costs nav button + section entirely — the
	// "don't show an empty Costs tab on a subscription" rule.
	CostTabVisible bool `json:"cost_tab_visible"`
	// CostTabWhatIf is the subscription-estimate display opt-in. The Costs API
	// chooses real versus WHAT-IF per row; this flag reveals hypothetical
	// per-agent badges even when real spend also exists.
	CostTabWhatIf bool `json:"cost_tab_whatif"`
	// BrokerRefusalsTotal counts EVERY brokered hook/statusline request
	// the daemon refused inside the refusal recorder's window, including
	// the ones a per-agent badge already reports.
	//
	// The overlap is deliberate. A badge is attached to the row the daemon
	// resolved, and in the pid-reuse case that row is a dead session — so
	// the badge can be recorded and then rendered nowhere at all (offline
	// agents hidden, or a conv that was never grouped). A machine-level
	// total cannot say which agent is affected without the attribution the
	// design forbids, but it can say the condition exists, which is what
	// makes it worth looking.
	BrokerRefusalsTotal int `json:"broker_refusals_total,omitempty"`
	// BrokerRefusalsUnplaceable is the subset of BrokerRefusalsTotal the
	// daemon could not place on any row at all.
	//
	// It is a COUNT and not an attribution on purpose: with no resolved
	// row there is nothing trustworthy to attribute to, and the only
	// identifier such a request carries is the caller-chosen session id
	// the refusal already declined to trust. Which agent is affected has
	// to be found from the daemon log, and that is the honest limit of
	// what this can say.
	BrokerRefusalsUnplaceable int `json:"broker_refusals_unplaceable,omitempty"`
	// UsageTabVisible is true while either subscription provider has retained
	// quota samples or OpenCode has retained provider activity that needs a
	// coverage warning in the 90-day history window.
	UsageTabVisible bool `json:"usage_tab_visible"`
	// CompanionCues are agents' recent dashboard-guidance cues (dashboard_guide.go),
	// played once each by the companion island.
	CompanionCues []companionCue `json:"companion_cues,omitempty"`
	// LivePages are the pages agents are showing (pages.go). Each carries its
	// stage origin, whose token is the page's key: local dashboard only. A
	// page's rev changes whenever it is shown or updated.
	LivePages []pageView `json:"live_pages,omitempty"`
	// GroupResources are every active group's pages, drawing boards, processes and
	// shell in one shape, with whether each is running (group_resources_snapshot.go).
	// Local dashboard only, like LivePages.
	GroupResources []groupResource `json:"group_resources,omitempty"`
	// RemoteAccess surfaces the optional network-exposed dashboard listener's
	// live state so the Config tab can guide setup: whether the cert/passphrase
	// material has been generated (run `tclaude remote-access setup` first) and
	// whether THIS agentd already has the listener running. config.json carries
	// the saved intent; this carries the live reality so the UI can flag a
	// "no material yet" foot-gun and a "restart agentd to apply" pending state.
	RemoteAccess dashboardRemoteAccess `json:"remote_access"`
}

type dashboardTerminalAttach struct {
	Mode                 string `json:"mode"`
	InitialResizeDelayMS int    `json:"initial_resize_delay_ms"`
	RepairDelayMS        int    `json:"repair_delay_ms"`
	PreAttachDelayMS     int    `json:"pre_attach_delay_ms"`
}

type dashboardAuthSession struct {
	MintedAt  string `json:"minted_at"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

// activityBotsView carries the resolved per-mode activity-bot style to the
// dashboard — each is "emoji" | "sprites" | "off" (config
// dashboard.activity_bots, defaulted by ActivityBotsRegular /
// ActivityBotsSlop / ActivityBotsWizard). The front-end picks Regular vs Slop
// vs Wizard off body.slop / body.wizard.
type activityBotsView struct {
	Regular string `json:"regular"`
	Slop    string `json:"slop"`
	Wizard  string `json:"wizard"`
}

// dashboardRemoteAccess is the snapshot view of the remote-access feature's
// runtime state — distinct from the config.json `remote_access` block the
// Config tab edits (delivered via /api/config). The dashboard reads this to
// render accurate status next to the toggle without a second round-trip.
type dashboardRemoteAccess struct {
	// MaterialExists is remoteaccess.Exists(): whether `tclaude remote-access
	// setup` has generated the CA/cert/passphrase material. Enabling the
	// listener without it is a no-op (agentd logs + skips), so the UI warns.
	MaterialExists bool `json:"material_exists"`
	// Running reports whether THIS agentd process started the remote listener
	// (true only after a successful startRemoteServer at boot). Lets the UI
	// distinguish "enabled & live" from "enabled but a restart is pending".
	Running bool `json:"running"`
	// RunningBind is the address the live listener is actually serving on,
	// "" when Running is false. The UI compares it to the edited bind to spot
	// an unsaved/pending bind change.
	RunningBind string `json:"running_bind"`
}

// dashboardHarness is the snapshot view of one spawnable harness — its
// identifier + human label, the model/effort/sandbox values its spawn
// menus offer, and the capability flags the per-row controls gate on. The
// dashboard never hard-codes a harness; it renders this list.
type dashboardHarness struct {
	// Name is the stable identifier ("claude", "codex") forwarded as the
	// spawn body's `harness` and matched against an agent's state.harness.
	Name string `json:"name"`
	// DisplayName is the human label ("Claude Code", "Codex").
	DisplayName string `json:"display_name"`
	// Models is the curated suggestion list for Model menus. It need not be an
	// allow-list: harness validation remains authoritative and every populated
	// dropdown includes a custom-ID entry. Empty means a harness has no
	// suggestions, so the dashboard falls back to a free-text model field.
	Models []string `json:"models"`
	// EffortLevels is the reasoning-effort scale for the Effort menu, in
	// ascending order. Both harnesses share tclaude's levels today.
	EffortLevels []string `json:"effort_levels"`
	// HarnessBuiltinModes lists the launch-time harness-builtin sandbox modes this harness
	// accepts for the spawn dialog's Sandbox selector (Codex: tclaude-agent /
	// workspace-write / read-only / danger-full-access; Claude Code:
	// inherit / on / off). Empty only for a harness with no sandbox catalog.
	HarnessBuiltinModes []string `json:"sandbox_modes"`
	// DefaultSandbox is the recommended default mode the spawn dialog
	// pre-selects (Codex: the managed tclaude-agent profile; Claude Code:
	// inherit). "" when HarnessBuiltinModes is empty.
	DefaultSandbox string `json:"default_sandbox"`
	// HarnessBuiltinModeHelp maps each sandbox mode value to a one-line description
	// the dialog shows as a live hint for the selected option (notably its
	// agentd-socket reachability). {} (not null) when the harness has no
	// launch sandbox, so the JS lookup is always safe.
	HarnessBuiltinModeHelp map[string]string `json:"sandbox_mode_help"`
	// ApprovalModes lists the launch-time approval/permission modes this
	// harness surfaces as a dropdown (Claude Code: inherit + its
	// --permission-mode values; Codex: its --ask-for-approval policies).
	// The ApprovalCatalog parallels HarnessBuiltinModes.
	ApprovalModes []string `json:"approval_modes"`
	// DefaultApproval is the recommended approval mode the dialog pre-selects
	// (Claude Code: auto). "" when ApprovalModes is empty.
	DefaultApproval string `json:"default_approval"`
	// ProfileRecommendedApproval is the approval mode a newly authored spawn
	// profile should pin when that recommendation intentionally differs from the
	// harness's unprofiled fallback. OpenCode uses allow-tools together with the
	// profile recommendation for tclaude-layer; its bare-launch default remains
	// deny so an unprofiled, unconfined launch does not gain autonomy.
	ProfileRecommendedApproval string `json:"profile_recommended_approval,omitempty"`
	// ProfileRecommendedSandboxImplementation is the matching OS-containment
	// recommendation for spawn profiles. It is separate from DefaultSandbox,
	// which is the harness-native mode axis rather than the implementation axis.
	ProfileRecommendedSandboxImplementation string `json:"profile_recommended_sandbox_implementation,omitempty"`
	// ApprovalModeHelp maps each approval mode to a one-line hint (notably
	// whether it is safe for a detached agent). {} (not null) when the harness
	// surfaces no approval modes, so the JS lookup is always safe.
	ApprovalModeHelp map[string]string `json:"approval_mode_help"`
	// ToolsModes is the OpenCode-only uniform action for the built-in tool
	// baseline. Empty on harnesses without a ToolGovernance catalog.
	ToolsModes    []string          `json:"tools_modes"`
	DefaultTools  string            `json:"default_tools"`
	ToolsModeHelp map[string]string `json:"tools_mode_help"`
	// AskTimeoutModes lists the Claude Code AskUserQuestion idle-timeout values
	// this harness surfaces as a dropdown (inherit / never / 60s / 5m / 10m).
	// Empty for a harness with no AskUserQuestion dialog (Codex), so the dialog
	// hides the selector. The AskTimeoutCatalog parallel to HarnessBuiltinModes.
	AskTimeoutModes []string `json:"ask_timeout_modes"`
	// DefaultAskTimeout is the recommended value the dialog pre-selects (Claude
	// Code: inherit). "" when AskTimeoutModes is empty.
	DefaultAskTimeout string `json:"default_ask_timeout"`
	// AskTimeoutModeHelp maps each value to a one-line hint. {} (not null) when
	// the harness surfaces no timeout, so the JS lookup is always safe.
	AskTimeoutModeHelp map[string]string `json:"ask_timeout_mode_help"`
	// CanRename / CanCompact mirror Harness.CanRename / CanCompact — the
	// deliverable-action predicates the per-row controls gate on. Note
	// CanRename is true for Codex (it renames via its ConvStore even
	// without an in-pane /rename), so the dashboard keeps Codex renameable.
	CanRename  bool `json:"can_rename"`
	CanCompact bool `json:"can_compact"`
	// CanMailMidTurn mirrors Harness.MailMidTurn, which continuous dictation
	// gates on.
	CanMailMidTurn bool `json:"can_mail_mid_turn"`
	// CanSandbox reports whether the harness takes a launch sandbox flag —
	// the same condition as a non-empty HarnessBuiltinModes, surfaced explicitly
	// so the dialog has a single boolean to gate the sandbox row on.
	CanSandbox bool `json:"can_sandbox"`
	// CanBuiltinOSSandbox is the narrower descriptor capability behind the
	// harness-builtin implementation. OpenCode's mode catalog keeps
	// CanSandbox true while this stays false, so the picker can omit a
	// confinement claim that does not exist.
	CanBuiltinOSSandbox bool `json:"can_builtin_os_sandbox"`
	// CanApproval reports whether the harness has a launch approval/permission
	// catalog. The dialog's approval row additionally gates on a non-empty
	// ApprovalModes, mirroring how the sandbox row gates on can_sandbox &&
	// sandbox_modes.length.
	CanApproval bool `json:"can_approval"`
	CanTools    bool `json:"can_tools"`
	// CanAutoReview reports whether approval requests can be routed to a
	// harness-owned reviewer instead of a human. This is intentionally distinct
	// from CanApproval: Codex exposes both axes, while Claude Code's permission
	// catalog has no separate approvals_reviewer control.
	CanAutoReview bool `json:"can_auto_review"`
	// CanDirTrust mirrors Harness.SupportsDirTrust — true for a harness whose
	// trust-folder dialog tclaude can pre-seed (Claude Code, Codex), false for
	// one with no such dialog. The spawn dialog's and profile editor's trust-dir
	// controls gate on this instead of naming a harness, so the two harnesses
	// that have the dialog both surface the checkbox.
	CanDirTrust bool `json:"can_dir_trust"`
	// DirTrustStore is the config file the trust-dir opt-in would edit
	// (~/.codex/config.toml, ~/.claude/.claude.json), so the checkbox can name the side
	// effect the operator is consenting to. "" when CanDirTrust is false.
	DirTrustStore string `json:"dir_trust_store,omitempty"`
	// CanAskTimeout reports whether the harness has a launch AskUserQuestion
	// idle-timeout catalog (Claude Code). The dialog's timeout row gates on this
	// + a non-empty AskTimeoutModes, mirroring the sandbox row.
	CanAskTimeout bool `json:"can_ask_timeout"`
	// CanRemoteControl mirrors Harness.CanRemoteControl — true only for a
	// harness with a built-in Remote Access toggle (Claude Code), false for
	// one without it (Codex). The per-row remote-control toggle gates on
	// this exactly the way the rename control gates on CanRename (JOH-259).
	CanRemoteControl bool `json:"can_remote_control"`
	// CanObserveAwaitingInput mirrors the registry's live question-waiting
	// capability. Trigger authoring uses it to explain when an
	// agent.awaiting_input fact can be known rather than silently treating an
	// unsupported harness as false.
	CanObserveAwaitingInput bool `json:"can_observe_awaiting_input"`
	// CanAutoMemory mirrors Harness.CanAutoMemory — true only for a harness
	// with an auto-memory system tclaude can steer (Claude Code), false for one
	// without it (Codex). The spawn dialog and profile editor gate their
	// auto-memory controls on this.
	CanAutoMemory bool `json:"can_auto_memory"`
	// CanPeerMessaging mirrors Harness.CanPeerMessaging — true only for a
	// harness with a cross-session messaging system tclaude can steer.
	CanPeerMessaging bool `json:"can_peer_messaging"`
	// CanSSHWorkaround is true for Linux harnesses. Codex's managed sandbox and
	// every harness in tclaude's packet sandbox can need an ownership-safe copy
	// of the host SSH client configuration.
	CanSSHWorkaround bool `json:"can_ssh_workaround"`
	// CanContextFeatures mirrors Harness.CanContextFeatures — true only for a
	// harness whose startup context tclaude can trim (Claude Code). The spawn
	// dialog and profile editor gate their "Context…" control on it.
	CanContextFeatures bool `json:"can_context_features"`
	// CanAutoCompactWindow mirrors Harness.CanAutoCompactWindow — true only for a
	// harness whose auto-compaction capacity tclaude can pin (Claude Code). The
	// spawn dialog and profile editor gate their token-window input on it.
	CanAutoCompactWindow bool `json:"can_auto_compact_window"`
	// CanContextWindowMax is the Copilot-only configured/assumed context cap
	// control. It is separate from the observed context_window_size snapshot.
	CanContextWindowMax bool `json:"can_context_window_max"`
	// CanCopilotAPI is the Copilot-only API-backed-drive opt-in. The spawn
	// dialog and profile editor gate their checkbox on it.
	CanCopilotAPI             bool   `json:"can_copilot_api"`
	CanCodexAppServer         bool   `json:"can_codex_app_server"`
	CodexNativeRegistryReady  bool   `json:"codex_native_registry_ready"`
	CodexNativeRegistryReason string `json:"codex_native_registry_reason,omitempty"`
	CanFastMode               bool   `json:"can_fast_mode"`
	// ModelProxyDialect is the gateway wire dialect (anthropic, openai)
	// the harness can be routed through; empty when it cannot use one.
	ModelProxyDialect string `json:"model_proxy_dialect,omitempty"`
	// CanTclaudeLayer reports whether the tclaude-layer sandbox
	// implementation can confine this harness's authoritative tool executor.
	// Read through the capability path (session.ValidateTclaudeLayerHarness),
	// never a harness-name switch in the dialog.
	CanTclaudeLayer bool `json:"can_tclaude_layer"`
	// TclaudeLayerServerBoundary tells disclosure surfaces to use the
	// relay-free server capability probe rather than the interactive pane
	// probe. It does not grant the capability; CanTclaudeLayer remains the gate.
	TclaudeLayerServerBoundary bool `json:"tclaude_layer_server_boundary,omitempty"`
	// AutoCompactWindowMin / Max are the accepted bounds, so the dialog can set
	// the input's own min/max and reject a slipped digit before the round trip.
	// Both 0 for a harness that cannot pin a window.
	AutoCompactWindowMin int64 `json:"auto_compact_window_min,omitempty"`
	AutoCompactWindowMax int64 `json:"auto_compact_window_max,omitempty"`
	ContextWindowMaxMin  int64 `json:"context_window_max_min,omitempty"`
	ContextWindowMaxMax  int64 `json:"context_window_max_max,omitempty"`
	// ContextFeatures is the trim catalog this harness offers, in presentation
	// order, so the dialog can render the rows (label + description + cautions)
	// without a second endpoint. [] (not null) for a harness with none, so the
	// page's .map() is always safe.
	ContextFeatures []dashboardContextFeature `json:"context_features"`
}

// dashboardContextFeature is one row of the startup-context trim catalog as the
// dashboard sees it. It is a projection of harness.ContextFeature minus the
// delivery mechanism (env var / settings key), which is an implementation
// detail the operator does not choose between.
type dashboardContextFeature struct {
	Slug string `json:"slug"`
	// Label / Descr are the row's human copy.
	Label string `json:"label"`
	Descr string `json:"descr"`
	// Heavy marks the biggest startup-context wins so the UI can lead with them.
	Heavy bool `json:"heavy"`
	// Caution is a non-empty warning for a trim that needs a deliberate decision.
	Caution string `json:"caution,omitempty"`
}

// buildHarnessCatalog assembles the spawnable-harness catalog for the
// snapshot from the harness registry. Only spawnable harnesses (those with
// a Spawner + ModelCatalog) are listed — the spawn dialog is the only
// consumer, and a non-spawnable harness has nothing to offer it. Ordered
// by Names() (sorted) so the dialog's harness selector is stable.
func buildHarnessCatalog() []dashboardHarness {
	out := []dashboardHarness{}
	for _, name := range harness.Names() {
		h, err := harness.ResolveSpawnable(name)
		if err != nil {
			continue // not spawnable — skip
		}
		dh := dashboardHarness{
			Name:                    h.Name,
			DisplayName:             h.DisplayName,
			Models:                  h.Models.Models(),
			EffortLevels:            h.Models.EffortLevels(),
			CanRename:               h.CanRename(),
			CanCompact:              h.CanCompact(),
			CanMailMidTurn:          h.MailMidTurn,
			CanSandbox:              h.SupportsSandbox(),
			CanBuiltinOSSandbox:     h.SupportsBuiltinOSSandbox(),
			CanApproval:             h.SupportsApproval(),
			CanTools:                h.SupportsToolGovernance(),
			CanAutoReview:           h.SupportsAutoReview(),
			CanDirTrust:             h.SupportsDirTrust(),
			DirTrustStore:           harness.DirTrustStore(h),
			CanAskTimeout:           h.SupportsAskTimeout(),
			CanRemoteControl:        h.CanRemoteControl(),
			CanObserveAwaitingInput: h.SupportsAwaitingInputObservation(),
			CanAutoMemory:           h.CanAutoMemory(),
			CanPeerMessaging:        h.CanPeerMessaging(),
			CanSSHWorkaround:        h.CanSSHWorkaround(),

			CanContextFeatures:   h.CanContextFeatures(),
			CanAutoCompactWindow: h.CanAutoCompactWindow(),
			CanContextWindowMax:  h.Name == harness.CopilotName,
			CanCopilotAPI:        h.CanCopilotAPI(),
			CanCodexAppServer:    h.CanCodexAppServer(),
			CanFastMode:          h.CanFastMode(),
			// The platform check alone passes every harness on Linux; a harness
			// that has not declared its single-wall launch posture (Gemini CLI
			// today) is refused at launch by TclaudeLayerHarnessBuiltinMode, so
			// the dialog must not offer what the launch will reject.
			CanTclaudeLayer: session.ValidateTclaudeLayerHarness(h.Name) == nil &&
				strings.TrimSpace(h.TclaudeLayerMode) != "",
			TclaudeLayerServerBoundary: session.TclaudeLayerUsesServerBoundary(h.Name),
		}
		if h.SupportsModelProxy() {
			dh.ModelProxyDialect = h.ModelProxyDialect()
		}
		if h.Name == harness.OpenCodeName {
			dh.ProfileRecommendedApproval = harness.OpenCodeApprovalAllowTools
			dh.ProfileRecommendedSandboxImplementation = string(sandboxpolicy.ImplementationTclaudeLayer)
		}
		if h.Name == harness.PiName {
			// pi has no sandbox of its own and never asks before a tool
			// call, so tclaude's sandbox is what makes its auto posture safe.
			dh.ProfileRecommendedSandboxImplementation = string(sandboxpolicy.ImplementationTclaudeLayer)
		}
		if h.Name == harness.CodexName {
			if registryErr := codexNativeRegistryReadiness(); registryErr == nil {
				dh.CodexNativeRegistryReady = true
			} else {
				dh.CodexNativeRegistryReason = registryErr.Error()
			}
		}
		if dh.CanAutoCompactWindow {
			dh.AutoCompactWindowMin = harness.MinAutoCompactWindow
			dh.AutoCompactWindowMax = harness.MaxAutoCompactWindow
		}
		if dh.CanContextWindowMax {
			dh.ContextWindowMaxMin = harness.MinCopilotContextWindow
			dh.ContextWindowMaxMax = harness.MaxCopilotContextWindow
		}
		dh.ContextFeatures = []dashboardContextFeature{}
		if h.CanContextFeatures() {
			for _, f := range harness.ContextFeatures() {
				dh.ContextFeatures = append(dh.ContextFeatures, dashboardContextFeature{
					Slug: f.Slug, Label: f.Label, Descr: f.Descr,
					Heavy: f.Heavy, Caution: f.Caution,
				})
			}
		}
		if dh.Models == nil {
			dh.Models = []string{} // JSON [] not null, so JS .map() is safe
		}
		dh.HarnessBuiltinModeHelp = map[string]string{}
		if h.SupportsSandbox() {
			dh.HarnessBuiltinModes = h.Sandbox.Modes()
			dh.DefaultSandbox = h.Sandbox.DefaultMode()
			for _, m := range dh.HarnessBuiltinModes {
				dh.HarnessBuiltinModeHelp[m] = h.Sandbox.ModeHelp(m)
			}
		} else {
			dh.HarnessBuiltinModes = []string{}
		}
		// Approval modes mirror the sandbox block. The dialog gates its row on a
		// non-empty ApprovalModes as well as CanApproval.
		dh.ApprovalModeHelp = map[string]string{}
		dh.ApprovalModes = []string{}
		if h.SupportsApproval() {
			if modes := h.Approval.Modes(); len(modes) > 0 {
				dh.ApprovalModes = modes
				dh.DefaultApproval = h.Approval.DefaultPolicy()
				for _, m := range modes {
					dh.ApprovalModeHelp[m] = h.Approval.ModeHelp(m)
				}
			}
		}
		dh.ToolsModeHelp = map[string]string{}
		dh.ToolsModes = []string{}
		if h.SupportsToolGovernance() {
			dh.ToolsModes = h.ToolGovernance.Modes()
			dh.DefaultTools = h.ToolGovernance.DefaultPolicy()
			for _, m := range dh.ToolsModes {
				dh.ToolsModeHelp[m] = h.ToolGovernance.ModeHelp(m)
			}
		}
		// AskUserQuestion idle-timeout modes mirror the sandbox block: a
		// Claude-only catalog (never|60s|5m|10m + inherit), hidden for a harness
		// with no AskUserQuestion dialog (Codex).
		dh.AskTimeoutModeHelp = map[string]string{}
		if h.SupportsAskTimeout() {
			dh.AskTimeoutModes = h.AskTimeout.Modes()
			dh.DefaultAskTimeout = h.AskTimeout.DefaultMode()
			for _, m := range dh.AskTimeoutModes {
				dh.AskTimeoutModeHelp[m] = h.AskTimeout.ModeHelp(m)
			}
		} else {
			dh.AskTimeoutModes = []string{}
		}
		out = append(out, dh)
	}
	return out
}

// dashboardLink is the snapshot view of one agent_group_links row.
// Group names are pre-resolved so the renderer doesn't need to do a
// second lookup.
type dashboardLink struct {
	ID            any    `json:"id"`
	Peer          string `json:"peer,omitempty"`
	PeerLabel     string `json:"peer_label,omitempty"`
	RemoteGroup   string `json:"remote_group,omitempty"`
	RemoteGroupID string `json:"remote_group_id,omitempty"`
	Direction     string `json:"direction,omitempty"`
	LocalState    string `json:"local_state,omitempty"`
	RemoteState   string `json:"remote_state,omitempty"`
	RemoteStateAt string `json:"remote_state_at,omitempty"`
	RemoteStale   bool   `json:"remote_stale,omitempty"`
	RequestID     string `json:"request_id,omitempty"`
	Deadline      string `json:"deadline,omitempty"`
	Auto          bool   `json:"auto,omitempty"`
	From          string `json:"from"`
	To            string `json:"to"`
	Mode          string `json:"mode"`
	CreatedAt     string `json:"created_at,omitempty"`
}

// dashboardCronJob is the snapshot view of one agent_cron_jobs row.
// Mirrors jobJSON in cron_handlers.go but adds a few resolved fields
// — owner/target labels and the most-recent run row — so the
// dashboard can render a self-contained table without a second
// fetch per row.
type dashboardCronJob struct {
	ID                         int64    `json:"id"`
	Name                       string   `json:"name"`
	OwnerAgent                 string   `json:"owner_agent,omitempty"`
	OwnerConv                  string   `json:"owner_conv"`
	OwnerLabel                 string   `json:"owner_label"`
	TargetKind                 string   `json:"target_kind"`
	TargetAgent                string   `json:"target_agent,omitempty"`
	TargetConv                 string   `json:"target_conv,omitempty"`
	TargetLabel                string   `json:"target_label,omitempty"`
	GroupID                    int64    `json:"group_id"`
	GroupName                  string   `json:"group_name,omitempty"`
	TargetRole                 string   `json:"target_role,omitempty"`
	IntervalSeconds            int64    `json:"interval_seconds"`
	CronExpr                   string   `json:"cron_expr,omitempty"`
	CronDesc                   string   `json:"cron_desc,omitempty"` // English rendering of CronExpr; best-effort, may be empty
	Subject                    string   `json:"subject,omitempty"`
	Body                       string   `json:"body"`
	ActionKind                 string   `json:"action_kind"`
	SpawnProfile               string   `json:"spawn_profile,omitempty"`
	SpawnRoleRefs              []string `json:"spawn_roles,omitempty"`
	SpawnNameTemplate          string   `json:"spawn_name_template,omitempty"`
	SpawnInstructionTemplate   string   `json:"spawn_instruction_template,omitempty"`
	SpawnConcurrencyPolicy     string   `json:"spawn_concurrency_policy,omitempty"`
	SpawnMaxLiveWorkers        int      `json:"spawn_max_live_workers,omitempty"`
	SpawnWorkerDeadlineSeconds int64    `json:"spawn_worker_deadline_seconds,omitempty"`
	Enabled                    bool     `json:"enabled"`
	RunImmediately             bool     `json:"run_immediately"`
	QueueWhenOffline           bool     `json:"queue_when_offline"`
	OperatorAuthored           bool     `json:"operator_authored,omitempty"`
	OwnerPeer                  string   `json:"owner_peer,omitempty"`
	CreatedAt                  string   `json:"created_at,omitempty"`
	LastRunAt                  string   `json:"last_run_at,omitempty"`
	LastRunStatus              string   `json:"last_run_status,omitempty"`
	NextDueAt                  string   `json:"next_due_at,omitempty"`
}

type snapshotPermissionsView struct {
	Defaults []string            `json:"defaults"`
	Grants   map[string][]string `json:"grants"`
	// Overrides is the full tri-state per-conv view — conv-id → slug →
	// "grant" | "deny" — that the permanent-permission editor reads to
	// pre-populate its modal. Grants (above) is the grant-only
	// projection, kept for the read-only Permissions tab.
	Overrides map[string]map[string]string `json:"overrides"`
	// Scopes carries the winning scope of each per-agent override that has
	// one — conv-id → slug → {dim: [matchers]} — so the editor can render
	// what a grant is narrowed to instead of showing it as unconditional.
	// An unscoped override is simply absent here, which is how a reader
	// tells "no constraint" from "constrained to nothing".
	Scopes map[string]map[string]PermissionScope `json:"scopes,omitempty"`
	// UnreadableScopes names the overrides whose stored scope this build
	// cannot decode — conv-id → slugs. The gate refuses such a row, so the
	// editor must NOT render it as unscoped: that is the widest possible
	// reading of a grant that authorizes nothing, and one Save would make it
	// true. The editor shows it as unreadable and leaves it alone; the write
	// path refuses to overwrite it (see handleDashboardPermissionsAPI).
	UnreadableScopes map[string][]string `json:"unreadable_scopes,omitempty"`
	// ScopeDimOptions is the picker's catalogue: for each dimension the
	// daemon knows, the concrete values an operator can choose and the
	// relational @selectors that dimension accepts. The editor renders
	// whatever arrives and falls back to free text, so a dimension added by
	// a later phase needs no frontend change to become editable.
	ScopeDimOptions map[ScopeDim]snapshotScopeDimOptions `json:"scope_dim_options,omitempty"`
}

// snapshotScopeDimOptions is one dimension's picker catalogue. Values may be
// empty for a dimension whose catalogue is not cheap enough for a 2s poll (or
// does not exist as a list at all) — the picker then takes free text, which is
// the same thing the CLI's --scope accepts.
type snapshotScopeDimOptions struct {
	Values    []string `json:"values,omitempty"`
	Selectors []string `json:"selectors,omitempty"`
}

type dashboardGroup struct {
	ID                         int64                            `json:"id"`
	Name                       string                           `json:"name"`
	Descr                      string                           `json:"descr"`
	AttachmentURL              string                           `json:"attachment_url,omitempty"`
	AttachmentLabel            string                           `json:"attachment_label,omitempty"`
	AttachmentLabelOverride    string                           `json:"attachment_label_override,omitempty"`
	DefaultCwd                 string                           `json:"default_cwd"`                   // pre-fills the spawn form's cwd; "" = none
	DefaultSpawnGroup          bool                             `json:"default_spawn_group,omitempty"` // directory auto-join ambiguity tie-breaker
	DefaultContext             string                           `json:"default_context"`               // shared startup context injected into spawned agents; "" = none
	Environment                []sandboxpolicy.EnvironmentEntry `json:"environment,omitempty"`
	DefaultPrompts             []string                         `json:"default_prompts,omitempty"` // saved prompts composed into this group's spawns when neither spawn nor named profile names any
	DefaultProfile             string                           `json:"default_profile"`           // spawn profile whose launch fields fill blank spawn fields for this group's agents; "" = none (the spawn default's single source — the vestigial default_model was dropped, JOH-220)
	SandboxProfile             string                           `json:"sandbox_profile"`           // filesystem/environment profile assigned to this group; "" = inherit global
	Permissions                []string                         `json:"permissions"`               // live additive grants held by current group members
	PermissionScopes           map[string]PermissionScope       `json:"permission_scopes,omitempty"`
	UnreadablePermissionScopes []string                         `json:"unreadable_permission_scopes,omitempty"`
	// OwnerScopes narrows this group's STRUCTURAL owner-implied permission
	// bypass (TCL-1071): slug → the scope an owner's bypass is confined to
	// here. Always serialized (possibly {}) so the group-permissions editor can
	// round-trip it without distinguishing "absent" from "no narrowing".
	OwnerScopes   map[string]PermissionScope `json:"owner_scopes"`
	MaxMembers    int                        `json:"max_members"`    // hard member cap; 0 = unlimited. A spawn that would exceed it is refused.
	NotifyEnabled bool                       `json:"notify_enabled"` // group OS-notification switch; false mutes every member (per-agent 'on' still overrides)
	// RemoteControlPolicy is the group's remote-control policy that overrides a
	// spawn profile's remote-control default (JOH-262): "inherit" (defer to the
	// profile), "optin" (force Remote Access on) or "deny" (force it off).
	RemoteControlPolicy string `json:"remote_control_policy"`
	// ReinjectAfterCompact is what tclaude re-injects into member agents after
	// a compaction or /clear: "contexts" (default), "identity" or "off".
	ReinjectAfterCompact string `json:"reinject_after_compact"`
	// Mission and SourceTemplate are deployment provenance (JOH-245): what
	// the group was deployed against and the template it was
	// instantiated/deployed from. Both "" for a group not created from a
	// template — the Task Forces framing on the Templates tab renders a group
	// as a deployed force only when SourceTemplate is set.
	Mission        string `json:"mission,omitempty"`
	SourceTemplate string `json:"source_template,omitempty"`
	// Parent is the NAME of the group this one is nested under (n-level
	// groups-in-groups, JOH-392); "" = top-level. Resolved from the row's
	// parent_id against the same snapshot's group set — name-keyed to match
	// how the client's sibling-order and collapse prefs already key. A
	// parent_id that doesn't resolve to a live group (should not happen — the
	// FK is ON DELETE SET NULL) is shipped as "" so the child renders
	// top-level rather than dangling.
	Parent          string `json:"parent,omitempty"`
	RouteGeneration int64  `json:"route_generation"`
	// Process is the group's advisory process state (JOH-242): the current
	// phase, the ordered phase map, and the transition log. nil for a group
	// with no process (the phase chip + advance control render only when set).
	Process *processStateJSON `json:"process,omitempty"`
	// Waves is the group's staged-spawn choreography status (JOH-244): the
	// current wave + how many waves/agents are still pending. nil once the
	// choreography is complete (or for a single-wave deploy) — the "wave N/M
	// pending" chip renders only while set.
	Waves *waveStatusJSON `json:"waves,omitempty"`
	// Scribe marks the group as a daemon-created scribe's eponymous system
	// group (descr == scribeGroupDescr — the circle-scribe machinery, JOH-361).
	// The Groups tab always surfaces these while at least one member is online;
	// dormant scribe groups remain behind the "show offline scribes" toggle.
	// The flag is the discriminator, so the client need not match a name.
	Scribe bool `json:"scribe,omitempty"`
	// FederationLinks lists how this local group is connected to federation
	// peers (group-scoped peer grants, opened remote routes), for the Groups
	// tab's linked-group marker. Local operator data only.
	FederationLinks []groupFederationLink `json:"federation_links,omitempty"`
	Members         []dashboardMember     `json:"members"`
	Online          int                   `json:"online"`
}

type dashboardRouteMap struct {
	Platform       string                             `json:"platform"`
	DarwinSlots    int                                `json:"darwin_slots,omitempty"`
	DarwinBoundary string                             `json:"darwin_boundary,omitempty"`
	DarwinCapacity map[string]dashboardDarwinCapacity `json:"darwin_capacity,omitempty"`
	Routes         []dashboardRoute                   `json:"routes"`
}

type dashboardDarwinCapacity struct {
	Used      int `json:"used"`
	Available int `json:"available"`
	Total     int `json:"total"`
	Pools     int `json:"pools"`
}

type dashboardRoute struct {
	HostOnly          bool                     `json:"host_only,omitempty"`
	ID                string                   `json:"id"`
	GroupID           int64                    `json:"group_id"`
	Group             string                   `json:"group"`
	StableReference   string                   `json:"stable_reference"`
	Name              string                   `json:"name"`
	Transport         string                   `json:"transport"`
	State             string                   `json:"state"`
	CreatedAt         string                   `json:"created_at"`
	GroupGeneration   int64                    `json:"group_generation"`
	GenerationHealth  string                   `json:"generation_health"`
	PublisherAgentID  string                   `json:"publisher_agent_id"`
	PublisherName     string                   `json:"publisher_name"`
	PublisherHealth   string                   `json:"publisher_health"`
	PublisherBoundary string                   `json:"publisher_boundary"`
	Consumers         []dashboardRouteConsumer `json:"consumers"`
}

type dashboardRouteConsumer struct {
	Mode             string `json:"mode,omitempty"`
	ConsumerKind     string `json:"consumer_kind,omitempty"`
	Endpoint         string `json:"endpoint,omitempty"`
	ID               string `json:"id"`
	ConsumerAgentID  string `json:"consumer_agent_id"`
	ConsumerName     string `json:"consumer_name"`
	State            string `json:"state"`
	EndpointState    string `json:"endpoint_state"`
	GenerationHealth string `json:"generation_health"`
	ConsumerHealth   string `json:"consumer_health"`
	Boundary         string `json:"boundary"`
}

// dashboardMember.Owner mirrors the memberJSON convention from
// /v1/groups/{name}/members:
//   - true on a member row → that member is also a group owner
//     (rendered as a badge alongside the role).
//   - true on a row with Role=="owner" and no descr → a pure owner
//     who isn't a member (so the list stays comprehensive).
type dashboardMember struct {
	FederationPresence *db.AgentFederationPresence `json:"federation_presence,omitempty"`
	// AgentID is the member's stable actor key — the canonical, rotation-immune
	// ID the roster leads with; ConvID is the live generation behind it (still
	// the internal drag-and-drop / routing key).
	AgentID string `json:"agent_id,omitempty"`
	ConvID  string `json:"conv_id"`
	Title   string `json:"title"`
	// ClaudeSubscription names the peer operator whose own Claude
	// subscription this agent runs on (claude_subscription.go); nil when it
	// runs on this node's login.
	ClaudeSubscription *memberClaudeSubscription `json:"claude_subscription,omitempty"`
	// CreatedAt is the earliest known existence timestamp: actor birth
	// (agents.created_at) or, for a late-enrolled legacy actor, the older
	// conv_index.Created — see snapshotRowCache.createdFor. Emitted as UTC
	// RFC3339Nano, empty when unknown. Rendered as a relative
	// "Age" column and the default sort key (newest first).
	CreatedAt string `json:"created_at,omitempty"`
	Role      string `json:"role,omitempty"`
	Descr     string `json:"descr,omitempty"`
	// agentLocationView carries `branch` (current branch) plus the
	// startup/current directory split — see agent_location_view.go.
	agentLocationView
	// repoLinksView carries the GitHub web links for the branch cells
	// — dashboard-only enrichment, see branchlinks.go.
	repoLinksView
	// taskRefView carries the per-agent task-reference link (Task
	// column) — see taskref.go.
	taskRefView
	// tagsView carries the per-agent tag set (chips in the Description
	// column) — see tags.go.
	tagsView
	Online bool `json:"online"`
	// ExternalSteward — see dashboardAgent.ExternalSteward.
	ExternalSteward bool `json:"external_steward,omitempty"`
	// SpawnedByExternalSteward — see dashboardAgent.SpawnedByExternalSteward.
	SpawnedByExternalSteward string `json:"spawned_by_external_steward,omitempty"`
	// Waking marks a resume in flight for this conversation: offline on the
	// wire, but seconds from either online or a failure toast. The UI renders
	// it as a pulsing dot + "waking…" pill instead of a dead offline row.
	Waking bool `json:"waking,omitempty"`
	// RouteHealth is a safe capability status for route-enabled groups. It is
	// deliberately a status-only projection; launch generations remain local.
	RouteHealth string     `json:"route_health,omitempty"`
	Owner       bool       `json:"owner,omitempty"`
	State       agentState `json:"state"`
	// Notify is the per-agent override ("on"/"off", "" = inherit);
	// NotifyEffective folds the agent + group levels together (the
	// global switch is separate — snapshot.notifications_enabled).
	Notify          string `json:"notify,omitempty"`
	NotifyEffective bool   `json:"notify_effective"`
}

type dashboardAgent struct {
	// AgentID is the agent's stable actor key — the canonical, rotation-immune
	// ID; ConvID is the live generation behind it (still the internal key).
	AgentID string `json:"agent_id,omitempty"`
	ConvID  string `json:"conv_id"`
	Title   string `json:"title"`
	// agentLocationView carries `branch` (current branch) plus the
	// startup/current directory split — see agent_location_view.go.
	agentLocationView
	// repoLinksView carries the GitHub web links for the branch cells
	// — dashboard-only enrichment, see branchlinks.go.
	repoLinksView
	// taskRefView carries the per-agent task-reference link (Task
	// column) — see taskref.go.
	taskRefView
	// tagsView carries the per-agent tag set (chips in the Description
	// column) — see tags.go.
	tagsView
	Online bool `json:"online"`
	// ExternalSteward marks an assistant running outside tclaude. It has no pane
	// and tclaude can't start it, so it is never online and has no resume.
	ExternalSteward bool `json:"external_steward,omitempty"`
	// SpawnedByExternalSteward names the assistant that started this worker.
	SpawnedByExternalSteward string `json:"spawned_by_external_steward,omitempty"`
	// Waking — see dashboardMember.Waking.
	Waking      bool       `json:"waking,omitempty"`
	State       agentState `json:"state"`
	Groups      []string   `json:"groups"`
	OwnedGroups []string   `json:"owned_groups"` // subset of Groups the agent owns; UI tags these distinctly
	// SpawnGroup is the one of Groups the agent was spawned into, if it is
	// still a member there: the group the Collab view lists it under.
	SpawnGroup string `json:"spawn_group,omitempty"`
	// Effective is what the permission GATE would allow for this agent —
	// computed by the gate's own resolver, so it covers sudo elevations
	// and the structural owner bypass as well as defaults, group grants
	// and per-conv overrides.
	Effective  []string             `json:"effective"`
	ActiveSudo []dashboardSudoEntry `json:"active_sudo,omitempty"` // current sudo grants (slug + id + remaining); empty when none
	// Notify is the per-agent override ("on"/"off", "" = inherit);
	// NotifyEffective folds the agent + group levels together (the
	// global switch is separate — snapshot.notifications_enabled).
	Notify          string `json:"notify,omitempty"`
	NotifyEffective bool   `json:"notify_effective"`
}

// dashboardConversation is the snapshot view of one non-enrolled
// conversation — a promotion candidate in the Agents tab's second
// list. Deliberately leaner than dashboardAgent: a plain conversation
// has no groups, permissions or sudo state to render.
type dashboardConversation struct {
	ConvID string     `json:"conv_id"`
	Title  string     `json:"title"`
	Online bool       `json:"online"`
	State  agentState `json:"state"`
	// Modified is the conv's last-activity RFC3339 stamp, so the
	// dashboard can show "how recent" without a second lookup.
	Modified string `json:"modified,omitempty"`
}

// dashboardRetiredAgent is the snapshot view of one retired agent —
// rendered in the Agents tab's "Retired" section with a reinstate
// button. Carries the retire audit fields so the human can see who
// demoted it and why.
type dashboardRetiredAgent struct {
	// AgentID is the retired actor's stable key — the canonical ID the
	// dashboard/CLI leads with; ConvID is the live generation behind it
	// (kept as the snapshot/hover).
	AgentID   string `json:"agent_id,omitempty"`
	ConvID    string `json:"conv_id"`
	Title     string `json:"title"`
	Online    bool   `json:"online"`
	RetiredAt string `json:"retired_at,omitempty"`
	// RetiredBy is the RAW audit value the retire recorded — a conv-id when an
	// agent performed the retire, or a literal ("human", "system:export-clone").
	// Kept for provenance (the JS surfaces it as the cell's hover title);
	// RetiredByDisplay is what the column actually renders.
	RetiredBy string `json:"retired_by,omitempty"`
	// RetiredByDisplay is the resolved, human-readable retirer (JOH-306): the
	// retirer's current name plus stable short agent_id ("name (agt_xxxxxxxx)"),
	// the bare short agent_id when the name can't be resolved, or the raw
	// RetiredBy literal when there is no stable companion at all.
	RetiredByDisplay string `json:"retired_by_display,omitempty"`
	RetireReason     string `json:"retire_reason,omitempty"`
}

// dashboardReplacedGen is the snapshot view of one REPLACED (predecessor)
// conversation generation — a past conv-id of a still-existing actor, left
// behind when a reincarnate / Claude Code /clear advanced the actor's live
// pointer (JOH-26). Rendered in the Groups tab's default-hidden "Replaced
// generations" virtual group so the operator can see (and prune) the
// generations the actor-level Retired tray no longer lists. Annotated with the
// owning actor so a row points back to the live agent, and with how/when this
// generation was superseded (the reason + timestamp of the generation that
// replaced it). Leaner than a roster row — a predecessor has no live identity
// state of its own.
type dashboardReplacedGen struct {
	ConvID string `json:"conv_id"` // this predecessor generation
	Title  string `json:"title"`   // the predecessor's own title, else the actor's
	// Reason / ReplacedAt describe how + when this generation was SUPERSEDED:
	// the rotation reason ("reincarnate" | "clear" | …) and RFC3339 timestamp
	// of the generation that replaced it.
	Reason     string `json:"reason,omitempty"`
	ReplacedAt string `json:"replaced_at,omitempty"`
	Online     bool   `json:"online"` // ~always false — a predecessor has no live pane
	// ActorConvID / ActorTitle point at the still-live (or retired) actor this
	// generation belongs to, so the row can link back to the current agent.
	// ActorAgentID is the actor's stable key — the canonical ID to link the
	// row back to the live agent (ActorConvID is its current generation,
	// kept as the snapshot/hover). NOTE: the row's own ConvID stays a bare
	// conv-id on purpose — it names this predecessor GENERATION, not an
	// actor (a KEEP-2 case), so it gets no agent_id companion.
	ActorAgentID string `json:"actor_agent_id,omitempty"`
	ActorConvID  string `json:"actor_conv_id"`
	ActorTitle   string `json:"actor_title"`
	ActorRetired bool   `json:"actor_retired,omitempty"` // the owning actor is itself retired
}

// dashboardPending is the snapshot view of one not-yet-enrolled
// dashboard spawn (a pending_spawns row). It carries what the dashboard
// needs to render the pending agent and drive its focus button: its stable
// AgentID, the spawn Label (which is the session-row id AND the focus key — a
// pending agent has no conv-id), its intended group/role/name/descr, whether
// its tmux pane is still alive, and where it is gated (Cwd). Leaner than
// dashboardAgent — a pending spawn has no groups/permissions/sudo to show.
type dashboardPending struct {
	// AgentID is reserved before the harness launches, so the pending row can
	// show the same stable identity as the enrolled-agent table before a
	// harness conversation id exists. Empty only for legacy pending rows.
	AgentID string `json:"agent_id,omitempty"`
	// Label is the spawn label = the session-row id. The focus button
	// keys on THIS (not a conv-id, which does not exist yet).
	Label string `json:"label"`
	// Group is the resolved name of the group the spawn will join (from
	// the row's group_id), or "" for an ungrouped spawn.
	Group string `json:"group,omitempty"`
	Role  string `json:"role,omitempty"`
	Name  string `json:"name,omitempty"`
	Descr string `json:"descr,omitempty"`
	// Online reports whether the spawn's tmux pane is still alive — a
	// pending row whose pane has died (operator closed it, or the spawn
	// crashed) is stale and can no longer be focused to clear the gate.
	Online bool `json:"online"`
	// Cwd is where the agent is gated (from its session row), so the
	// operator sees which untrusted dir to trust. Harness is the spawn's
	// harness ("codex" in practice — Claude Code fires its hook at launch
	// and never lands here). Both empty when no session row exists yet.
	Cwd     string `json:"cwd,omitempty"`
	Harness string `json:"harness,omitempty"`
	// CreatedAt is the RFC3339Nano spawn time (how long it has been
	// pending), used to sort newest-first and show age.
	CreatedAt string `json:"created_at,omitempty"`
}

// dashboardSudoEntry is the wire shape for one active sudo grant in
// the snapshot. Used both as agent[*].active_sudo[] (per-row "this
// agent currently holds these") and as the top-level Sudo[] (full
// list across all agents for the dedicated tab).
type dashboardSudoEntry struct {
	ID               int64  `json:"id"`
	AgentID          string `json:"agent_id,omitempty"` // stable actor key the grant is keyed on
	ConvID           string `json:"conv_id,omitempty"`  // omitted on agent[*].active_sudo (caller already knows)
	ConvTitle        string `json:"conv_title,omitempty"`
	Slug             string `json:"slug"`
	ScopeJSON        string `json:"scope_json,omitempty"`
	Carried          bool   `json:"carried,omitempty"` // also stored for all future Stewards
	UsesLimit        int64  `json:"uses_limit,omitempty"`
	UsesLeft         int64  `json:"uses_left,omitempty"`
	GrantedAt        string `json:"granted_at"`
	ExpiresAt        string `json:"expires_at"`
	GrantedBy        string `json:"granted_by,omitempty"`
	Reason           string `json:"reason,omitempty"`
	RemainingSeconds int64  `json:"remaining_seconds"`
}

// sandboxProfileRef names one tclaude sandbox profile that contributed to a
// launch, with the tier it came from ("global" / "group" / "explicit"). It is
// the display-facing subset of sandboxpolicy.AppliedProfile: the dashboard
// needs to say WHICH profile and WHERE it was assigned, and nothing else in
// that record (row id, mtime) belongs in a snapshot served to the browser.
type sandboxProfileRef struct {
	Scope string `json:"scope"`
	Name  string `json:"name"`
}

// sandboxProfileRefs projects a launch's frozen sandbox snapshot onto the
// display refs above, in the order the snapshot recorded them. That order is
// global → group → explicit, an invariant of the snapshot BUILDER
// (db.resolveEffectiveSandboxSnapshot walks the tiers in that sequence) — this
// function neither re-sorts nor relies on anything else to.
//
// A nil snapshot — a launch older than the column, or one that recorded none —
// yields nil, which the caller distinguishes from "resolved to no profiles"
// via the recorded flag. An entry with a blank name is skipped because there is
// nothing to display for it; that cannot silently turn into a false "no profile
// applied", because names are validated non-empty at profile creation.
func sandboxProfileRefs(snapshot *sandboxpolicy.Snapshot) []sandboxProfileRef {
	if snapshot == nil || len(snapshot.Applied) == 0 {
		return nil
	}
	out := make([]sandboxProfileRef, 0, len(snapshot.Applied))
	for _, applied := range snapshot.Applied {
		name := strings.TrimSpace(applied.Name)
		if name == "" {
			continue
		}
		out = append(out, sandboxProfileRef{Scope: string(applied.Scope), Name: name})
	}
	return out
}
