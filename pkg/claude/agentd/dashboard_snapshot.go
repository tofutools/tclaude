package agentd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/cronexpr"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/remoteaccess"
	"github.com/tofutools/tclaude/pkg/common/buildversion"
)

// handleDashboardSnapshot returns one JSON blob covering everything
// the page renders: groups + members, all known agents, the live
// permission state (defaults + per-conv grants), and the slug
// registry. Read-only.
const snapshotLoadConcurrency = 8

var snapshotLoadSlots = make(chan struct{}, snapshotLoadConcurrency)

func runSnapshotLoads(loads ...func()) {
	named := make([]snapshotNamedLoad, len(loads))
	for i, load := range loads {
		named[i].run = load
	}
	runSnapshotNamedLoads(named...)
}

type snapshotNamedLoad struct {
	name string
	run  func()
}

// runSnapshotNamedLoads is the instrumented form of runSnapshotLoads. It
// returns measurements in declaration order even though the work runs in
// parallel, which keeps the nested Debug table stable between polls. Each
// child's total includes time waiting for the daemon-wide load slot; queue and
// execution are nested separately so contention cannot disappear into the
// preload parent's otherwise-unexplained wall time.
func runSnapshotNamedLoads(loads ...snapshotNamedLoad) []perfPhase {
	if len(loads) == 0 {
		return nil
	}
	timings := make([]perfPhase, len(loads))
	queuedAt := make([]time.Time, len(loads))
	jobs := make(chan int, len(loads))
	for i := range loads {
		if loads[i].name != "" {
			queuedAt[i] = time.Now()
		}
		jobs <- i
	}
	close(jobs)
	workers := min(len(loads), snapshotLoadConcurrency)
	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			for index := range jobs {
				snapshotLoadSlots <- struct{}{}
				if loads[index].name == "" {
					loads[index].run()
					<-snapshotLoadSlots
					continue
				}
				func() {
					defer func() { <-snapshotLoadSlots }()
					startedAt := time.Now()
					loads[index].run()
					finishedAt := time.Now()
					timings[index] = perfPhase{
						Name: loads[index].name,
						Ms:   durMs(finishedAt.Sub(queuedAt[index])),
						Children: []perfPhase{
							{Name: "queue", Ms: durMs(startedAt.Sub(queuedAt[index]))},
							{Name: "run", Ms: durMs(finishedAt.Sub(startedAt))},
						},
					}
				}()
			}
		}()
	}
	wg.Wait()
	phases := make([]perfPhase, 0, len(loads))
	for i := range loads {
		if timings[i].Name != "" {
			phases = append(phases, timings[i])
		}
	}
	return phases
}

