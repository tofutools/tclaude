package agentd

import (
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/remoteaccess"
	"github.com/tofutools/tclaude/pkg/common/buildversion"
)

// The snapshot's assembly steps: the payload preamble, the groups, and the
// roster (permissions, sudo, and the final Agents/Ungrouped lists).
// handleDashboardSnapshot calls them in order between its phase marks.

// snapshotRowInputs is what every agent and member row draws from: the row
// cache, the PR index, the notify state and the external-steward parents.
type snapshotRowInputs struct {
	rc                     *snapshotRowCache
	prs                    snapshotPRIndex
	notifyPrefs            map[string]string
	notifyEffective        func(convID string) bool
	externalStewardParents map[string]string
}

// linksFor is a row's repo links decorated with its presented PRs and the
// snapshot-wide PR state, checks, titles and reviews.
func (in snapshotRowInputs) linksFor(b *convRowBundle) repoLinksView {
	return b.Links.withPresentedPRs(in.prs.presentedPRsFor(b.AgentID)).
		withFreshestPRStates(in.prs.freshPRStates).
		withPRChecks(in.prs.prChecks).
		withPRTitles(in.prs.cachedPRTitles).
		withPRReviews(in.prs.prReviews)
}

// snapshotAgentRows is the roster: the union of (every group member) +
// (every conv-id with explicit grants). Keyed by conv-id so members
// appearing in multiple groups dedupe naturally.
type snapshotAgentRows struct {
	snapshotRowInputs
	byConv map[string]*dashboardAgent
}

func newSnapshotAgentRows(in snapshotRowInputs) *snapshotAgentRows {
	return &snapshotAgentRows{snapshotRowInputs: in, byConv: map[string]*dashboardAgent{}}
}

// add returns convID's agent row, creating it on first use.
func (ar *snapshotAgentRows) add(convID string) *dashboardAgent {
	if existing, ok := ar.byConv[convID]; ok {
		return existing
	}
	// TCL-369: a retired/superseded conv never becomes an agent row, so
	// skip its resolution entirely instead of building it and dropping it
	// in the output loop. Callers that use the return value nil-check it.
	if ar.rc.inactiveActor(convID) {
		return nil
	}
	b := ar.rc.viewFor(convID)
	links := ar.linksFor(b)
	a := &dashboardAgent{
		AgentID:           b.AgentID,
		ConvID:            convID,
		Title:             b.Title,
		agentLocationView: b.Loc,
		repoLinksView:     links,
		taskRefView:       ar.prs.taskRefFor(b.AgentID),
		tagsView:          ar.prs.tagsFor(b.AgentID),
		Online:            b.Online,
		Waking:            !b.Online && isConvWaking(convID),
		State:             b.State,
		ExternalSteward:   db.IsExternalStewardConv(convID),

		SpawnedByExternalSteward: ar.externalStewardParents[b.AgentID],
		// init non-nil so JSON serializes [] not null;
		// the dashboard's JS does .length / .map without a guard.
		Groups:          []string{},
		OwnedGroups:     []string{},
		Effective:       []string{},
		Notify:          ar.notifyPrefs[convID],
		NotifyEffective: ar.notifyEffective(convID),
	}
	ar.byConv[convID] = a
	return a
}

