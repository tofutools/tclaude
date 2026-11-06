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

	p := loadSnapshotPreload(span)
	rows := loadSnapshotGroupRows(span, p.groups, p.groupsRouteMapEnabled)
	notifyEffective := snapshotNotifyFilter(p.groups, rows.membersByGroup, p.notifyPrefs)
	convIDs := collectSnapshotConvIDs(span, p, rows)
	rc := newSnapshotRowCache(convIDs, sharedStatus, func(phases []perfPhase) {
		span.addChildren("preload", phases...)
	})
	prs := loadSnapshotPRIndex(span, rc, convIDs, p.cfg)
	externalStewardParents := externalStewardChildNames()

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
	groupNames := make(map[int64]string, len(p.groups))
	for _, group := range p.groups {
		groupNames[group.ID] = group.Name
	}
	rowInputs := snapshotRowInputs{
		rc:                     rc,
		prs:                    prs,
		notifyPrefs:            p.notifyPrefs,
		notifyEffective:        notifyEffective,
		externalStewardParents: externalStewardParents,
	}
	agents := newSnapshotAgentRows(rowInputs)
	out := assembleSnapshotPayload(r, span, p, payloadStart)
	// 1: exclude only the Codex telemetry that accrued in THIS window. Zero
	// today (nothing here resolves a conv row), but keeping the split
	// consistent means a future row read moved into the preamble cannot make
	// groups subtract telemetry it never paid.
	codexAfterPayload := rc.codexTelemetryTiming.total
	span.markExcluding("payload", codexAfterPayload-codexAfterPreload)
	out.Groups = buildSnapshotGroups(r, span, p.groups, rowInputs, rows, agents, p.groupsRouteMapEnabled)
	codexAfterGroups := rc.codexTelemetryTiming.total
	span.markExcluding("groups", codexAfterGroups-codexAfterPayload)
	// Route rows and leases were read alongside the roster. Resolve their
	// friendly labels only after group member rows are assembled so this
	// projection adds no per-route/per-member database work to the poll.
	if p.groupsRouteMapEnabled {
		routeMap := buildDashboardRouteMap(p.groups, out.Groups, rows.routesByGroup, rows.leasesByGroup, rows.darwinLaunchesByGroup)
		appendDashboardHostPublications(&routeMap)
		out.RouteMap = &routeMap
	}
	registerSnapshotPermissions(&out.Permissions, p, agents)

	// Register every active actor's current conv on the roster. The set was
	// loaded up front (activeConvIDs) so its conv-ids could join the batch; here
	// we just fold them into the agent rows. agents.add skips a retired/superseded
	// conv (TCL-369) — harmless here, its return value is unused.
	for _, convID := range p.activeConvIDs {
		agents.add(convID)
	}

	sudo, sudoByAgent := buildSnapshotSudo(p.sudoGrants, rc)
	out.Sudo = sudo
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

	out.Agents, out.Ungrouped = finishSnapshotRoster(agents, out.Permissions, sudoByAgent)
	span.mark("assemble")

	// The retired / conversations / replaced lists are no longer embedded in
	// the snapshot — the dashboard fetches them from their own paginated
	// endpoints (GET /api/retired, /api/conversations, /api/replaced). The
	// snapshot still guards the roster using actor state carried by the existing
	// AgentsByConv row-cache batch above.
	c := runSnapshotCollectors(span, p.cfg, aliveSessions, groupNames)
	attachSnapshotCollected(&out, c, p)
	attachSnapshotPRLists(&out, prs, p.cfg)
	attachSnapshotLiveState(&out, p.cfg)
	// The origins are the pages' keys: the local dashboard only, never a
	// peer's snapshot, restricted or not.
	if !isPeerSnapshot(r) {
		out.LivePages = livePagesSnapshotFor(r)
		out.GroupResources = groupResourcesSnapshot(p.groups, aliveSessions)
	}

	// Display-only cost compensation, applied as the final step over the
	// fully-assembled payload (cfg was loaded once at the top). The DB
	// rows feeding these figures stay raw — see config.CostConfig.
	applyCostDisplayFactor(&out, p.cfg)
	span.mark("collectors")

	elideUnchangedStatics(&out, r)
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
	// A linked node's job is labelled as that node's (S7b).
	if j.IsPeerOwned() {
		row.OwnerPeer = j.OwnerPeer
		owner := processOwnerPeerPrefix + j.OwnerPeer
		if j.OwnerPeerAgent != "" {
			owner += "/agent:" + j.OwnerPeerAgent
		}
		row.OwnerLabel = processPeerOwnerLabel(owner)
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