func handleDashboardSnapshot(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	// A popped-out board polls too but has no presence loop, so it is no sign of
	// a dashboard that capture could reach.
	if !isPeerSnapshot(r) && r.URL.Query().Get("standalone") == "" {
		noteDashboardPoll()
	}
	// This response is dynamic even when static_version remains unchanged: the
	// query parameter only lets us omit stable registry blobs, while agent
	// liveness and status still change on every poll. Never let a browser or
	// intermediary reuse an older response for the same versioned URL.
	w.Header().Set("Cache-Control", "no-store")
	// Wall-clock phase marks land in the /api/perf ring (perf.go,
	// TCL-374). Nil-safe: a direct call outside withPerfTiming (tests)
	// simply records nothing.
	span := perfSpanFrom(r)
	// All status consumers share this gather, including its one tmux probe.
	// Warm reads only project cached data; authority is always checked below.
	sharedStatus, ok := requestStatusSnapshot(w, asDashboardHumanPeer(r), func(phases []perfPhase) { span.addChildren("status_snapshot", phases...) })
	if !ok {
		return
	}
	aliveSessions := sharedStatus.alive
	span.mark("status_snapshot")

	var (
		groups               []*db.AgentGroup
		sandboxProfiles      []*db.SandboxProfile
		globalSandboxProfile *db.SandboxProfile
		allGrants            map[string][]string
		allOverrides         map[string]map[string]string
		allOverrideScopes    map[string]map[string]string
		cfg                  *config.Config
		notifyPrefs          map[string]string
		activeConvIDs        []string
		retiredTotal         int
		agentRosterErr       error
		sudoGrants           []*db.SudoGrant
	)
	// The snapshot preload is a set of independent SQLite WAL reads plus one
	// config-file read. Run them as one fan-out so its wall clock is the slowest
	// read, not the sum of ~dozen fixed round trips (which dominated even with
	// only a handful of agents).
	span.addChildren("preload", runSnapshotNamedLoads(
		snapshotNamedLoad{"groups", func() { groups, _ = db.ListAgentGroups() }},
		snapshotNamedLoad{"sandbox_profiles", func() { sandboxProfiles, _ = db.ListSandboxProfiles() }},
		snapshotNamedLoad{"global_sandbox", func() { globalSandboxProfile, _ = db.GetGlobalSandboxProfile() }},
		snapshotNamedLoad{"permission_overrides", func() { allOverrides, _ = db.ListAllAgentPermissionOverrides() }},
		snapshotNamedLoad{"permission_scopes", func() { allOverrideScopes, _ = db.ListAllAgentPermissionScopes() }},
		snapshotNamedLoad{"config", func() { cfg, _ = config.Load() }},
		snapshotNamedLoad{"notify_prefs", func() { notifyPrefs, _ = db.ListConvNotifyPrefs() }},
		snapshotNamedLoad{"agent_roster", func() {
			activeConvIDs, retiredTotal, agentRosterErr = db.ListAgentRosterState()
		}},
		snapshotNamedLoad{"sudo_grants", func() { sudoGrants, _ = db.ListAllActiveSudoGrants() }},
	)...)
	// The all-overrides read contains both grant and deny effects. Derive the
	// grant-only compatibility view in memory instead of scanning and sorting
	// the same permission table a second time on every poll.
	allGrants = map[string][]string{}
	for convID, effects := range allOverrides {
		for slug, effect := range effects {
			if effect == db.PermEffectGrant {
				allGrants[convID] = append(allGrants[convID], slug)
			}
		}
		sort.Strings(allGrants[convID])
	}
	defaults := []string{}
	if cfg != nil && cfg.Agent != nil {
		defaults = append(defaults, cfg.Agent.DefaultPermissions...)
	}
	sort.Strings(defaults)
	groupsRouteMapEnabled := cfg.GroupsRouteMapEnabled()

	// agentRows: union of (every group member) + (every conv-id with
	// explicit grants). Keyed by conv-id so members appearing in
	// multiple groups dedupe naturally.

	// Load each group's members + owners ONCE. ListAgentGroupMembers used to
	// run twice per group (the muted-notify pass below + the main group loop),
	// and ListAgentGroupOwners once in the group loop; keying both by group id
	// here lets inMutedGroup, the conv-set collect and the group loop all reuse
	// a single read.
	groupIDs := make([]int64, len(groups))
	for i, group := range groups {
		groupIDs[i] = group.ID
	}
	var (
		membersByGroup        map[int64][]*db.AgentGroupMember
		ownersByGroup         map[int64][]*db.AgentGroupOwner
		routesByGroup         map[int64][]*db.AgentRoute
		leasesByGroup         map[int64][]*db.AgentRouteLease
		darwinLaunchesByGroup map[int64][]*db.DarwinRouteLaunch
		membersErr            error
		ownersErr             error
		routesErr             error
		leasesErr             error
		darwinLaunchesErr     error
	)
	span.addChildren("preload", runSnapshotNamedLoads(
		snapshotNamedLoad{"group_members", func() { membersByGroup, membersErr = db.ListAgentGroupMembersBatch(groupIDs) }},
		snapshotNamedLoad{"group_owners", func() { ownersByGroup, ownersErr = db.ListAgentGroupOwnersBatch(groupIDs) }},
		snapshotNamedLoad{"routes", func() {
			if groupsRouteMapEnabled {
				routesByGroup, routesErr = db.ListAgentRoutesBatch(groupIDs)
			}
		}},
		snapshotNamedLoad{"route_leases", func() {
			if groupsRouteMapEnabled {
				leasesByGroup, leasesErr = db.ListAgentRouteLeasesBatch(groupIDs)
			}
		}},
		snapshotNamedLoad{"darwin_route_launches", func() {
			if groupsRouteMapEnabled && dashboardRouteMapPlatform == "darwin" {
				darwinLaunchesByGroup, darwinLaunchesErr = db.ListActiveDarwinRouteLaunchesByGroup(groupIDs)
			}
		}},
	)...)
	if membersErr != nil {
		slog.Warn("snapshot: failed to preload group members", "error", membersErr)
	}
	if ownersErr != nil {
		slog.Warn("snapshot: failed to preload group owners", "error", ownersErr)
	}
	if routesErr != nil {
		slog.Warn("snapshot: failed to preload group routes", "error", routesErr)
	}
	if leasesErr != nil {
		slog.Warn("snapshot: failed to preload route leases", "error", leasesErr)
	}
	if darwinLaunchesErr != nil {
		slog.Warn("snapshot: failed to preload Darwin route launch contracts", "error", darwinLaunchesErr)
	}

	// Notification-filter state: the per-agent overrides plus the set
	// of convs sitting in at least one muted (non-archived) group.
	// notifyEffective mirrors notify.AllowedForConv — agent pref wins,
	// else any muted group silences — so the bells the dashboard
	// renders agree with what the notify path will actually do.
	inMutedGroup := map[string]bool{}
	for _, g := range groups {
		if g.IsArchived() || g.NotifyEnabled {
			continue
		}
		for _, m := range membersByGroup[g.ID] {
			inMutedGroup[m.ConvID] = true
		}
	}
	notifyEffective := func(convID string) bool {
		switch notifyPrefs[convID] {
		case db.NotifyPrefOn:
			return true
		case db.NotifyPrefOff:
			return false
		}
		return !inMutedGroup[convID]
	}

	// Every active ACTOR — the canonical roster (JOH-26 PR3b reads the
	// agent-level roster, one row per live actor at its current conv). Unlike
	// the old "online ungrouped session" probe, this includes OFFLINE agents: a
	// conv that was an agent yesterday keeps showing after its tmux pane closed,
	// instead of silently vanishing. Plain conversations that were never
	// promoted are not here — they surface in out.Conversations.
	// Active sudo grants across every agent were loaded in the initial fan-out;
	// they are bucketed per conv-id below for O(1) per-agent rendering.

	// Collect the full conv set the snapshot will render — group members ∪
	// owners ∪ grant/override holders ∪ active agents ∪ sudo holders — then
	// batch-load every per-conv table once (TCL-368/TCL-367). rc.viewFor(convID)
	// returns a memoized row bundle so the member loop, addAgent and the owners
	// pass share ONE computation instead of ~13 point queries each, and the
	// location resolves from the batch without a per-poll .jsonl rescan.
	convSetStart := time.Now()
	convSet := map[string]struct{}{}
	addConv := func(convID string) {
		if convID != "" {
			convSet[convID] = struct{}{}
		}
	}
	for _, members := range membersByGroup {
		for _, m := range members {
			addConv(m.ConvID)
		}
	}
	for _, owners := range ownersByGroup {
		for _, o := range owners {
			addConv(o.ConvID)
		}
	}
	for convID := range allGrants {
		addConv(convID)
	}
	for convID := range allOverrides {
		addConv(convID)
	}
	for _, convID := range activeConvIDs {
		addConv(convID)
	}
	for _, g := range sudoGrants {
		addConv(g.ConvID)
	}
	convIDs := make([]string, 0, len(convSet))
	for convID := range convSet {
		convIDs = append(convIDs, convID)
	}
	span.addChildDuration("preload", "conv_set", time.Since(convSetStart))
	var (
		taskRefs          map[string]db.AgentTaskRef
		presentedPRs      map[string][]db.AgentPR
		cachedPRStates    prStateIndex
		cachedPRTitles    map[string]string
		allTags           map[string][]string
		authoredOpenPRs   dashboardAuthoredOpenPRs
		trackedPRAuthors  []dashboardAuthoredOpenPRs
		branchPRCacheURLs []string
	)
	rc := newSnapshotRowCache(convIDs, sharedStatus, func(phases []perfPhase) {
		span.addChildren("preload", phases...)
	})
	branchPRCacheURLs = make([]string, 0, len(convIDs)*3)
	for _, convID := range convIDs {
		links := rc.viewFor(convID).Links
		// The statusline PR rides along so its badge gets the same cached
		// checks, reviews, title and background refresh as a slot PR.
		branchPRCacheURLs = append(branchPRCacheURLs, links.BranchPRURL, links.StartupPRURL, links.statuslinePR.URL)
	}
	visibleAgentIDs := make([]string, 0, len(rc.agents))
	visibleAgentSet := make(map[string]struct{}, len(rc.agents))
	for _, actor := range rc.agents {
		if actor.AgentID == "" {
			continue
		}
		if _, seen := visibleAgentSet[actor.AgentID]; seen {
			continue
		}
		visibleAgentSet[actor.AgentID] = struct{}{}
		visibleAgentIDs = append(visibleAgentIDs, actor.AgentID)
	}
	span.addChildren("preload", runSnapshotNamedLoads(
		snapshotNamedLoad{"task_refs", func() {
			taskRefs, _ = db.ListAgentTaskRefsByAgentIDs(visibleAgentIDs)
		}},
		snapshotNamedLoad{"presented_prs", func() { presentedPRs = preloadPresentedPRsForDashboard(time.Now()) }},
		snapshotNamedLoad{"tags", func() { allTags, _ = db.ListAllAgentTags() }},
		snapshotNamedLoad{"authored_open_prs", func() { authoredOpenPRs = loadAuthoredOpenPRsSnapshot(cfg) }},
		snapshotNamedLoad{"tracked_pr_authors", func() { trackedPRAuthors = loadTrackedPRAuthorsSnapshot(cfg) }},
	)...)
	// Every PR visualization draws state and checks from these two canonical
	// identity-keyed caches. Build the union first so branch/startup,
	// explicitly presented and footer-only PRs share one bounded batch read.
	allPRCacheURLs := append([]string(nil), branchPRCacheURLs...)
	for _, rows := range presentedPRs {
		for _, row := range rows {
			allPRCacheURLs = append(allPRCacheURLs, row.PRURL)
		}
	}
	localPRs := make([]locallyKnownPR, 0, len(allPRCacheURLs))
	for _, rawURL := range branchPRCacheURLs {
		localPRs = append(localPRs, locallyKnownPR{URL: rawURL})
	}
	for _, rows := range presentedPRs {
		for _, row := range rows {
			localPRs = append(localPRs, locallyKnownPR{URL: row.PRURL})
		}
	}
	for _, list := range [][]dashboardAuthoredOpenPR{authoredOpenPRs.Items, authoredOpenPRs.Recent} {
		for _, pr := range list {
			allPRCacheURLs = append(allPRCacheURLs, pr.URL)
		}
	}
	for _, view := range trackedPRAuthors {
		for _, list := range [][]dashboardAuthoredOpenPR{view.Items, view.Recent} {
			for _, pr := range list {
				allPRCacheURLs = append(allPRCacheURLs, pr.URL)
			}
		}
	}
	span.addChildren("preload", runSnapshotNamedLoads(
		snapshotNamedLoad{"pr_state_cache", func() { cachedPRStates, cachedPRTitles = cachedPresentedPRMetadata(allPRCacheURLs) }},
	)...)

	indexedPRs := make(map[string]bool, len(authoredOpenPRs.Items)+len(authoredOpenPRs.Recent))
	for _, list := range [][]dashboardAuthoredOpenPR{authoredOpenPRs.Items, authoredOpenPRs.Recent} {
		for _, pr := range list {
			indexedPRs[prStateKey(pr.URL)] = true
		}
	}
	for i := range localPRs {
		key := prStateKey(localPRs[i].URL)
		localPRs[i].Title = cachedPRTitles[key]
		// Local discoveries may not be indexed by the authored search yet,
		// or may belong to another author. Reuse the background metadata
		// refresh; snapshot rendering itself never waits for GitHub.
		if authoredOpenPRs.Available && strings.HasPrefix(key, "github:") &&
			!indexedPRs[key] && !presentedPRTitleCacheFresh(localPRs[i].URL, time.Now()) {
			schedulePresentedPRRefresh("", localPRs[i].URL)
		}
		indexedPRs[key] = true
	}

	externalStewardParents := externalStewardChildNames()
	taskRefFor := func(agentID string) taskRefView {
		return taskRefViewFor(taskRefs[agentID])
	}
	presentedPRsFor := func(agentID string) []presentedPRView {
		return presentedPRViews(presentedPRs[agentID])
	}
	// Resolve one freshest state per PR URL across every dashboard source.
	// This deliberately crosses agent rows: a presented-PR poll on one row and
	// an automatic branch link on another are observations of the same PR, not
	// independent display state.
	freshPRStates := make(prStateIndex)
	addAuthoredOpenPRStates(freshPRStates, authoredOpenPRs)
	for _, convID := range convIDs {
		freshPRStates.addRepoLinks(rc.viewFor(convID).Links)
	}
	for key, candidate := range cachedPRStates {
		current, ok := freshPRStates[key]
		if !ok {
			freshPRStates[key] = candidate
			continue
		}
		state, updatedAt := newestPRState(
			current.state, current.updatedAt, candidate.state, candidate.updatedAt)
		freshPRStates[key] = prStateObservation{state: state, updatedAt: updatedAt}
	}
	for _, rows := range presentedPRs {
		for _, row := range rows {
			freshPRStates.add(row.PRURL, row.State, row.UpdatedAt)
		}
	}
	// CI check summaries for every PR badge this snapshot will draw, in one
	// batched cache read. Nothing here shells out: the blobs are written by
	// the branch-link / presented-PR refreshes that already run `gh`, and by
	// the hover endpoint. Presented PR URLs join the branch/startup ones so a
	// presented badge gets the same indicator as an automatic one.
	prChecks := prChecksIndexFor(allPRCacheURLs)
	// Review status rides the same identity-keyed batch discipline; see
	// prreviews.go. Written only by GitHub reads that already happen.
	prReviews := prReviewsIndexFor(allPRCacheURLs)
	// Same preload discipline as taskRefs: one ListAllAgentTags per snapshot,
	// keyed by agent_id, looked up per row (not a query per member/agent in
	// this 2s-polled path). The stored set is already sorted alphabetically.
	tagsFor := func(agentID string) tagsView {
		return tagsView{Tags: allTags[agentID]}
	}
	// The preload warm loop above resolves EVERY conv the snapshot will render,
	// so it — not the group loop — is where the un-batched per-row side reads
	// are actually paid. Report them here, and exclude the Codex telemetry they
	// dragged in so preload stays comparable to the top-level codex_telemetry
	// metric it is reported beside. Before this, preload double-counted that
	// telemetry while the groups phase (which never pays it) subtracted the
	// whole cumulative total and clamped toward zero.
	span.addChildren("preload", rc.rowWorkPhasesSince(nil)...)
	codexAfterPreload := rc.codexTelemetryTiming.total
	span.markExcluding("preload", codexAfterPreload)
	payloadStart := time.Now()
	groupNames := make(map[int64]string, len(groups))
	for _, group := range groups {
		groupNames[group.ID] = group.Name
	}

	agentRows := map[string]*dashboardAgent{}
	addAgent := func(convID string) *dashboardAgent {
		if existing, ok := agentRows[convID]; ok {
			return existing
		}
		// TCL-369: a retired/superseded conv never becomes an agent row, so
		// skip its resolution entirely instead of building it and dropping it
		// in the output loop. Callers that use the return value nil-check it.
		if rc.inactiveActor(convID) {
			return nil
		}
		b := rc.viewFor(convID)
		links := b.Links.withPresentedPRs(presentedPRsFor(b.AgentID)).
			withFreshestPRStates(freshPRStates).
			withPRChecks(prChecks).
			withPRTitles(cachedPRTitles).
			withPRReviews(prReviews)
		a := &dashboardAgent{
			AgentID:           b.AgentID,
			ConvID:            convID,
			Title:             b.Title,
			agentLocationView: b.Loc,
			repoLinksView:     links,
			taskRefView:       taskRefFor(b.AgentID),
			tagsView:          tagsFor(b.AgentID),
			Online:            b.Online,
			Waking:            !b.Online && isConvWaking(convID),
			State:             b.State,
			ExternalSteward:   db.IsExternalStewardConv(convID),

			SpawnedByExternalSteward: externalStewardParents[b.AgentID],
			// init non-nil so JSON serializes [] not null;
			// the dashboard's JS does .length / .map without a guard.
			Groups:          []string{},
			OwnedGroups:     []string{},
			Effective:       []string{},
			Notify:          notifyPrefs[convID],
			NotifyEffective: notifyEffective(convID),
		}
		agentRows[convID] = a
		return a
	}

	// Host-catalog reads, measured. These look like plain struct fields, but
	// two of them reach the host:
	//
	//   - buildSandboxImplCatalog FORKS bwrap to probe namespace availability
	//     (twice — interactive and server boundary — plus a pidfd probe)
	//     whenever its 60 s disclosure cache expires. At a 2 s poll that is one poll in thirty paying a
	//     multi-fork bill: exactly the shape of an occasional
	//     multi-hundred-ms spike in an otherwise ~13 ms request.
	//   - buildHarnessCatalog used to cost ~500 ms whenever OpenCode's model
	//     catalog went stale, because building it EXECUTED `opencode models`.
	//     That read is now served from cache and refreshed in the background,
	//     so this should stay in the microseconds; if it ever does not, a
	//     harness descriptor has started doing work on the read path again.
	//
	// Both sat inside the "groups" window unmeasured, so their cost was charged
	// to a group loop whose own sub-phases never exceed a fraction of a
	// millisecond.
	var payloadPhases []perfPhase
	payloadMeasured := time.Duration(0)
	payloadPhase := func(name string, fn func()) {
		start := time.Now()
		fn()
		d := time.Since(start)
		payloadMeasured += d
		payloadPhases = append(payloadPhases, perfPhase{Name: name, Ms: durMs(d)})
	}
	var (
		authSession dashboardAuthSession
		userModel   string
		harnesses   []dashboardHarness
		sandboxImpl dashboardSandboxImpl
		raRunning   bool
		raBind      string
		raMaterial  bool
	)
	payloadPhase("auth_session", func() { authSession = dashboardAuthSessionForRequest(r) })
	payloadPhase("user_model", func() { userModel = readUserDefaultModel() })
	payloadPhase("harness_catalog", func() { harnesses = buildHarnessCatalog() })
	payloadPhase("sandbox_impl", func() { sandboxImpl = buildSandboxImplCatalog() })
	payloadPhase("remote_access", func() {
		raRunning, raBind = remoteListenerStatus()
		raMaterial = remoteaccess.Exists()
	})
	terminalAttachMode, terminalInitialDelay, terminalRepairDelay, terminalPreAttachDelay := cfg.ResolvedTerminalAttach()
	out := snapshotPayload{
		GeneratedAt:          time.Now().Format(time.RFC3339),
		Version:              buildversion.AppVersion(),
		AssetsVersion:        dashboardAssetsVersion,
		AuthSession:          authSession,
		PopupBase:            popupBaseURL,
		UserDefaultModel:     userModel,
		Harnesses:            harnesses,
		SandboxImpl:          sandboxImpl,
		NotificationsEnabled: cfg != nil && cfg.Notifications != nil && cfg.Notifications.Enabled,
		SpawnNameNormalize:   cfg.SpawnNameNormalizeEnabled(),
		VegasInRegularMode:   cfg.ShowVegasInRegularMode(),
		HidePullLever:        cfg.HidePullLever(),
		ActivityBots: activityBotsView{
			Regular: cfg.ActivityBotsRegular(),
			Slop:    cfg.ActivityBotsSlop(),
			Wizard:  cfg.ActivityBotsWizard(),
		},
		HScrollFollow:     cfg.HScrollFollow(),
		GroupQuickOptions: cfg.GroupQuickOptions(),
		OverlayBackdrop:   cfg.OverlayBackdrop(),
		WindowControls:    cfg.WindowControls(),
		SnapAssist:        cfg.SnapAssist(),
		DefaultTerminal:   cfg.DefaultTerminal(),
		TerminalAttach: dashboardTerminalAttach{
			Mode:                 terminalAttachMode,
			InitialResizeDelayMS: terminalInitialDelay,
			RepairDelayMS:        terminalRepairDelay,
			PreAttachDelayMS:     terminalPreAttachDelay,
		},
		DefaultDirectoryPicker:        cfg.DefaultDirectoryPicker(),
		ShowAgentHideButton:           cfg.ShowAgentHideButton(),
		ShowGroupDescription:          cfg.ShowGroupDescription(),
		ShowFinalPRChecks:             cfg.ShowFinalPRChecks(),
		StayOnTerminalsAfterLastClose: cfg.StayOnTerminalsAfterLastClose(),
		FusedToolbarFixedNode:         cfg.FusedToolbarFixedNode(),
		UsageCollapseSubviews:         cfg.UsageCollapseSubviews(),
		ProcessesEnabled:              true,
		TriggersEnabled:               true,
		StewardsDisabled:              !cfg.StewardsEnabled(),
		Voice:                         cfg.Voice(),
		StewardName:                   cfg.StewardName(),
		GroupsRouteMapEnabled:         groupsRouteMapEnabled,
		GroupAttachmentsMode:          cfg.GroupAttachmentsMode(),
		TerminalPaletteShortcut:       cfg.TerminalCommandPaletteShortcutEnabled(),
		Keybindings:                   cfg.DashboardKeybindings(),
		RecordedSandboxDetails:        cfg.RecordedSandboxDetailsEnabled(),
		AgentRosterAuthoritative:      agentRosterErr == nil,
		Permissions: snapshotPermissionsView{
			Defaults:         defaults,
			Grants:           map[string][]string{},
			Overrides:        map[string]map[string]string{},
			UnreadableScopes: map[string][]string{},
		},
		Slugs: visiblePermissionRegistry(configuredProxyVisibility()),
	}
	out.RetiredTotal = retiredTotal
	sort.Slice(out.Slugs, func(i, j int) bool { return out.Slugs[i].Slug < out.Slugs[j].Slug })

	// Remote-access runtime state for the Config tab's guidance (JOH-227): has
	// setup generated the material, and is the listener live in this process.
	// Read above, with the rest of the host catalog, so its cost is measured.
	out.RemoteAccess = dashboardRemoteAccess{
		MaterialExists: raMaterial,
		Running:        raRunning,
		RunningBind:    raBind,
	}

	// Initialise slices empty (not nil) so JSON serializes [] instead
	// of null — the dashboard's JS does .length on members directly,
	// which would crash on null.
	out.Groups = []dashboardGroup{}
	out.SandboxProfiles, out.SandboxProfilesRev = sandboxProfileNamesRev(sandboxProfiles)
	if globalSandboxProfile != nil {
		out.SandboxProfileDefault = globalSandboxProfile.Name
	}
	out.Agents = []dashboardAgent{}
	// Close the payload preamble before the group loop starts, so "groups" is
	// the loop and nothing else. "other" is the rest of this window — the
	// groupNames map, the ~20 cfg accessors in the literal above, the slug copy
	// and sort, the sandbox-profile names — named rather than left as an
	// unexplained gap between the parent and its children, which is the very
	// problem this phase exists to remove.
	payloadPhases = append(payloadPhases, perfPhase{
		Name: "other",
		Ms:   durMs(max(time.Since(payloadStart)-payloadMeasured, 0)),
	})
	span.addChildren("payload", payloadPhases...)
	// 1: exclude only the Codex telemetry that accrued in THIS window. Zero
	// today (nothing here resolves a conv row), but keeping the split
	// consistent means a future row read moved into the preamble cannot make
	// groups subtract telemetry it never paid.
	codexAfterPayload := rc.codexTelemetryTiming.total
	span.markExcluding("payload", codexAfterPayload-codexAfterPreload)
	// id→name for resolving each group's parent_id to a parent NAME the
	// client tree keys off. Built from the same group set we're serializing,
	// so a parent_id with no live group simply doesn't resolve (child stays
	// top-level) — the FK's ON DELETE SET NULL should already prevent that.
	spawnGroups, err := db.AgentSpawnGroupIDs()
	if err != nil {
		slog.Warn("dashboard: spawn groups unavailable", "error", err)
	}
	groupNameByID := make(map[int64]string, len(groups))
	for _, g := range groups {
		groupNameByID[g.ID] = g.Name
	}
	// The groups phase was a single opaque number while its p99/max ran orders
	// of magnitude above its p50: the loop below fires three point queries PER
	// GROUP plus a per-conv row assembly, and nothing said which of them
	// stalled. gt accumulates each part so /api/perf can name the culprit.
	gt := newGroupsPhaseTimer(rc)
	var groupFedLinks map[int64][]groupFederationLink
	if !isPeerSnapshot(r) {
		groupFedLinks = gatherGroupFederationLinks()
	}
	for _, g := range groups {
		groupMark := gt.begin()
		groupPermissions := []string{}
		groupPermissionScopes := map[string]PermissionScope{}
		groupUnreadablePermissionScopes := []string{}
		gt.track(&gt.perms, func() {
			rows, _ := db.ListAgentGroupPermissionRows(g.ID)
			for _, row := range rows {
				groupPermissions = append(groupPermissions, row.Slug)
				if scope := permissionScopeFromJSON(row.ScopeJSON); len(scope) > 0 {
					groupPermissionScopes[row.Slug] = scope
				} else if row.ScopeJSON != "" {
					groupUnreadablePermissionScopes = append(groupUnreadablePermissionScopes, row.Slug)
				}
			}
		})
		attachment := groupAttachmentViewFor(g)
		dg := dashboardGroup{ID: g.ID, Name: g.Name, Descr: g.Descr, AttachmentURL: attachment.URL, AttachmentLabel: attachment.Label, AttachmentLabelOverride: attachment.LabelOverride, DefaultCwd: g.DefaultCwd, DefaultSpawnGroup: g.DefaultSpawnGroup, DefaultContext: g.DefaultContext, Environment: append([]sandboxpolicy.EnvironmentEntry(nil), g.Environment...), DefaultProfile: g.DefaultProfile, DefaultPrompts: g.DefaultPrompts, SandboxProfile: g.SandboxProfile, Permissions: groupPermissions, PermissionScopes: groupPermissionScopes, UnreadablePermissionScopes: groupUnreadablePermissionScopes, MaxMembers: g.MaxMembers, NotifyEnabled: g.NotifyEnabled, RemoteControlPolicy: remoteControlPolicyToWire(g.RemoteControl), ReinjectAfterCompact: g.EffectiveReinjectAfterCompact(), OwnerScopes: ownerScopesWire(g.OwnerScopesJSON), Mission: g.Mission, SourceTemplate: g.SourceTemplate, Scribe: isScribeGroup(g), RouteGeneration: g.RouteGeneration, Members: []dashboardMember{}}
		if g.ParentGroupID != nil {
			dg.Parent = groupNameByID[*g.ParentGroupID]
		}
		dg.FederationLinks = groupFedLinks[g.ID]
		// Advisory process state (JOH-242): attach the current phase + phase map
		// + transition log so the group view can render a phase chip + advance
		// control. nil for a group with no process.
		gt.track(&gt.process, func() {
			if st, trs, perr := loadGroupProcess(g.ID); perr == nil && st != nil {
				pv := processStateToJSON(st, trs)
				dg.Process = &pv
			}
		})
		// Staged-spawn choreography status (JOH-244): a "wave N/M pending" chip
		// while later waves are still deferred. nil when the deploy is complete.
		gt.track(&gt.waves, func() {
			if wv := loadWaveStatus(g.ID); wv != nil {
				dg.Waves = wv
			}
		})
		members := membersByGroup[g.ID]
		// Pre-load the owner set so we can tag members who are also
		// owners. Mirrors handleGroupMembersList in group_member_handlers.go.
		ownerSet := map[string]bool{}
		for _, o := range ownersByGroup[g.ID] {
			ownerSet[o.ConvID] = true
		}
		memberSet := map[string]bool{}
		memberMark := gt.begin()
		subscriptions := snapshotClaudeSubscriptions()
		for _, m := range members {
			memberSet[m.ConvID] = true
			b := rc.viewFor(m.ConvID)
			var routeHealth string
			if groupsRouteMapEnabled {
				routeHealth = dashboardRouteHealth(g, groupPermissions, b, darwinLaunchesByGroup[g.ID])
			}
			links := b.Links.withPresentedPRs(presentedPRsFor(b.AgentID)).
				withFreshestPRStates(freshPRStates).
				withPRChecks(prChecks).
				withPRTitles(cachedPRTitles).
				withPRReviews(prReviews)
			presence, _ := db.ProjectedAgentFederationPresence(b.AgentID)
			online := b.Online
			if presence != nil && presence.State == "away" {
				online = false
			}
			dg.Members = append(dg.Members, dashboardMember{
				FederationPresence: presence,
				ClaudeSubscription: subscriptions.forAgent(b.AgentID),
				AgentID:            b.AgentID,
				ConvID:             m.ConvID,
				Title:              b.Title,
				CreatedAt:          b.Created,
				Role:               m.Role,
				Descr:              m.Descr,
				agentLocationView:  b.Loc,
				repoLinksView:      links,
				taskRefView:        taskRefFor(b.AgentID),
				tagsView:           tagsFor(b.AgentID),
				Online:             online,
				Waking:             !b.Online && isConvWaking(m.ConvID),
				ExternalSteward:    db.IsExternalStewardConv(m.ConvID),

				SpawnedByExternalSteward: externalStewardParents[b.AgentID],

				Owner:           ownerSet[m.ConvID],
				State:           b.State,
				Notify:          notifyPrefs[m.ConvID],
				NotifyEffective: notifyEffective(m.ConvID),
				RouteHealth:     routeHealth,
			})
			if online {
				dg.Online++
			}
			// addAgent returns nil for a retired/superseded member (TCL-369):
			// its member row above still renders, but it never joins the
			// Agents/Ungrouped roster.
			if a := addAgent(m.ConvID); a != nil {
				a.Groups = append(a.Groups, g.Name)
				if a.AgentID != "" && spawnGroups[a.AgentID] == g.ID {
					a.SpawnGroup = g.Name
				}
				if ownerSet[m.ConvID] {
					a.OwnedGroups = append(a.OwnedGroups, g.Name)
				}
			}
		}
		gt.end(&gt.memberRows, memberMark)
		// Surface owners who aren't members so the list stays
		// comprehensive. Same shape as the CLI:
		// role="owner", no descr.
		ownerMark := gt.begin()
		for ownerConv := range ownerSet {
			if memberSet[ownerConv] {
				continue
			}
			b := rc.viewFor(ownerConv)
			var routeHealth string
			if groupsRouteMapEnabled {
				routeHealth = dashboardRouteHealth(g, groupPermissions, b, darwinLaunchesByGroup[g.ID])
			}
			ownerLinks := b.Links.withPresentedPRs(presentedPRsFor(b.AgentID)).
				withFreshestPRStates(freshPRStates).
				withPRChecks(prChecks).
				withPRTitles(cachedPRTitles).
				withPRReviews(prReviews)
			presence, _ := db.ProjectedAgentFederationPresence(b.AgentID)
			ownerOnline := b.Online && (presence == nil || presence.State != "away")
			dg.Members = append(dg.Members, dashboardMember{
				AgentID:         b.AgentID,
				ConvID:          ownerConv,
				Title:           b.Title,
				CreatedAt:       b.Created,
				Role:            "owner",
				ExternalSteward: db.IsExternalStewardConv(ownerConv),

				SpawnedByExternalSteward: externalStewardParents[b.AgentID],

				agentLocationView:  b.Loc,
				repoLinksView:      ownerLinks,
				taskRefView:        taskRefFor(b.AgentID),
				tagsView:           tagsFor(b.AgentID),
				Online:             ownerOnline,
				FederationPresence: presence,
				Waking:             !b.Online && isConvWaking(ownerConv),
				Owner:              true,
				State:              b.State,
				Notify:             notifyPrefs[ownerConv],
				NotifyEffective:    notifyEffective(ownerConv),
				RouteHealth:        routeHealth,
			})
			// Pure-owners are reachable via this group too — surface
			// the group on the agent's row in the Agents view so
			// "what groups can this conv see?" matches reality. nil for a
			// retired/superseded owner (TCL-369).
			if a := addAgent(ownerConv); a != nil {
				a.Groups = append(a.Groups, g.Name)
				a.OwnedGroups = append(a.OwnedGroups, g.Name)
			}
		}
		gt.end(&gt.ownerRows, ownerMark)
		// Default ordering: newest-first by creation time (the Age
		// column; the JS column sort treats this as the natural order it
		// falls back to). Mirrors handleGroupMembersList.
		sortMark := gt.begin()
		sortMembersByAge(dg.Members,
			func(m dashboardMember) string { return m.CreatedAt },
			func(m dashboardMember) string { return m.ConvID })
		gt.end(&gt.sortMembers, sortMark)
		out.Groups = append(out.Groups, dg)
		gt.observeGroup(g.Name, groupMark)
	}
	codexAfterGroups := rc.codexTelemetryTiming.total
	span.addChildren("groups", gt.phases()...)
	span.markExcluding("groups", codexAfterGroups-codexAfterPayload)
	// Route rows and leases were read alongside the roster. Resolve their
	// friendly labels only after group member rows are assembled so this
	// projection adds no per-route/per-member database work to the poll.
	if groupsRouteMapEnabled {
		routeMap := buildDashboardRouteMap(groups, out.Groups, routesByGroup, leasesByGroup, darwinLaunchesByGroup)
		appendDashboardHostPublications(&routeMap)
		out.RouteMap = &routeMap
	}
	for convID, slugs := range allGrants {
		addAgent(convID)
		copySlice := append([]string{}, slugs...)
		sort.Strings(copySlice)
		out.Permissions.Grants[convID] = copySlice
	}
	// Full tri-state overrides (grant AND deny) for the editor modal.
	// A conv with only a deny override is still an agent — surface it.
	for convID, slugEffects := range allOverrides {
		addAgent(convID)
		copyMap := make(map[string]string, len(slugEffects))
		for slug, effect := range slugEffects {
			copyMap[slug] = effect
		}
		out.Permissions.Overrides[convID] = copyMap
	}
	// The scope each override carries, decoded. A row this build cannot decode
	// is reported separately rather than as unscoped: the gate refuses such a
	// row (evalPermissionScope), so showing it as unconditional would be the
	// widest possible reading of a grant that authorizes nothing.
	for convID, bySlug := range allOverrideScopes {
		for slug, raw := range bySlug {
			scope := permissionScopeFromJSON(raw)
			if len(scope) == 0 {
				if strings.TrimSpace(raw) != "" {
					addAgent(convID)
					out.Permissions.UnreadableScopes[convID] = append(
						out.Permissions.UnreadableScopes[convID], slug)
				}
				continue
			}
			addAgent(convID)
			if out.Permissions.Scopes == nil {
				out.Permissions.Scopes = map[string]map[string]PermissionScope{}
			}
			if out.Permissions.Scopes[convID] == nil {
				out.Permissions.Scopes[convID] = map[string]PermissionScope{}
			}
			out.Permissions.Scopes[convID][slug] = scope
		}
	}

	// Register every active actor's current conv on the roster. The set was
	// loaded up front (activeConvIDs) so its conv-ids could join the batch; here
	// we just fold them into agentRows. addAgent skips a retired/superseded conv
	// (TCL-369) — harmless here, its return value is unused.
	for _, convID := range activeConvIDs {
		addAgent(convID)
	}

	// Bucket the active sudo grants (loaded up front) per stable agent id so the
	// per-agent Active rendering is O(1) inside the output loop. The same rows
	// feed the top-level Sudo[] for the dedicated tab.
	sudoByAgent := map[string][]dashboardSudoEntry{}
	out.Sudo = []dashboardSudoEntry{}
	sudoNow := time.Now()
	for _, g := range sudoGrants {
		// Grantee name from the batch-loaded conv_index cache — custom
		// title > pending name > summary > first prompt, no .jsonl
		// rescan. The pending-name tier covers a just-spawned
		// grantee before its first index event; "(unknown)" means
		// nothing resolved, which this surface renders as blank.
		title := rc.titleFor(g.ConvID)
		if title == agent.UnknownTitle {
			title = ""
		}
		remaining := int64(0)
		if rem := g.ExpiresAt.Sub(sudoNow); rem > 0 {
			remaining = int64(rem.Seconds())
		}
		topEntry := dashboardSudoEntry{
			ID:               g.ID,
			AgentID:          g.AgentID,
			ConvID:           g.ConvID,
			ConvTitle:        title,
			Slug:             g.Slug,
			ScopeJSON:        g.ScopeJSON,
			Carried:          g.CarriedID > 0,
			UsesLimit:        g.UsesLimit,
			UsesLeft:         g.UsesLeft,
			GrantedAt:        g.GrantedAt.Format(time.RFC3339Nano),
			ExpiresAt:        g.ExpiresAt.Format(time.RFC3339Nano),
			GrantedBy:        g.GrantedBy,
			Reason:           g.Reason,
			RemainingSeconds: remaining,
		}
		out.Sudo = append(out.Sudo, topEntry)
		// On the per-agent slice we omit ConvID — the agent row's
		// own ConvID already identifies who holds the grant. Saves
		// bytes on agents with many grants and keeps the JSON
		// readable in browser devtools.
		rowEntry := topEntry
		rowEntry.ConvID = ""
		rowEntry.AgentID = "" // the agent row already identifies the holder
		sudoByAgent[g.AgentID] = append(sudoByAgent[g.AgentID], rowEntry)
	}
	// All calls to rc.viewFor (group rows plus the grants/active-agent roster)
	// have completed, so this nested metric is the request's total Codex
	// telemetry cost rather than only the grouped subset.
	if err := rc.flushCodexContextWrites(); err != nil {
		slog.Warn("dashboard: batched Codex context snapshots completed with failures",
			"error", err, "module", "agentd")
	}
	span.addDuration("codex_telemetry", rc.codexTelemetryTiming.total)
	span.addChildren("codex_telemetry", rc.codexTelemetryTiming.perfPhases()...)
	span.markExcluding("roster", rc.codexTelemetryTiming.total-codexAfterGroups)

	out.Ungrouped = []dashboardAgent{}
	for _, a := range agentRows {
		// Defensive: a retired conv must never appear on the roster,
		// even if a partially-applied retire left a stale group/grant
		// row that the passes above picked up.
		if rc.inactiveActor(a.ConvID) {
			continue
		}
		// Effective comes from the same routine the permission GATE uses
		// (effectivePermsFor → resolvePermissionVerdict), so this column
		// cannot drift from what the daemon will actually allow. It used
		// to be a third hand-rolled union here, which omitted active sudo
		// elevations and the structural owner bypass.
		merged, _, _, _ := effectivePermsFor(permissionsState{
			Defaults:  out.Permissions.Defaults,
			Grants:    out.Permissions.Grants,
			Overrides: out.Permissions.Overrides,
		}, a.ConvID, ownerImpliedSlugsFor(a.ConvID))
		sort.Strings(merged)
		a.Effective = merged
		sort.Strings(a.Groups)
		sort.Strings(a.OwnedGroups)
		if rows := sudoByAgent[a.AgentID]; len(rows) > 0 {
			a.ActiveSudo = rows
		}
		out.Agents = append(out.Agents, *a)
		// An agent with no group memberships is "ungrouped" — surfaces
		// in the dedicated array so the dashboard can render the
		// virtual "Ungrouped" group (and feed the `+ add member`
		// overlay) without re-deriving the membership state. Effective
		// perms still come from the broader Agents row, so the
		// dashboard uses Ungrouped purely as a candidate-set hint.
		//
		// Online and offline alike: the virtual "Ungrouped" group is a
		// membership-management surface, so a freshly-promoted offline
		// conversation must show up there to be dragged into a group.
		// The `+ add member` overlay applies its own online filter, so
		// including offline rows here doesn't leak them into that
		// live-roster picker.
		if len(a.Groups) == 0 {
			out.Ungrouped = append(out.Ungrouped, *a)
		}
	}
	sort.Slice(out.Agents, func(i, j int) bool {
		return out.Agents[i].Title < out.Agents[j].Title
	})
	sort.Slice(out.Ungrouped, func(i, j int) bool {
		return out.Ungrouped[i].Title < out.Ungrouped[j].Title
	})
	span.mark("assemble")

	// The retired / conversations / replaced lists are no longer embedded in
	// the snapshot — the dashboard fetches them from their own paginated
	// endpoints (GET /api/retired, /api/conversations, /api/replaced). The
	// snapshot still guards the roster using actor state carried by the existing
	// AgentsByConv row-cache batch above.
	var (
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
	)
	// These collectors do not depend on each other. Most are one or two small
	// SQLite reads; serial execution made their fixed per-query overhead add up
	// to >10 ms even on tiny rosters.
	collectorPhases := runSnapshotNamedLoads(
		snapshotNamedLoad{"pending", func() { pending = collectPendingSnapshot(aliveSessions, groupNames) }},
		snapshotNamedLoad{"cron", func() { cron = collectCronSnapshot() }},
		snapshotNamedLoad{"export_jobs", func() { exportJobsActive, _ = db.CountActiveExportJobs() }},
		snapshotNamedLoad{"links", func() { links = collectLinksSnapshot(groupNames) }},
		snapshotNamedLoad{"usage", func() {
			showWhatIf := cfg != nil && cfg.Cost != nil && cfg.Cost.ShowOnSubscription
			usage, hasRealCost, usagePhases, costErr = collectUsageSnapshot(cfg.ResolvedUsageIdleTimeout(), showWhatIf, cfg)
		}},
		snapshotNamedLoad{"opencode_usage_activity", func() {
			openCodeActivity, _ = db.HasOpenCodeUsageActivitySince(time.Now().Add(-db.OpenCodeUsageActivityRetention))
		}},
		snapshotNamedLoad{"templates", func() { templates = collectTemplatesSnapshot() }},
		snapshotNamedLoad{"profiles", func() { profiles = collectProfilesSnapshot() }},
		snapshotNamedLoad{"default_profile", func() { defaultProfile = globalDefaultProfile() }},
		snapshotNamedLoad{"roles", func() { roles = collectRolesSnapshot() }},
		snapshotNamedLoad{"saved_prompts", func() {
			if savedPrompts, _ = db.ListSavedPrompts(); savedPrompts == nil {
				savedPrompts = []db.SavedPrompt{}
			}
		}},
		snapshotNamedLoad{"queued_spawns", func() { queuedSpawns = collectQueuedSpawnsSnapshot() }},
		snapshotNamedLoad{"capacity_queue", func() { capacityQueue = collectCapacityQueueSnapshot() }},
		snapshotNamedLoad{"messages", func() { messages, messagesUnread = buildHumanMessagesSnapshot() }},
		snapshotNamedLoad{"plugins", func() { plugins, pluginsWarn, pluginsErr = collectPluginsSnapshot() }},
	)
	for i := range collectorPhases {
		if collectorPhases[i].Name == "usage" {
			for j := range collectorPhases[i].Children {
				if collectorPhases[i].Children[j].Name == "run" {
					collectorPhases[i].Children[j].Children = usagePhases
					break
				}
			}
			break
		}
	}
	span.addChildren("collectors", collectorPhases...)
	out.Pending = pending
	out.Cron = cron
	// Badge-only: the Jobs tab's row window comes from /api/jobs, not the
	// snapshot. A count read error degrades to 0 (no badge), never a failed poll.
	out.ExportJobsActive = exportJobsActive
	out.Links = links
	out.Usage = usage
	out.AuthoredOpenPRs = associateAuthoredOpenPRs(
		withAuthoredPRReviews(withoutArchivedPRs(unionLocallyKnownOpenPRs(
			reconcileAuthoredOpenPRs(authoredOpenPRs, freshPRStates, prChecks, time.Now()),
			localPRs,
			freshPRStates,
			prChecks,
		)), prReviews),
		out.Agents,
	)
	out.AuthoredOpenMRs = authoredOpenMRsSnapshot(cfg, out.Agents)
	for _, view := range trackedPRAuthors {
		out.TrackedPRAuthors = append(out.TrackedPRAuthors, associateAuthoredOpenPRs(
			withAuthoredPRReviews(withoutArchivedPRs(
				reconcileAuthoredOpenPRs(view, freshPRStates, prChecks, time.Now())), prReviews),
			out.Agents,
		))
	}
	// Costs-tab visibility: show when there is real pay-per-token spend to
	// display, OR a subscription account has opted into the WHAT-IF view
	// (cost.show_on_subscription). A subscription-only account with the opt-in
	// off hides the tab — it would only show an empty chart. The opt-in also
	// reveals per-agent hypothetical badges. /api/costs always selects real
	// versus WHAT-IF per row and reports kind metadata; the front-end renders a
	// warning whenever hypothetical values contribute. A DB error
	// degrades to "no real cost" (the opt-in still governs), matching how the
	// cost figures themselves degrade to 0.
	if costErr != nil {
		slog.Debug("snapshot: cost visibility read failed; treating as no real cost", "error", costErr)
	}
	showOnSub := cfg != nil && cfg.Cost != nil && cfg.Cost.ShowOnSubscription
	out.CostTabVisible = hasRealCost || showOnSub
	out.CostTabWhatIf = showOnSub
	out.BrokerRefusalsTotal, out.BrokerRefusalsUnplaceable = brokerRefusals.counts()
	out.UsageTabVisible = usage.historyAvailable || openCodeActivity
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
	// The origins are the pages' keys: the local dashboard only, never a
	// peer's snapshot, restricted or not.
	if !isPeerSnapshot(r) {
		out.LivePages = livePagesSnapshotFor(r)
		out.GroupResources = groupResourcesSnapshot(groups, aliveSessions)
	}
	out.Templates = templates
	out.Profiles = profiles
	var httpProxyNames []string
	if cfg != nil && cfg.Agent != nil {
		for name := range cfg.Agent.HTTPProxies {
			httpProxyNames = append(httpProxyNames, name)
		}
	}
	out.Permissions.ScopeDimOptions = scopeDimOptionsSnapshot(
		groups, profiles, sandboxProfiles,
		cfg.ResolvedLinearProxy().AllowedTeams, cfg.ResolvedAWBProxy().AllowedWorkspaces, httpProxyNames)
	if defaultProfile != nil {
		out.SpawnProfileDefault = defaultProfile.Name
	}
	out.Roles = roles
	out.SavedPrompts = savedPrompts
	out.QueuedSpawns = queuedSpawns
	out.CapacityQueue = capacityQueue
	out.Messages, out.MessagesUnread = messages, messagesUnread
	out.AccessRequests = approvals.dashboardSnapshot()
	// Only the actionable (pending) ones drive the blink/banner/badge — the
	// list also carries recently-handled history, which must not count.
	out.AccessRequestsPending = approvals.pendingCount()
	out.Plugins, out.PluginsWarn = plugins, pluginsWarn
	if pluginsErr != nil {
		out.PluginsError = pluginsErr.Error()
	}
	out.PluginsCatalog = pluginCatalog()
	// Plugins-tab visibility mirrors the Costs-tab rule: show it when there's
	// something to manage (≥1 installed plugin), when plugins.json is broken
	// (so the human can see + fix the error), or when they've opted to always
	// keep it (config dashboard.always_show_plugins_tab — the escape hatch to
	// the install-from-catalog UI). Otherwise hide the empty tab.
	out.PluginsTabVisible = len(out.Plugins) > 0 || out.PluginsError != "" || cfg.ShowPluginsTabAlways()

	// Display-only cost compensation, applied as the final step over the
	// fully-assembled payload (cfg was loaded once at the top). The DB
	// rows feeding these figures stay raw — see config.CostConfig.
	applyCostDisplayFactor(&out, cfg)
	span.mark("collectors")

	// The large registries below change only after an explicit management
	// action (or a binary upgrade for the built-in registries). Hash them into
	// the dynamic snapshot and omit their contents when the browser proves it
	// already has this exact version. We still collect them before hashing so a
	// mutation is visible on the very next 2-second tick.
	out.StaticVersion = snapshotStaticVersion(out)
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

	writeJSON(w, http.StatusOK, out)
}