// assembleSnapshotPayload builds the payload preamble: the host-catalog
// reads (each timed as a "payload" child), the config-driven flags, and the
// permission and sandbox-profile scaffolding the later steps fill in. Its
// "other" child is the rest of the window since payloadStart.
func assembleSnapshotPayload(r *http.Request, span *perfSpan, p snapshotPreload, payloadStart time.Time) snapshotPayload {
	// Host-catalog reads, measured. These look like plain struct fields, but
	// two of them reach the host:
	//
	//   - buildSandboxImplCatalog FORKS bwrap to probe namespace availability
	//     (twice — interactive and server boundary — plus a pidfd probe)
	//     whenever its 60 s disclosure cache expires. At a 2 s poll that is one poll in thirty paying a
	//     multi-fork bill: exactly the shape of an occasional
	//     multi-hundred-ms spike in an otherwise ~13 ms request.
	//   - The harness catalog used to cost ~500 ms whenever OpenCode's model
	//     catalog went stale (it EXECUTED `opencode models`), and ~300-400 ms
	//     whenever pi's did (`pi --list-models`). The whole catalog is now
	//     served from memory and rebuilt in the background
	//     (harness_catalog_cache.go), so this should stay in the microseconds
	//     on every poll but the daemon's very first.
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
	payloadPhase("harness_catalog", func() { harnesses = cachedHarnessCatalog() })
	payloadPhase("sandbox_impl", func() { sandboxImpl = buildSandboxImplCatalog() })
	payloadPhase("remote_access", func() {
		raRunning, raBind = remoteListenerStatus()
		raMaterial = remoteaccess.Exists()
	})
	terminalAttachMode, terminalInitialDelay, terminalRepairDelay, terminalPreAttachDelay := p.cfg.ResolvedTerminalAttach()
	out := snapshotPayload{
		GeneratedAt:          time.Now().Format(time.RFC3339),
		Version:              buildversion.AppVersion(),
		AssetsVersion:        dashboardAssetsVersion,
		AuthSession:          authSession,
		PopupBase:            popupBaseURL,
		UserDefaultModel:     userModel,
		Harnesses:            harnesses,
		SandboxImpl:          sandboxImpl,
		NotificationsEnabled: p.cfg != nil && p.cfg.Notifications != nil && p.cfg.Notifications.Enabled,
		SpawnNameNormalize:   p.cfg.SpawnNameNormalizeEnabled(),
		VegasInRegularMode:   p.cfg.ShowVegasInRegularMode(),
		HidePullLever:        p.cfg.HidePullLever(),
		ActivityBots: activityBotsView{
			Regular: p.cfg.ActivityBotsRegular(),
			Slop:    p.cfg.ActivityBotsSlop(),
			Wizard:  p.cfg.ActivityBotsWizard(),
		},
		HScrollFollow:     p.cfg.HScrollFollow(),
		GroupQuickOptions: p.cfg.GroupQuickOptions(),
		OverlayBackdrop:   p.cfg.OverlayBackdrop(),
		WindowControls:    p.cfg.WindowControls(),
		SnapAssist:        p.cfg.SnapAssist(),
		DefaultTerminal:   p.cfg.DefaultTerminal(),
		TerminalAttach: dashboardTerminalAttach{
			Mode:                 terminalAttachMode,
			InitialResizeDelayMS: terminalInitialDelay,
			RepairDelayMS:        terminalRepairDelay,
			PreAttachDelayMS:     terminalPreAttachDelay,
		},
		DefaultDirectoryPicker:        p.cfg.DefaultDirectoryPicker(),
		ShowAgentHideButton:           p.cfg.ShowAgentHideButton(),
		ShowGroupDescription:          p.cfg.ShowGroupDescription(),
		SidePanelEdgeReveal:           p.cfg.SidePanelEdgeReveal(),
		SidePanelTuning:               p.cfg.SidePanelTuning(),
		ShowFinalPRChecks:             p.cfg.ShowFinalPRChecks(),
		StayOnTerminalsAfterLastClose: p.cfg.StayOnTerminalsAfterLastClose(),
		FusedToolbarFixedNode:         p.cfg.FusedToolbarFixedNode(),
		UsageCollapseSubviews:         p.cfg.UsageCollapseSubviews(),
		ProcessesEnabled:              true,
		TriggersEnabled:               true,
		StewardsDisabled:              !p.cfg.StewardsEnabled(),
		Voice:                         p.cfg.Voice(),
		StewardName:                   p.cfg.StewardName(),
		GroupsRouteMapEnabled:         p.groupsRouteMapEnabled,
		GroupAttachmentsMode:          p.cfg.GroupAttachmentsMode(),
		TerminalPaletteShortcut:       p.cfg.TerminalCommandPaletteShortcutEnabled(),
		Keybindings:                   p.cfg.DashboardKeybindings(),
		RecordedSandboxDetails:        p.cfg.RecordedSandboxDetailsEnabled(),
		AgentRosterAuthoritative:      p.agentRosterErr == nil,
		Permissions: snapshotPermissionsView{
			Defaults:         p.defaults,
			Grants:           map[string][]string{},
			Overrides:        map[string]map[string]string{},
			UnreadableScopes: map[string][]string{},
		},
		Slugs: visiblePermissionRegistry(configuredProxyVisibility()),
	}
	out.RetiredTotal = p.retiredTotal
	sort.Slice(out.Slugs, func(i, j int) bool { return out.Slugs[i].Slug < out.Slugs[j].Slug })

	// Remote-access runtime state for the Config tab's guidance (JOH-227): has
	// setup generated the material, and is the listener live in this process.
	// Read above, with the rest of the host catalog, so its cost is measured.
	out.RemoteAccess = dashboardRemoteAccess{
		MaterialExists: raMaterial,
		Running:        raRunning,
		RunningBind:    raBind,
	}

	out.SandboxProfiles, out.SandboxProfilesRev = sandboxProfileNamesRev(p.sandboxProfiles)
	if p.globalSandboxProfile != nil {
		out.SandboxProfileDefault = p.globalSandboxProfile.Name
	}
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
	return out
}

