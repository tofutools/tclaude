package agentd

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// The snapshot's collector steps: the independent list and registry reads,
// the PR footer lists, the daemon's in-process state, and the static-registry
// elision. handleDashboardSnapshot calls them after the roster is assembled.

// snapshotCollected is what the snapshot's collectors read: the lists and
// registries that do not depend on the roster, plus the usage collector's
// own sub-phases and the cost-visibility inputs.
type snapshotCollected struct {
	pending          []dashboardPending
	cron             []dashboardCronJob
	exportJobsActive int
	links            []dashboardLink
	usage            dashboardUsage
	hasRealCost      bool
	costErr          error
	templates        []templateJSON
	profiles         []spawnProfileJSON
	defaultProfile   *db.SpawnProfile
	roles            []roleJSON
	savedPrompts     []db.SavedPrompt
	queuedSpawns     []queuedSpawnJSON
	capacityQueue    *spawnQueueListJSON
	messages         []dashboardHumanMessage
	messagesUnread   int
	plugins          []dashboardPlugin
	pluginsWarn      int
	pluginsErr       error
	usagePhases      []perfPhase
	openCodeActivity bool
}

// runSnapshotCollectors runs them as one fan-out under span's "collectors"
// phase, with the usage collector's own reads nested under its run.
func runSnapshotCollectors(span *perfSpan, cfg *config.Config, aliveSessions map[string]struct{},
	groupNames map[int64]string) snapshotCollected {
	var c snapshotCollected
	// These collectors do not depend on each other. Most are one or two small
	// SQLite reads; serial execution made their fixed per-query overhead add up
	// to >10 ms even on tiny rosters.
	collectorPhases := runSnapshotNamedLoads(
		snapshotNamedLoad{"pending", func() { c.pending = collectPendingSnapshot(aliveSessions, groupNames) }},
		snapshotNamedLoad{"cron", func() { c.cron = collectCronSnapshot() }},
		snapshotNamedLoad{"export_jobs", func() { c.exportJobsActive, _ = db.CountActiveExportJobs() }},
		snapshotNamedLoad{"links", func() { c.links = collectLinksSnapshot(groupNames) }},
		snapshotNamedLoad{"usage", func() {
			showWhatIf := cfg != nil && cfg.Cost != nil && cfg.Cost.ShowOnSubscription
			c.usage, c.hasRealCost, c.usagePhases, c.costErr = collectUsageSnapshot(cfg.ResolvedUsageIdleTimeout(), showWhatIf, cfg)
		}},
		snapshotNamedLoad{"opencode_usage_activity", func() {
			c.openCodeActivity, _ = db.HasOpenCodeUsageActivitySince(time.Now().Add(-db.OpenCodeUsageActivityRetention))
		}},
		snapshotNamedLoad{"templates", func() { c.templates = collectTemplatesSnapshot() }},
		snapshotNamedLoad{"profiles", func() { c.profiles = collectProfilesSnapshot() }},
		snapshotNamedLoad{"default_profile", func() { c.defaultProfile = globalDefaultProfile() }},
		snapshotNamedLoad{"roles", func() { c.roles = collectRolesSnapshot() }},
		snapshotNamedLoad{"saved_prompts", func() {
			if c.savedPrompts, _ = db.ListSavedPrompts(); c.savedPrompts == nil {
				c.savedPrompts = []db.SavedPrompt{}
			}
		}},
		snapshotNamedLoad{"queued_spawns", func() { c.queuedSpawns = collectQueuedSpawnsSnapshot() }},
		snapshotNamedLoad{"capacity_queue", func() { c.capacityQueue = collectCapacityQueueSnapshot() }},
		snapshotNamedLoad{"messages", func() { c.messages, c.messagesUnread = buildHumanMessagesSnapshot() }},
		snapshotNamedLoad{"plugins", func() { c.plugins, c.pluginsWarn, c.pluginsErr = collectPluginsSnapshot() }},
	)
	for i := range collectorPhases {
		if collectorPhases[i].Name == "usage" {
			for j := range collectorPhases[i].Children {
				if collectorPhases[i].Children[j].Name == "run" {
					collectorPhases[i].Children[j].Children = c.usagePhases
					break
				}
			}
			break
		}
	}
	span.addChildren("collectors", collectorPhases...)
	return c
}