// sandboxProfileNamesRev lists the sandbox profiles' names, and a revision
// that changes whenever any of them is created, renamed, edited or deleted.
func sandboxProfileNamesRev(profiles []*db.SandboxProfile) ([]string, string) {
	names := []string{}
	rev := sha256.New()
	for _, profile := range profiles {
		names = append(names, profile.Name)
		fmt.Fprintf(rev, "%s\x00%s\x00", profile.Name, profile.UpdatedAt.Format(time.RFC3339Nano))
	}
	return names, hex.EncodeToString(rev.Sum(nil))[:16]
}

// snapshotStaticVersion returns a stable content fingerprint for the registry
// blobs restored client-side on an unchanged poll. Struct field order makes the
// JSON deterministic; each collector already returns a deterministic slice.
func snapshotStaticVersion(out snapshotPayload) string {
	static := struct {
		Slugs           []PermSlug                           `json:"slugs"`
		Templates       []templateJSON                       `json:"templates"`
		Profiles        []spawnProfileJSON                   `json:"profiles"`
		Roles           []roleJSON                           `json:"roles"`
		SavedPrompts    []db.SavedPrompt                     `json:"saved_prompts"`
		PluginsCatalog  []Plugin                             `json:"plugins_catalog"`
		Harnesses       []dashboardHarness                   `json:"harnesses"`
		SandboxImpl     dashboardSandboxImpl                 `json:"sandbox_impl"`
		ScopeDimOptions map[ScopeDim]snapshotScopeDimOptions `json:"scope_dim_options"`
	}{out.Slugs, out.Templates, out.Profiles, out.Roles, out.SavedPrompts, out.PluginsCatalog,
		out.Harnesses, out.SandboxImpl, out.Permissions.ScopeDimOptions}
	b, _ := json.Marshal(static) // all fields are JSON-native wire structs
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// applyCostDisplayFactor scales session badges using their own harness. Usage
// totals have already been scaled per delta before aggregation in the collector.
// The database and native credit amounts are never changed.
func applyCostDisplayFactor(out *snapshotPayload, cfg *config.Config) {
	scale := func(state *agentState) {
		factor := cfg.CostFactorForHarness(state.Harness)
		state.CostUSD *= factor
		state.VirtualCostUSD *= factor
	}
	for i := range out.Agents {
		scale(&out.Agents[i].State)
	}
	for i := range out.Ungrouped {
		scale(&out.Ungrouped[i].State)
	}
	for gi := range out.Groups {
		for mi := range out.Groups[gi].Members {
			scale(&out.Groups[gi].Members[mi].State)
		}
	}
}

// resolveRetiredByDisplay renders the dashboard's retired "by" column from the
// retire audit fields (JOH-306). retiredBy is the RAW value (a conv-id when an
// agent performed the retire, or a literal like "human" / "system:export-clone");
// retiredByAgent is the stable agent_id companion (db.RetireAgentByID derives it,
// the v78 backfill fills historical rows). agent_id is the source of truth, so:
//
//   - With a companion, name the retirer by its CURRENT title plus stable short
//     agent_id — "name (agt_xxxxxxxx)", the roster style (PR3c) — which is
//     rotation-immune (always the retirer's live name, not a stale generation's).
//     When the name can't be resolved, fall back to the bare short agent_id.
//   - Without a companion (a human/system literal, or a pre-v78 row whose
//     retired_by didn't resolve), show the raw retiredBy value unchanged.
//
// It never returns a bare conv-id for an agent retirer — that was the bug.
func resolveRetiredByDisplay(retiredBy, retiredByAgent string) string {
	retiredBy = strings.TrimSpace(retiredBy)
	retiredByAgent = strings.TrimSpace(retiredByAgent)
	if retiredByAgent == "" {
		return retiredBy // literal, or unresolvable — show the raw audit value
	}
	shortID := agent.ShortAgentID(retiredByAgent, "")
	name := ""
	if a, _ := db.GetAgent(retiredByAgent); a != nil {
		name = agent.CachedTitle(a.CurrentConvID)
	}
	if name == "" || name == agent.UnknownTitle {
		return shortID // name gone — the stable agent_id is the durable fallback
	}
	return name + " (" + shortID + ")"
}

// collectPendingSnapshot turns the pending_spawns rows into the
// dashboard's "Pending" list — dashboard spawns whose conv-id has not
// materialised yet (a live tmux pane stuck behind a startup gate). Each
// carries the reserved stable agent id, the spawn label (the focus key —
// there is no conv-id), the resolved group name, the spawn descriptors, and
// — from the spawn's session row — whether its pane is still alive and where
// it is gated.
// Newest spawn first, so the agent the operator just clicked sits at the
// top. aliveSessions is the snapshot-shaped alive set the caller
// pre-fetched; this function never spawns its own tmux probe. Returns an
// empty slice (not nil) so JS .map() / .length are safe.
func collectPendingSnapshot(aliveSessions map[string]struct{}, groupNames map[int64]string) []dashboardPending {
	out := []dashboardPending{}
	pendings, err := db.ListPendingSpawns()
	if err != nil {
		return out
	}
	labels := make([]string, 0, len(pendings))
	for _, pending := range pendings {
		labels = append(labels, pending.Label)
	}
	sessions, _ := db.LoadSessionsByIDs(labels)
	for _, p := range pendings {
		dp := dashboardPending{
			AgentID:   p.AgentID,
			Label:     p.Label,
			Group:     groupNames[p.GroupID],
			Role:      p.Role,
			Name:      p.Name,
			Descr:     p.Descr,
			CreatedAt: p.CreatedAt,
		}
		// The session row (keyed by label, since the conv-id is the very
		// thing that hasn't materialised) tells us whether the pane is
		// still alive and where the agent is gated. A pending row with no
		// session row, or a dead pane, renders as offline — stale, no
		// longer focusable.
		if sess := sessions[p.Label]; sess != nil {
			if sess.TmuxSession != "" {
				_, dp.Online = aliveSessions[sess.TmuxSession]
			}
			dp.Cwd = sess.Cwd
			dp.Harness = sess.Harness
		}
		out = append(out, dp)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt > out[j].CreatedAt // newest first
	})
	return out
}