// snapshotGroupsBuild is what rendering one group needs beyond the group
// itself: the shared row inputs, the per-group rows, the agent registry the
// member and owner rows join, and the group-loop phase timer.
type snapshotGroupsBuild struct {
	snapshotRowInputs
	rows            snapshotGroupRows
	agents          *snapshotAgentRows
	routeMapEnabled bool
	spawnGroups     map[string]int64
	groupNameByID   map[int64]string
	groupFedLinks   map[int64][]groupFederationLink
	gt              *groupsPhaseTimer
}

// buildSnapshotGroups renders every group, timing the loop's parts as the
// "groups" phase children.
func buildSnapshotGroups(r *http.Request, span *perfSpan, groups []*db.AgentGroup, in snapshotRowInputs,
	rows snapshotGroupRows, agents *snapshotAgentRows, routeMapEnabled bool) []dashboardGroup {
	gb := snapshotGroupsBuild{snapshotRowInputs: in, rows: rows, agents: agents, routeMapEnabled: routeMapEnabled}
	var err error
	gb.spawnGroups, err = db.AgentSpawnGroupIDs()
	if err != nil {
		slog.Warn("dashboard: spawn groups unavailable", "error", err)
	}
	// id→name for resolving each group's parent_id to a parent NAME the
	// client tree keys off. Built from the same group set we're serializing,
	// so a parent_id with no live group simply doesn't resolve (child stays
	// top-level) — the FK's ON DELETE SET NULL should already prevent that.
	gb.groupNameByID = make(map[int64]string, len(groups))
	for _, g := range groups {
		gb.groupNameByID[g.ID] = g.Name
	}
	// The groups phase was a single opaque number while its p99/max ran orders
	// of magnitude above its p50: the loop below fires three point queries PER
	// GROUP plus a per-conv row assembly, and nothing said which of them
	// stalled. gt accumulates each part so /api/perf can name the culprit.
	gb.gt = newGroupsPhaseTimer(in.rc)
	if !isPeerSnapshot(r) {
		gb.groupFedLinks = gatherGroupFederationLinks()
	}
	// Initialise slices empty (not nil) so JSON serializes [] instead
	// of null — the dashboard's JS does .length on members directly,
	// which would crash on null.
	dgs := []dashboardGroup{}
	for _, g := range groups {
		groupMark := gb.gt.begin()
		dgs = append(dgs, gb.buildGroup(g))
		gb.gt.observeGroup(g.Name, groupMark)
	}
	span.addChildren("groups", gb.gt.phases()...)
	return dgs
}

