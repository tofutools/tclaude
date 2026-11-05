package agentd

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// groupRetireResp is the response shape of the bulk groups.members.retire
// endpoint. It mirrors groupOpResp (so the CLI renders the per-member
// table identically to stop/resume) but carries an extra Warnings list
// — retire can leave a group ownerless when it demotes an owner, and
// the human needs to hear about that.
type groupRetireResp struct {
	Group   string           `json:"group"`
	Action  string           `json:"action"`
	Members []memberOpResult `json:"members"`
	// RhythmsDisabled is the number of group-target cron jobs (template-seeded
	// rhythms) auto-disabled because this retire left the group with no live
	// members (JOH-345). Omitted when zero — a partial retire, or one that left
	// live members behind, disables nothing.
	RhythmsDisabled int      `json:"rhythms_disabled,omitempty"`
	Warnings        []string `json:"warnings,omitempty"`
}

// groupHasLiveMembers reports whether groupID still has at least one member
// backed by a live (current-generation, active) agent conv. Placeholder members
// (no conv yet) and retired/superseded convs do not count. Used to decide
// whether a retire has emptied the group of live recipients.
func groupHasLiveMembers(groupID int64) (bool, error) {
	members, err := db.ListAgentGroupMembers(groupID)
	if err != nil {
		return false, err
	}
	for _, m := range members {
		if m.ConvID == "" {
			continue
		}
		live, err := db.IsLiveAgentConv(m.ConvID)
		if err != nil {
			return false, err
		}
		if live {
			return true, nil
		}
	}
	return false, nil
}

// disableGroupRhythmsIfEmptied disables the group's template-seeded rhythm cron
// jobs when a retire has left it with no live members (JOH-345). Non-destructive
// (the jobs stay visible + reversible in the Cron tab, marked 'group-retired'),
// and a later `groups resume` re-enables exactly these. Returns the number
// disabled (0 when live members remain). Best-effort tidy-up: a failure is
// logged and swallowed — the retire itself already succeeded, and a stray rhythm
// firing at a dormant group merely no-ops (fireCronGroupJob resolves an empty
// roster gracefully).
func disableGroupRhythmsIfEmptied(g *db.AgentGroup) int {
	live, err := groupHasLiveMembers(g.ID)
	if err != nil {
		slog.Warn("retire: could not check group liveness for rhythm cleanup",
			"group", g.Name, "err", err)
		return 0
	}
	if live {
		return 0
	}
	// Standing orders share the rhythms' auto-pause semantics, so an emptied
	// group must pause both. Only rows tclaude itself paused carry the marker,
	// so a hand-disabled order is untouched and a later resume restores exactly
	// what this paused.
	hookHarnesses := standingOrderHookHarnessesForGroupBestEffort(g.ID)
	if so, err := db.DisableGroupTargetStandingOrdersForRetire(g.ID); err != nil {
		slog.Warn("retire: could not disable group standing orders",
			"group", g.Name, "err", err)
	} else if so > 0 {
		slog.Info("retire emptied group — disabled its standing orders",
			"group", g.Name, "disabled", so)
		if warning := reconcileStandingOrderHookHarnesses(hookHarnesses); warning != "" {
			slog.Warn("retire: standing-order hook reconciliation failed",
				"group", g.Name, "warning", warning)
		}
	}
	n, err := db.DisableGroupTargetCronJobsForRetire(g.ID)
	if err != nil {
		slog.Warn("retire: could not disable group rhythms",
			"group", g.Name, "err", err)
		return 0
	}
	if n > 0 {
		slog.Info("retire emptied group — disabled its rhythms",
			"group", g.Name, "disabled", n)
	}
	return n
}