// collectLinksSnapshot enumerates every inter-group link, resolved through the
// group-name map the snapshot already loaded during preload. The link list is
// therefore this collector's only DB hit; per-row lookups are map indexed.
// Returns an empty slice (not nil) so JS can safely call .map() / .length
// without a guard.
func collectLinksSnapshot(names map[int64]string) []dashboardLink {
	out := []dashboardLink{}
	rows, err := db.ListAllAgentGroupLinks()
	if err != nil {
		return out
	}
	for _, l := range rows {
		out = append(out, dashboardLink{
			ID:        l.ID,
			From:      names[l.FromGroupID],
			To:        names[l.ToGroupID],
			Mode:      l.Mode,
			CreatedAt: l.CreatedAt.Format(time.RFC3339),
		})
	}
	remote, _ := db.ListFederationGroupLinks(0)
	for _, l := range remote {
		peer, _ := db.GetFederationPeer(l.Peer)
		label := l.Peer
		if peer != nil {
			label = peerDisplay(peer)
		}
		from, to := names[l.LocalGroupID], l.RemoteGroupName
		if l.Direction == "in" {
			from, to = to, from
		}
		rt := currentFederation()
		row := dashboardLink{ID: l.ID, From: from, To: to, Mode: l.Mode, Peer: l.Peer, PeerLabel: label, RemoteGroup: l.RemoteGroupName, RemoteGroupID: l.RemoteGroupID, Direction: l.Direction, LocalState: l.LocalState, RemoteState: l.RemoteState, RemoteStale: rt == nil || !rt.sessionPeerOnline(l.Peer), RequestID: l.ProposalID, CreatedAt: l.CreatedAt.UTC().Format(time.RFC3339)}
		if !l.RemoteStateAt.IsZero() {
			row.RemoteStateAt = l.RemoteStateAt.UTC().Format(time.RFC3339)
		}
		if p, _ := db.GetFederationProposal(l.ProposalID); p != nil {
			row.Deadline = p.Deadline.UTC().Format(time.RFC3339)
		}
		out = append(out, row)
	}
	return out
}