// buildGroup renders one group: its permissions, process and wave state,
// then its member rows and its owner-only rows, newest first. Each row
// also registers its conv on the agent roster.
func (gb snapshotGroupsBuild) buildGroup(g *db.AgentGroup) dashboardGroup {
	groupPermissions := []string{}
	groupPermissionScopes := map[string]PermissionScope{}
	groupUnreadablePermissionScopes := []string{}
	gb.gt.track(&gb.gt.perms, func() {
		rows, _ := db.ListAgentGroupPermissionRows(g.ID)
		groupPermissions, groupPermissionScopes, groupUnreadablePermissionScopes = groupPermissionsView(rows)
	})
	attachment := groupAttachmentViewFor(g)
	dg := dashboardGroup{ID: g.ID, Name: g.Name, Descr: g.Descr, AttachmentURL: attachment.URL, AttachmentLabel: attachment.Label, AttachmentLabelOverride: attachment.LabelOverride, DefaultCwd: g.DefaultCwd, DefaultSpawnGroup: g.DefaultSpawnGroup, DefaultContext: g.DefaultContext, Environment: append([]sandboxpolicy.EnvironmentEntry(nil), g.Environment...), DefaultProfile: g.DefaultProfile, DefaultPrompts: g.DefaultPrompts, SandboxProfile: g.SandboxProfile, Permissions: groupPermissions, PermissionScopes: groupPermissionScopes, UnreadablePermissionScopes: groupUnreadablePermissionScopes, MaxMembers: g.MaxMembers, NotifyEnabled: g.NotifyEnabled, RemoteControlPolicy: remoteControlPolicyToWire(g.RemoteControl), ReinjectAfterCompact: g.EffectiveReinjectAfterCompact(), OwnerScopes: ownerScopesWire(g.OwnerScopesJSON), Mission: g.Mission, SourceTemplate: g.SourceTemplate, Scribe: isScribeGroup(g), RouteGeneration: g.RouteGeneration, Members: []dashboardMember{}}
	if g.ParentGroupID != nil {
		dg.Parent = gb.groupNameByID[*g.ParentGroupID]
	}
	dg.FederationLinks = gb.groupFedLinks[g.ID]
	// Advisory process state (JOH-242): attach the current phase + phase map
	// + transition log so the group view can render a phase chip + advance
	// control. nil for a group with no process.
	gb.gt.track(&gb.gt.process, func() {
		if st, trs, perr := loadGroupProcess(g.ID); perr == nil && st != nil {
			pv := processStateToJSON(st, trs)
			dg.Process = &pv
		}
	})
	// Staged-spawn choreography status (JOH-244): a "wave N/M pending" chip
	// while later waves are still deferred. nil when the deploy is complete.
	gb.gt.track(&gb.gt.waves, func() {
		if wv := loadWaveStatus(g.ID); wv != nil {
			dg.Waves = wv
		}
	})
	members := gb.rows.membersByGroup[g.ID]
	// Pre-load the owner set so we can tag members who are also
	// owners. Mirrors handleGroupMembersList in group_member_handlers.go.
	ownerSet := map[string]bool{}
	for _, o := range gb.rows.ownersByGroup[g.ID] {
		ownerSet[o.ConvID] = true
	}
	memberSet := map[string]bool{}
	memberMark := gb.gt.begin()
	subscriptions := snapshotClaudeSubscriptions()
	for _, m := range members {
		memberSet[m.ConvID] = true
		b := gb.rc.viewFor(m.ConvID)
		var routeHealth string
		if gb.routeMapEnabled {
			routeHealth = dashboardRouteHealth(g, groupPermissions, b, gb.rows.darwinLaunchesByGroup[g.ID])
		}
		links := gb.linksFor(b)
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
			taskRefView:        gb.prs.taskRefFor(b.AgentID),
			tagsView:           gb.prs.tagsFor(b.AgentID),
			Online:             online,
			Waking:             !b.Online && isConvWaking(m.ConvID),
			ExternalSteward:    db.IsExternalStewardConv(m.ConvID),

			SpawnedByExternalSteward: gb.externalStewardParents[b.AgentID],

			Owner:           ownerSet[m.ConvID],
			State:           b.State,
			Notify:          gb.notifyPrefs[m.ConvID],
			NotifyEffective: gb.notifyEffective(m.ConvID),
			RouteHealth:     routeHealth,
		})
		if online {
			dg.Online++
		}
		// agents.add returns nil for a retired/superseded member (TCL-369):
		// its member row above still renders, but it never joins the
		// Agents/Ungrouped roster.
		if a := gb.agents.add(m.ConvID); a != nil {
			a.Groups = append(a.Groups, g.Name)
			if a.AgentID != "" && gb.spawnGroups[a.AgentID] == g.ID {
				a.SpawnGroup = g.Name
			}
			if ownerSet[m.ConvID] {
				a.OwnedGroups = append(a.OwnedGroups, g.Name)
			}
		}
	}
	gb.gt.end(&gb.gt.memberRows, memberMark)
	// Surface owners who aren't members so the list stays
	// comprehensive. Same shape as the CLI:
	// role="owner", no descr.
	ownerMark := gb.gt.begin()
	for ownerConv := range ownerSet {
		if memberSet[ownerConv] {
			continue
		}
		b := gb.rc.viewFor(ownerConv)
		var routeHealth string
		if gb.routeMapEnabled {
			routeHealth = dashboardRouteHealth(g, groupPermissions, b, gb.rows.darwinLaunchesByGroup[g.ID])
		}
		ownerLinks := gb.linksFor(b)
		presence, _ := db.ProjectedAgentFederationPresence(b.AgentID)
		ownerOnline := b.Online && (presence == nil || presence.State != "away")
		dg.Members = append(dg.Members, dashboardMember{
			AgentID:         b.AgentID,
			ConvID:          ownerConv,
			Title:           b.Title,
			CreatedAt:       b.Created,
			Role:            "owner",
			ExternalSteward: db.IsExternalStewardConv(ownerConv),

			SpawnedByExternalSteward: gb.externalStewardParents[b.AgentID],

			agentLocationView:  b.Loc,
			repoLinksView:      ownerLinks,
			taskRefView:        gb.prs.taskRefFor(b.AgentID),
			tagsView:           gb.prs.tagsFor(b.AgentID),
			Online:             ownerOnline,
			FederationPresence: presence,
			Waking:             !b.Online && isConvWaking(ownerConv),
			Owner:              true,
			State:              b.State,
			Notify:             gb.notifyPrefs[ownerConv],
			NotifyEffective:    gb.notifyEffective(ownerConv),
			RouteHealth:        routeHealth,
		})
		// Pure-owners are reachable via this group too — surface
		// the group on the agent's row in the Agents view so
		// "what groups can this conv see?" matches reality. nil for a
		// retired/superseded owner (TCL-369).
		if a := gb.agents.add(ownerConv); a != nil {
			a.Groups = append(a.Groups, g.Name)
			a.OwnedGroups = append(a.OwnedGroups, g.Name)
		}
	}
	gb.gt.end(&gb.gt.ownerRows, ownerMark)
	// Default ordering: newest-first by creation time (the Age
	// column; the JS column sort treats this as the natural order it
	// falls back to). Mirrors handleGroupMembersList.
	sortMark := gb.gt.begin()
	sortMembersByAge(dg.Members,
		func(m dashboardMember) string { return m.CreatedAt },
		func(m dashboardMember) string { return m.ConvID })
	gb.gt.end(&gb.gt.sortMembers, sortMark)
	return dg
}