// handleGroupRetire retires the active-agent members of the group in
// one shot — the bulk parallel of `agent retire`, completing the
// groups.members.stop / groups.members.resume lifecycle family (which until now had no
// retire sibling). It is the SO_PEERCRED /v1 surface; the cookie-authed
// dashboard route (dashboardGroupRetire) shares the same core.
//
// "Retire" demotes an agent to a plain conversation: retireAgentConv
// drops every group membership (this group and any others the member
// belongs to), revokes every permission and sudo grant, and flips the
// enrollment bit. The conversation itself — .jsonl, history, conv_index
// row — is left completely intact and reinstatable; this is the
// non-destructive bulk cleanup, never `agent delete`. Unless
// ?shutdown=0, a retired member's running tmux pane is also soft-exited
// (stopOneConv, soft only — never a force-kill), since a retired
// agent's idle process is almost never wanted.
//
// ?status= optionally restricts the cohort to members of a given live
// status (e.g. status=idle, status=offline, or a comma list) — the
// "retire idle agents in <group>" palette command. Absent / "all" =
// every member, the legacy behaviour. See parseRetireStatusFilter.
//
// ?delete_worktree=1 additionally removes each retired member's git
// worktree and force-deletes its branch — the bulk parallel of the
// single-agent retire option. It defaults OFF (the failsafe in
// retireShouldDeleteWorktree); the same safety rules apply per member
// (the main repo and worktrees shared with a surviving agent are kept,
// removal waits until the member's pane exits).
//
// Permission: groups.members.retire (not in the global defaults — retiring
// agents is a sensitive cleanup the human normally drives; the slug
// delegates it to a trusted coordinator). Gated with
// requireGroupPermission, like the other bulk group endpoints
// (stop/resume/spawn): owning THIS group raises the slug by default
// (the owner-state bypass), so an owner can run its own team's
// lifecycle without an explicit grant. The bypass fills only the
// permUndecided gap — an explicit deny override is always
// authoritative and suppresses it.
func handleGroupRetire(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	filter, ferr := parseRetireStatusFilter(r.URL.Query().Get("status"))
	if ferr != nil {
		writeError(w, http.StatusBadRequest, "status", ferr.Error())
		return
	}
	var selected map[string]struct{}
	var affected []string
	if filter != nil {
		affected = []string{}
		members, err := db.ListAgentGroupMembers(g.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		alive, _ := session.LiveTmuxSessions()
		selected = map[string]struct{}{}
		for _, member := range members {
			online, status := convLiveStatus(member.ConvID, alive)
			if filter.matchesConv(member.ConvID, online, status) {
				selected[member.ConvID] = struct{}{}
				affected = append(affected, member.ConvID)
			}
		}
	}
	ctx := ActionContext{}
	if filter != nil {
		ctx.affectedConvs = affected
	}
	caller, ok := requireGroupPermission(w, r, PermGroupsMembersRetire, g, ctx)
	if !ok {
		return
	}
	out, err := bulkRetireGroupMembers(g, caller,
		strings.TrimSpace(r.URL.Query().Get("reason")),
		retireShouldShutdown(r), retireShouldDeleteWorktree(r), nil, selected,
		auditRequestEventID(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	// If this retire left the group with no live members, disable its
	// template-seeded rhythms so they stop firing to nobody (JOH-345).
	out.RhythmsDisabled = disableGroupRhythmsIfEmptied(g)
	writeJSON(w, http.StatusOK, out)
}

// bulkRetireGroupConcurrency bounds how many members the bulk retire
// works on at once. Retire is I/O-bound per member (a .jsonl title read,
// the SQLite demotion writes, a tmux soft-exit), so a handful of workers
// overlaps that latency without stampeding tmux or the single SQLite
// writer (WAL serialises writes; busy_timeout absorbs contention).
const bulkRetireGroupConcurrency = 8

// bulkRetireGroupMembers is the shared core behind both retire surfaces:
// the SO_PEERCRED /v1/groups/{name}/retire endpoint (agent callers,
// slug-gated via handleGroupRetire) and the cookie-authed
// /api/groups/{name}/retire dashboard route (the human, via
// dashboardGroupRetire). It retires every member of g that passes the
// status filter and returns the per-member table plus any
// ownerless-group warnings.
//
// caller is the requester's own conv ("" for the human): it is always
// skipped (skipped:self), since the brief is "retire OTHER agents in the
// group" and an agent demoting itself mid-request would revoke its own
// grants and /exit its own pane out from under the request it is
// serving.
//
// Cohort selection is one of two mutually-exclusive mechanisms:
//   - selected != nil — an EXPLICIT set of conv-ids: retire exactly the
//     members whose conv-id is in the set, regardless of their current
//     live status. This is the dashboard preview path — the human ticked
//     a list, and the BE must retire precisely that list and nothing it
//     re-derived (so an agent that flips status between preview and
//     submit is still retired iff it was on the previewed list). A member
//     not in the set is omitted from the response; a conv in the set that
//     is not (or no longer) a member of g is simply never reached, so it
//     is silently ignored — the membership table is authoritative, the
//     set only narrows it.
//   - selected == nil — the status FILTER path: filter==nil retires every
//     member (the legacy behaviour); a non-nil filter restricts the
//     cohort to members whose live status matches, re-resolved server-side
//     from live tmux. Non-matching members are omitted from the response.
//
// When selected is non-nil the status filter is ignored entirely (the
// human's explicit pick wins — there is nothing to re-resolve).
//
// deleteWorktree (the batch parallel of the single-agent retire option)
// additionally removes each retired member's git worktree and force-
// deletes its branch. It is per-member and reuses the single-agent
// machinery (resolveRetireWorktree before the shutdown, then
// scheduleRetireWorktreeCleanup), so the same safety rules hold: the main
// repo and worktrees a SURVIVING agent still works in are kept, and the
// removal waits until the member's pane exits (its cwd is the worktree).
// A worktree shared by two members BOTH retired in this batch is
// conservatively kept: every member's view is resolved from the same
// pre-mutation cohort snapshot, while all co-sharers are still active. The
// safe failure mode — never a yank from under a sibling whose pane is still
// draining. The per-member outcome rides back in memberOpResult.Worktree.
//
// Per-member outcomes (memberOpResult.Action):
//   - retired                  — demoted (Detail summarises what changed)
//   - skipped:self             — the caller's own conv; never self-retire
//   - skipped:no_conv_id       — a placeholder member with no conv yet
//   - skipped:not_active_agent — already retired / never an agent
//   - error                    — the retire failed (Detail has the cause)
//
// The per-member work runs in parallel, bounded by
// bulkRetireGroupConcurrency. Each worker writes its result and the
// owner-groups it touched into its own pre-sized slot, so there is no
// contended shared state; the ownerless set is merged sequentially once
// every worker has settled — checked once at the end so a bulk retire
// that demotes a member-owner warns about the now-ownerless group,
// matching the single-agent cleanup path.
func bulkRetireGroupMembers(g *db.AgentGroup, caller, reason string, shutdown, deleteWorktree bool, filter retireStatusFilter, selected map[string]struct{}, relatedEventID string) (groupRetireResp, error) {
	members, err := db.ListAgentGroupMembers(g.ID)
	if err != nil {
		return groupRetireResp{}, err
	}
	by := enrollmentActor(caller)

	// Normalize an explicit selection to canonical conv-ids. The group preview
	// deliberately submits canonical conv_id values, but a selector may also be
	// an agt_ id, a live conv-id, or a UUID-shaped reference to a dangling
	// agent. resolveCleanupConv maps agt_/conv to the conv-id the member
	// universe (m.ConvID) is keyed on, and KEEPS a raw UUID-shaped fallback
	// so a dangling agent — actor row broken/unresolvable — stays retirable
	// by its conv-id (the recovery escape hatch D2's cold review pinned,
	// PR #628). An entry that resolves to nothing AND isn't UUID-shaped is
	// dropped: it can match no member, and the explicit set only ever
	// NARROWS the authoritative membership table (never widens it). Runs
	// only on the dashboard's explicit-selection path; the /v1 status-filter
	// path passes selected==nil and is untouched.
	if selected != nil {
		canon := make(map[string]struct{}, len(selected))
		for sel := range selected {
			if convID, ok := resolveCleanupConv(sel); ok {
				canon[convID] = struct{}{}
			}
		}
		selected = canon
	}

	// The status filter needs live tmux state; fetch it once
	// (snapshot-shaped) and share the read-only map across workers.
	// Skipped entirely when no filter is active OR an explicit selection
	// is supplied (the explicit path never consults live status), so the
	// legacy "retire everyone" path and the preview path keep their cost.
	var alive map[string]struct{}
	if filter != nil && selected == nil {
		alive, _ = session.LiveTmuxSessions()
	}

	// Snapshot every member's worktree view before any worker demotes or stops
	// anyone. The workers run concurrently; resolving inside a worker would
	// make a co-shared worktree's safety decision depend on whether its sibling
	// had already retired/exited. Pre-resolution keeps the whole batch on one
	// stable pre-mutation view and preserves the conservative co-share rule.
	retireWorktrees := map[string]agentWorktreeView{}
	if deleteWorktree {
		claimSnapshot := captureAgentWorktreeClaims()
		for _, m := range members {
			if m.ConvID != "" {
				retireWorktrees[m.ConvID] = claimSnapshot.resolve(
					m.ConvID, map[string]bool{m.ConvID: true})
			}
		}
	}

	results := make([]*memberOpResult, len(members))
	ownerGroupsPer := make([][]int64, len(members))

	forEachAgentConcurrently(members, bulkRetireGroupConcurrency, func(i int, m *db.AgentGroupMember) {
		res := memberOpResult{AgentID: peerAgentID(m.ConvID), ConvID: m.ConvID, Title: agent.FreshTitle(m.ConvID)}
		switch {
		case m.ConvID == "":
			res.Action = "skipped:no_conv_id"
			res.Detail = "placeholder member (no conv yet)"
		case caller != "" && sameActor(m.ConvID, caller):
			// Match on the stable actor (JOH-323): the caller never
			// retires itself, including a predecessor generation of
			// itself that still sits in the roster.
			res.Action = "skipped:self"
			res.Detail = "the caller never retires itself"
		default:
			switch {
			case selected != nil:
				if _, ok := selected[m.ConvID]; !ok {
					return // not in the explicit selection — omit
				}
			case filter != nil:
				online, status := convLiveStatus(m.ConvID, alive)
				if !filter.matchesConv(m.ConvID, online, status) {
					return // filtered out — omit from the response
				}
			}
			res, ownerGroupsPer[i] = retireGroupMember(
				m.ConvID, by, reason, shutdown, deleteWorktree, retireWorktrees[m.ConvID], res, relatedEventID)
		}
		results[i] = &res
	})

	out := groupRetireResp{Group: g.Name, Action: "retire", Members: []memberOpResult{}}
	ownerless := map[int64]bool{}
	for i := range members {
		if results[i] != nil {
			out.Members = append(out.Members, *results[i])
		}
		for _, gid := range ownerGroupsPer[i] {
			ownerless[gid] = true
		}
	}
	out.Warnings = warnOwnerlessGroups(ownerless)
	return out, nil
}

// retireGroupMember retires one member as part of the bulk retire. It
// enforces the "active agent only" guard (a no-op on a conv that was
// never an agent or is already retired comes back as
// skipped:not_active_agent), runs the shared retireAgentConv demotion,
// and — when shutdown is requested — soft-exits the member's pane.
// Returns the populated result plus the ids of any groups whose owner
// roster the demotion touched (for the caller's ownerless-warning
// merge); res arrives pre-seeded with ConvID + Title so the table stays
// consistent across every branch.
//
// When deleteWorktree is set the member's git worktree+branch is also
// cleaned up, reusing the single-agent retire machinery: the worktree is
// resolved for the whole cohort BEFORE any worker starts — defensive ordering
// that keeps shared-worktree decisions stable across concurrent demotion and
// shutdown. scheduleRetireWorktreeCleanup then runs it — inline when the
// member is already offline, deferred to a waiter when a /exit is in flight,
// kept when no shutdown was asked for. The per-member plan rides back on
// res.Worktree, and its one-line note is folded into Detail so the CLI/table
// row says what happened.
func retireGroupMember(convID, by, reason string, shutdown, deleteWorktree bool, wt agentWorktreeView, res memberOpResult, relatedEventID string) (memberOpResult, []int64) {
	// Gate on the LIVE generation (current conv of an active actor), not just
	// "active": retire acts on the actor, so a superseded predecessor handle
	// would demote the live agent. Members always come through as the current
	// generation, so this is a defensive guard for the invariant.
	live, serr := db.IsLiveAgentConv(convID)
	if serr != nil {
		res.Action = "error"
		res.Detail = "agent-state lookup: " + serr.Error()
		return res, nil
	}
	if !live && !db.AgentConvAway(convID) {
		state, _ := db.AgentState(convID)
		res.Action = "skipped:not_active_agent"
		res.Detail = "state: " + state
		return res, nil
	}
	outcome, ownerGroups, rerr := retireAgentConv(convID, by, reason)
	if rerr != nil {
		res.Action = "error"
		res.Detail = rerr.Error()
		return res, nil
	}
	res.Action = "retired"
	res.Detail = summarizeRetireOutcome(outcome)

	td := finishRetiredConv(convID, shutdown, deleteWorktree, wt, relatedEventID)
	res.TmuxSes = td.Stop.TmuxSes
	res.Worktree = td.Worktree
	for _, note := range td.Notes {
		res.Detail = joinDetail(res.Detail, note)
	}
	return res, ownerGroups
}

// retireTeardown is what the post-demotion half of a retire did: the stop
// result (zero-valued when no shutdown was asked for), the worktree plan (nil
// when ?delete_worktree was off) and the human-readable notes each step
// produced, in order.
type retireTeardown struct {
	StopOutcome softExitOutcome
	Stop        memberOpResult
	Worktree    *retireWorktreePlan
	Notes       []string
}

// finishRetiredConv is THE post-demotion half of a retire, shared by all three
// retire surfaces: the single-agent /v1/agent/{selector}/retire endpoint, the
// bulk group retire (retireGroupMember) and the dashboard's fleet cleanup tier.
// Each of those used to inline its own slightly-different copy of the same
// three steps, which is how they drifted apart on which stop primitive they
// used and which notes they surfaced.
//
// The order is load-bearing:
//
//  1. Stop the pane, and WAIT for the process to actually be gone. Waiting is
//     the default for every stop that has work behind it: the response then
//     means what it says, and steps 2 and 3 get to do their job inline instead
//     of promising it. The bulk surfaces make this affordable by running their
//     members concurrently, so the cost is the slowest agent rather than the
//     sum.
//  2. Agent-owned directory cleanup.
//  3. Worktree + branch cleanup, from the view the CALLER resolved before any
//     demotion or shutdown (wt) — resolving it here would read a world the
//     stop above already changed.
//
// Both cleanups keep their own deferred fallback for the case the wait cannot
// resolve: a pane that survives the whole escalation ladder (softExitStuck) is
// still alive when they run, so they schedule themselves behind a background
// exit-waiter and report to the human via the Messages tab if that promise
// cannot be kept. That is why retire does NOT use stopBeforePurge's refusal —
// unlike a purge, it has somewhere safe to land.
//
// The demotion itself (retireAgentConv) stays with the caller: only it knows
// the precondition to enforce (live-generation guard, require_offline) and the
// actor to attribute the change to.
func finishRetiredConv(convID string, shutdown, deleteWorktree bool, wt agentWorktreeView, relatedEventID string) retireTeardown {
	var td retireTeardown
	if shutdown {
		td.Stop, td.StopOutcome = stopOneConvAndWait(convID, false /* soft exit */, db.AgentExitActionRetire, relatedEventID, 0)
		switch td.Stop.Action {
		case "soft_stopped":
			// Harness-agnostic wording on purpose: the group-retire copy of this
			// used to say "/exit sent", which is simply untrue for a Codex pane
			// (/quit) or an OpenCode one (a managed TUI call, no keystroke).
			note := "session soft-stopped"
			if td.Stop.Detail != "" {
				note += " (" + td.Stop.Detail + ")"
			}
			td.Notes = append(td.Notes, note)
		case "error":
			td.Notes = append(td.Notes, "session shutdown failed: "+td.Stop.Detail)
		}
	}
	if !db.AgentConvAway(convID) {
		cleanupAgentDirectoriesAfterRetire(convID, shutdown)
		cleanupRetiredCodexNativeProfiles(convID)
	}
	if deleteWorktree {
		plan := scheduleRetireWorktreeCleanup(convID, wt, shutdown)
		td.Worktree = &plan
		// Every plan gets its note, "none" ("no worktree") included: the note
		// only ever appears when the caller explicitly asked for
		// delete_worktree, and there "this one had nothing to delete" is an
		// answer, not noise. Keeps the group-retire member table byte-identical
		// to what it produced before the three copies were merged.
		if plan.Detail != "" {
			td.Notes = append(td.Notes, plan.Detail)
		}
	}
	return td
}

// retireStatusFilter is the optional ?status= filter for bulk retire.
// nil = match every member (the legacy "retire everyone" behaviour). A
// non-nil set restricts the retire to members whose live status
// normalizes to one of its tokens:
//
//   - "offline"  → no live tmux session (the pane is dead)
//   - "idle"     → online, last hook status == idle
//   - "working"  → online, working
//   - "awaiting" → online, awaiting_permission OR awaiting_input
//   - "error"    → online, error
//
// The dashboard palette uses "idle" and "offline"; the rest fall out of
// the same normalization for free and are reachable via the CLI
// --status flag.
type retireStatusFilter map[string]bool

// validRetireStatuses is the closed vocabulary of ?status= tokens — the
// outputs of normalizeMemberStatus. Kept in sync with that switch: an
// unknown token is rejected rather than silently matching nobody.
var validRetireStatuses = map[string]bool{
	"offline": true, "idle": true, "working": true, "awaiting": true, "error": true,
}

// parseRetireStatusFilter reads the ?status= query value into a filter.
// Empty / absent / "all" yield a nil filter (match everything). Tokens
// are comma-separated, lower-cased and trimmed. An unknown token is an
// error, not a silent no-op: without this a typo (?status=offlien) would
// match nobody and return 200 with an empty member list, indistinguish-
// able from "the group has no offline agents". Callers surface it as 400.
func parseRetireStatusFilter(raw string) (retireStatusFilter, error) {
	raw = strings.TrimSpace(strings.ToLower(raw))
	if raw == "" || raw == "all" {
		return nil, nil
	}
	set := retireStatusFilter{}
	for tok := range strings.SplitSeq(raw, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		if tok == "all" {
			return nil, nil // "all" anywhere in the list = no filter
		}
		if !validRetireStatuses[tok] {
			return nil, fmt.Errorf("unknown status %q (valid: all, offline, idle, working, awaiting, error)", tok)
		}
		set[tok] = true
	}
	if len(set) == 0 {
		return nil, nil
	}
	return set, nil
}

// matches reports whether a member with the given liveness + hook status
// passes the filter. A nil filter matches everything.
func (f retireStatusFilter) matches(online bool, status string) bool {
	if f == nil {
		return true
	}
	return f[normalizeMemberStatus(online, status)]
}

// matchesConv is matches for a real member. An external steward runs outside
// tclaude, so it has no status to filter on: it always looks offline, and a
// "retire offline" tidy-up would end its credential. A status filter never
// selects one; retiring it stays a deliberate act that names it.
func (f retireStatusFilter) matchesConv(convID string, online bool, status string) bool {
	if f == nil {
		return true
	}
	return f.matches(online, status) && !db.IsExternalStewardConv(convID)
}

// normalizeMemberStatus folds a member's (online, hook-status) pair into
// the single token the retire filter keys on — the SAME mapping the
// dashboard snapshot renders, so a "retire idle agents" palette command
// retires exactly the rows the human sees marked idle. An offline member
// (no live session) is "offline" regardless of its frozen hook status;
// an online member reports its hook status, with the two awaiting_*
// variants collapsed to "awaiting".
func normalizeMemberStatus(online bool, status string) string {
	if !online {
		return "offline"
	}
	switch status {
	case session.StatusAwaitingPermission, session.StatusAwaitingInput:
		return "awaiting"
	default:
		return status
	}
}

// convLiveStatus resolves a conv's (online, hook-status) from the
// pre-fetched alive set — the snapshot-shaped twin of isConvOnlineIn /
// stateForConvIn used by the retire status filter. online is true when
// any of the conv's session rows names a live tmux session; status is
// that live row's hook status (empty for an offline conv).
func convLiveStatus(convID string, alive map[string]struct{}) (bool, string) {
	rows, err := db.FindSessionsByConvID(convID)
	if err != nil {
		return false, ""
	}
	for _, r := range rows {
		if r.TmuxSession == "" {
			continue
		}
		if _, ok := alive[r.TmuxSession]; ok {
			return true, r.Status
		}
	}
	return false, ""
}

// summarizeRetireOutcome renders the parts of a retireConvOutcome the
// bulk table cares about into a compact, human-readable Detail cell:
// how many groups the member left and how many grants were revoked. An
// outcome that changed nothing beyond the enrollment bit yields "".
func summarizeRetireOutcome(o retireConvOutcome) string {
	var parts []string
	if n := len(o.GroupsLeft); n > 0 {
		parts = append(parts, fmt.Sprintf("left %d group(s)", n))
	}
	if revoked := o.PermsRevoked + o.SudoRevoked; revoked > 0 {
		parts = append(parts, fmt.Sprintf("revoked %d grant(s)", revoked))
	}
	return strings.Join(parts, ", ")
}

// joinDetail appends extra to a Detail string with ", " glue, treating
// an empty base as "no prefix".
func joinDetail(base, extra string) string {
	if base == "" {
		return extra
	}
	return base + ", " + extra
}