// collectCronSnapshot builds the wire-shape rows for the dashboard's
// Cron tab. Resolves owner/target conv-ids to display titles and
// computes the next-due timestamp so the UI doesn't need a clock-
// arithmetic helper. Returns an empty slice (not nil) so the page's
// .map() doesn't crash on null.
func collectCronSnapshot() []dashboardCronJob {
	out := []dashboardCronJob{}
	jobs, err := db.ListAgentCronJobs()
	if err != nil {
		return out
	}
	// Cache group-id → name across rows in the same snapshot to avoid
	// a per-row lookup.
	groupNames := map[int64]string{}
	for _, j := range jobs {
		out = append(out, cronJobToView(j, groupNames))
	}
	return out
}

// cronJobToView maps one agent_cron_jobs row to its dashboard view. groupNames
// caches group-id → name lookups across rows in the same collection pass.
// Shared by the snapshot's cron list (above) and the Jobs tab's unified
// /api/jobs listing (dashboard_jobs.go).
func cronJobToView(j *db.AgentCronJob, groupNames map[int64]string) dashboardCronJob {
	row := dashboardCronJob{
		ID:                         j.ID,
		Name:                       j.Name,
		OwnerAgent:                 j.OwnerAgent,
		OwnerConv:                  j.OwnerConv,
		OwnerLabel:                 labelForConv(j.OwnerConv),
		TargetKind:                 j.TargetKind,
		TargetAgent:                j.TargetAgent,
		TargetConv:                 j.TargetConv,
		GroupID:                    j.GroupID,
		TargetRole:                 j.TargetRole,
		IntervalSeconds:            j.IntervalSeconds,
		CronExpr:                   j.CronExpr,
		CronDesc:                   cronexpr.Describe(j.CronExpr),
		Subject:                    j.Subject,
		Body:                       j.Body,
		ActionKind:                 j.ActionKind,
		SpawnProfile:               j.SpawnProfile,
		SpawnRoleRefs:              j.SpawnRoleRefs,
		SpawnNameTemplate:          j.SpawnNameTemplate,
		SpawnInstructionTemplate:   j.SpawnInstructionTemplate,
		SpawnConcurrencyPolicy:     j.SpawnConcurrencyPolicy,
		SpawnMaxLiveWorkers:        j.SpawnMaxLiveWorkers,
		SpawnWorkerDeadlineSeconds: j.SpawnWorkerDeadlineSeconds,
		Enabled:                    j.Enabled,
		RunImmediately:             j.RunImmediately,
		QueueWhenOffline:           j.QueueWhenOffline,
		OperatorAuthored:           j.OperatorAuthored,
		LastRunStatus:              j.LastRunStatus,
	}
	if j.TargetConv != "" {
		row.TargetLabel = labelForConv(j.TargetConv)
	}
	// Resolve group_name ONLY for a group-target job. A conv-target
	// job routed through a shared group also carries a non-zero
	// group_id, but it is not a multicast — leaving group_name empty
	// keeps target_kind the sole, unambiguous discriminator the
	// dashboard renders off.
	if j.IsGroupTarget() && j.GroupID > 0 {
		name, ok := groupNames[j.GroupID]
		if !ok {
			if g, gerr := db.GetAgentGroupByID(j.GroupID); gerr == nil && g != nil {
				name = g.Name
			}
			groupNames[j.GroupID] = name
		}
		row.GroupName = name
	}
	if !j.CreatedAt.IsZero() {
		row.CreatedAt = j.CreatedAt.Format(time.RFC3339)
	}
	if !j.LastRunAt.IsZero() {
		row.LastRunAt = j.LastRunAt.Format(time.RFC3339)
	}
	// Next-due mirrors the scheduler's due check in db.ListDueAgentCronJobs:
	// both modes anchor never-run jobs at created_at, matching the scheduler.
	if j.CronExpr != "" {
		base := j.LastRunAt
		if base.IsZero() {
			base = j.CreatedAt
		}
		if next, err := cronexpr.Next(j.CronExpr, base); err == nil && !next.IsZero() {
			row.NextDueAt = next.Format(time.RFC3339)
		}
	} else {
		base := j.LastRunAt
		if base.IsZero() {
			base = j.CreatedAt
		}
		if !base.IsZero() && j.IntervalSeconds > 0 {
			next := base.Add(time.Duration(j.IntervalSeconds) * time.Second)
			row.NextDueAt = next.Format(time.RFC3339)
		}
	}
	return row
}

// labelForConv returns a short display label for a conv-id. Resolves
// the conv's name from the live conv_index cache (custom title >
// pending name > summary > first prompt — no .jsonl rescan), then
// falls back to the 8-char prefix when nothing resolves. Mirrors the
// rendering used in the Groups/Agents tabs.
func labelForConv(convID string) string {
	if convID == "" {
		return ""
	}
	if t := agent.CachedTitle(convID); t != "" && t != agent.UnknownTitle {
		return t
	}
	if len(convID) >= 8 {
		return convID[:8]
	}
	return convID
}

// dashboardSandboxImplementation shows a session row's recorded implementation
// in its normalized spelling, so a row recorded under the retired "stacked"
// value reads as the tclaude-layer it now resumes as. Empty and unrecognized
// values pass through unchanged.
func dashboardSandboxImplementation(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return raw
	}
	if implementation, err := sandboxpolicy.NormalizeImplementation(raw); err == nil {
		return string(implementation)
	}
	return raw
}