// registerSnapshotPermissions copies every conv's grants, tri-state
// overrides and decoded scopes into the permissions view, and registers
// each holder on the agent roster.
func registerSnapshotPermissions(perms *snapshotPermissionsView, p snapshotPreload, agents *snapshotAgentRows) {
	for convID, slugs := range p.allGrants {
		agents.add(convID)
		copySlice := append([]string{}, slugs...)
		sort.Strings(copySlice)
		perms.Grants[convID] = copySlice
	}
	// Full tri-state overrides (grant AND deny) for the editor modal.
	// A conv with only a deny override is still an agent — surface it.
	for convID, slugEffects := range p.allOverrides {
		agents.add(convID)
		copyMap := make(map[string]string, len(slugEffects))
		for slug, effect := range slugEffects {
			copyMap[slug] = effect
		}
		perms.Overrides[convID] = copyMap
	}
	// The scope each override carries, decoded. A row this build cannot decode
	// is reported separately rather than as unscoped: the gate refuses such a
	// row (evalPermissionScope), so showing it as unconditional would be the
	// widest possible reading of a grant that authorizes nothing.
	for convID, bySlug := range p.allOverrideScopes {
		for slug, raw := range bySlug {
			scope := permissionScopeFromJSON(raw)
			if len(scope) == 0 {
				if strings.TrimSpace(raw) != "" {
					agents.add(convID)
					perms.UnreadableScopes[convID] = append(
						perms.UnreadableScopes[convID], slug)
				}
				continue
			}
			agents.add(convID)
			if perms.Scopes == nil {
				perms.Scopes = map[string]map[string]PermissionScope{}
			}
			if perms.Scopes[convID] == nil {
				perms.Scopes[convID] = map[string]PermissionScope{}
			}
			perms.Scopes[convID][slug] = scope
		}
	}
}