// attachSnapshotCollected writes every collector result into the payload,
// with the tab-visibility rules that read them (Costs, Usage, Plugins).
func attachSnapshotCollected(out *snapshotPayload, c snapshotCollected, p snapshotPreload) {
	out.Pending = c.pending
	out.Cron = c.cron
	// Badge-only: the Jobs tab's row window comes from /api/jobs, not the
	// snapshot. A count read error degrades to 0 (no badge), never a failed poll.
	out.ExportJobsActive = c.exportJobsActive
	out.Links = c.links
	out.Usage = c.usage
	// Costs-tab visibility: show when there is real pay-per-token spend to
	// display, OR a subscription account has opted into the WHAT-IF view
	// (cost.show_on_subscription). A subscription-only account with the opt-in
	// off hides the tab — it would only show an empty chart. The opt-in also
	// reveals per-agent hypothetical badges. /api/costs always selects real
	// versus WHAT-IF per row and reports kind metadata; the front-end renders a
	// warning whenever hypothetical values contribute. A DB error
	// degrades to "no real cost" (the opt-in still governs), matching how the
	// cost figures themselves degrade to 0.
	if c.costErr != nil {
		slog.Debug("snapshot: cost visibility read failed; treating as no real cost", "error", c.costErr)
	}
	showOnSub := p.cfg != nil && p.cfg.Cost != nil && p.cfg.Cost.ShowOnSubscription
	out.CostTabVisible = c.hasRealCost || showOnSub
	out.CostTabWhatIf = showOnSub
	out.UsageTabVisible = c.usage.historyAvailable || c.openCodeActivity
	out.Templates = c.templates
	out.Profiles = c.profiles
	var httpProxyNames []string
	if p.cfg != nil && p.cfg.Agent != nil {
		for name := range p.cfg.Agent.HTTPProxies {
			httpProxyNames = append(httpProxyNames, name)
		}
	}
	out.Permissions.ScopeDimOptions = scopeDimOptionsSnapshot(
		p.groups, c.profiles, p.sandboxProfiles,
		p.cfg.ResolvedLinearProxy().AllowedTeams, p.cfg.ResolvedAWBProxy().AllowedWorkspaces, httpProxyNames)
	if c.defaultProfile != nil {
		out.SpawnProfileDefault = c.defaultProfile.Name
	}
	out.Roles = c.roles
	out.SavedPrompts = c.savedPrompts
	out.QueuedSpawns = c.queuedSpawns
	out.CapacityQueue = c.capacityQueue
	out.Messages, out.MessagesUnread = c.messages, c.messagesUnread
	out.Plugins, out.PluginsWarn = c.plugins, c.pluginsWarn
	if c.pluginsErr != nil {
		out.PluginsError = c.pluginsErr.Error()
	}
	// Plugins-tab visibility mirrors the Costs-tab rule: show it when there's
	// something to manage (≥1 installed plugin), when plugins.json is broken
	// (so the human can see + fix the error), or when they've opted to always
	// keep it (config dashboard.always_show_plugins_tab — the escape hatch to
	// the install-from-catalog UI). Otherwise hide the empty tab.
	out.PluginsTabVisible = len(out.Plugins) > 0 || out.PluginsError != "" || p.cfg.ShowPluginsTabAlways()
}

// attachSnapshotPRLists builds the footer's authored-PR, authored-MR and
// tracked-author lists, each reconciled with the snapshot's fresh PR state,
// checks and reviews and associated with the agent rows.
func attachSnapshotPRLists(out *snapshotPayload, prs snapshotPRIndex, cfg *config.Config) {
	out.AuthoredOpenPRs = associateAuthoredOpenPRs(
		withAuthoredPRReviews(withoutArchivedPRs(unionLocallyKnownOpenPRs(
			reconcileAuthoredOpenPRs(prs.authoredOpenPRs, prs.freshPRStates, prs.prChecks, time.Now()),
			prs.localPRs,
			prs.freshPRStates,
			prs.prChecks,
		)), prs.prReviews),
		out.Agents,
	)
	out.AuthoredOpenMRs = authoredOpenMRsSnapshot(cfg, out.Agents)
	for _, view := range prs.trackedPRAuthors {
		out.TrackedPRAuthors = append(out.TrackedPRAuthors, associateAuthoredOpenPRs(
			withAuthoredPRReviews(withoutArchivedPRs(
				reconcileAuthoredOpenPRs(view, prs.freshPRStates, prs.prChecks, time.Now())), prs.prReviews),
			out.Agents,
		))
	}
}

// attachSnapshotLiveState writes the daemon's in-process state: broker
// refusal counts, the steward's elevation, access and seat, the knowledge
// source, companion cues, access requests and the plugin catalog.
func attachSnapshotLiveState(out *snapshotPayload, cfg *config.Config) {
	out.BrokerRefusalsTotal, out.BrokerRefusalsUnplaceable = brokerRefusals.counts()
	stewardState := permissionsState{
		Defaults:  out.Permissions.Defaults,
		Grants:    out.Permissions.Grants,
		Overrides: out.Permissions.Overrides,
	}
	out.StewardElevated = stewardElevations(cfg, stewardState)
	out.StewardAccess = stewardAccessView(cfg, stewardState)
	out.StewardRoster = stewardRosterView()
	if len(out.StewardRoster) > 0 && out.StewardRoster[0].Seat == db.StewardSeatActive {
		out.StewardActive = out.StewardRoster[0].AgentID
	}
	source := newKnowledgeSourceView(cfg)
	out.KnowledgeSource = &source
	out.CompanionCues = activeCompanionCues()
	out.AccessRequests = approvals.dashboardSnapshot()
	// Only the actionable (pending) ones drive the blink/banner/badge — the
	// list also carries recently-handled history, which must not count.
	out.AccessRequestsPending = approvals.pendingCount()
	out.PluginsCatalog = pluginCatalog()
}

// The large registries below change only after an explicit management
// action (or a binary upgrade for the built-in registries). Hash them into
// the dynamic snapshot and omit their contents when the browser proves it
// already has this exact version. We still collect them before hashing so a
// mutation is visible on the very next 2-second tick.
func elideUnchangedStatics(out *snapshotPayload, r *http.Request) {
	out.StaticVersion = snapshotStaticVersion(*out)
	if r.URL.Query().Get("static_version") == out.StaticVersion {
		out.StaticUnchanged = true
		out.Slugs = nil
		out.Templates = nil
		out.Profiles = nil
		out.Roles = nil
		out.SavedPrompts = nil
		out.PluginsCatalog = nil
		// Page JS comes from the viewing node and this snapshot may go to a peer's
		// older page, which restores only the fields above. A page that restores
		// these too says so.
		if r.URL.Query().Get("static_catalogs") == "1" {
			out.Harnesses = nil
			out.SandboxImpl = dashboardSandboxImpl{}
			out.Permissions.ScopeDimOptions = nil
		}
	}
}