// Bucket the active sudo grants (loaded up front) per stable agent id so the
// per-agent Active rendering is O(1) inside the output loop. The same rows
// feed the top-level Sudo[] for the dedicated tab.
func buildSnapshotSudo(grants []*db.SudoGrant, rc *snapshotRowCache) ([]dashboardSudoEntry, map[string][]dashboardSudoEntry) {
	sudoByAgent := map[string][]dashboardSudoEntry{}
	top := []dashboardSudoEntry{}
	sudoNow := time.Now()
	for _, g := range grants {
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
		top = append(top, topEntry)
		// On the per-agent slice we omit ConvID — the agent row's
		// own ConvID already identifies who holds the grant. Saves
		// bytes on agents with many grants and keeps the JSON
		// readable in browser devtools.
		rowEntry := topEntry
		rowEntry.ConvID = ""
		rowEntry.AgentID = "" // the agent row already identifies the holder
		sudoByAgent[g.AgentID] = append(sudoByAgent[g.AgentID], rowEntry)
	}
	return top, sudoByAgent
}

// finishSnapshotRoster turns the agent registry into the sorted Agents and
// Ungrouped lists: each live row gets its effective permissions (from the
// same routine the gate uses), sorted group lists and active sudo grants.
func finishSnapshotRoster(agents *snapshotAgentRows, perms snapshotPermissionsView,
	sudoByAgent map[string][]dashboardSudoEntry) (all, ungrouped []dashboardAgent) {
	all = []dashboardAgent{}
	ungrouped = []dashboardAgent{}
	for _, a := range agents.byConv {
		// Defensive: a retired conv must never appear on the roster,
		// even if a partially-applied retire left a stale group/grant
		// row that the passes above picked up.
		if agents.rc.inactiveActor(a.ConvID) {
			continue
		}
		// Effective comes from the same routine the permission GATE uses
		// (effectivePermsFor → resolvePermissionVerdict), so this column
		// cannot drift from what the daemon will actually allow. It used
		// to be a third hand-rolled union here, which omitted active sudo
		// elevations and the structural owner bypass.
		merged, _, _, _ := effectivePermsFor(permissionsState{
			Defaults:  perms.Defaults,
			Grants:    perms.Grants,
			Overrides: perms.Overrides,
		}, a.ConvID, ownerImpliedSlugsFor(a.ConvID))
		sort.Strings(merged)
		a.Effective = merged
		sort.Strings(a.Groups)
		sort.Strings(a.OwnedGroups)
		if rows := sudoByAgent[a.AgentID]; len(rows) > 0 {
			a.ActiveSudo = rows
		}
		all = append(all, *a)
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
			ungrouped = append(ungrouped, *a)
		}
	}
	sort.Slice(all, func(i, j int) bool {
		return all[i].Title < all[j].Title
	})
	sort.Slice(ungrouped, func(i, j int) bool {
		return ungrouped[i].Title < ungrouped[j].Title
	})
	return all, ungrouped
}
