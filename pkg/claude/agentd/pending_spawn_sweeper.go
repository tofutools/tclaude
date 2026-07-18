package agentd

import (
	"database/sql"
	"errors"
	"log/slog"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// pendingSpawnSweepInterval is how often the sweeper checks pending_spawns
// for a conv-id that has finally materialised. A few seconds keeps the
// pending→enrolled latency low — the operator wants a gated agent to "click
// into place" soon after they clear its startup modal — without the scan
// being costly: the table is normally empty, and each non-empty row costs one
// cheap session-row lookup.
const pendingSpawnSweepInterval = 5 * time.Second

// pendingSpawnLaunchGrace bounds protection for a reservation whose session
// wrapper never created a row. Normal launches clear Launching as soon as the
// row appears; a crashed daemon/launcher cannot leave the reservation forever.
const pendingSpawnLaunchGrace = 30 * time.Second

// startPendingSpawnSweeper runs the pending-spawn back-fill sweeper in its
// own goroutine until stop closes (the daemon-wide quit channel). JOH-205
// inc2: a non-blocking dashboard spawn whose conv-id had not materialised
// within the inline grace is recorded in pending_spawns; once its first turn
// lands and the conv-id appears, this sweep finishes the enrollment and drops
// the row.
//
// Restart-safe — the durable pending_spawns table is the whole state, so a
// daemon that restarts mid-pending resumes from it. The first sweep fires
// immediately so a restart picks up ready rows without waiting a full
// interval.
func startPendingSpawnSweeper(stop <-chan struct{}) {
	go func() {
		sweepPendingSpawns()
		t := time.NewTicker(pendingSpawnSweepInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				sweepPendingSpawns()
			}
		}
	}()
}

// sweepPendingSpawns is one sweep over every pending_spawns row.
func sweepPendingSpawns() {
	pending, err := db.ListPendingSpawns()
	if err != nil {
		slog.Warn("pending-spawn sweeper: list failed", "error", err)
		return
	}
	for _, ps := range pending {
		sweepOnePendingSpawn(ps)
	}
}

// sweepOnePendingSpawn resolves one pending spawn's conv-id and, when it has
// materialised, completes the enrollment via the shared finishSpawnEnrollment
// tail and deletes the row. A row whose session died before taking a turn (so
// its conv-id can never appear) is dropped; a row still waiting behind a
// startup gate is left for a later sweep. Every terminal outcome removes the
// row; every transient error leaves it for retry.
func sweepOnePendingSpawn(ps *db.PendingSpawn) {
	// The spawn label is the session-row id; the first-turn hook writes the
	// conv-id onto that row (keyed by the spawned process's
	// TCLAUDE_SESSION_ID=label). LoadSession surfaces a missing row as
	// sql.ErrNoRows (it never returns nil, nil), so that terminal case must be
	// separated from genuinely transient DB errors here — treating it as
	// transient made an orphaned pending row (wrapper died before
	// SaveSessionState, so the session row never existed) warn-loop forever.
	sess, err := db.LoadSession(ps.Label)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		slog.Warn("pending-spawn sweeper: load session failed",
			"label", ps.Label, "error", err)
		return // transient — retry next tick
	}
	if sess == nil {
		if ps.Launching {
			if created, parseErr := time.Parse(time.RFC3339Nano, ps.CreatedAt); parseErr == nil && time.Since(created) < pendingSpawnLaunchGrace {
				return
			}
		}
		// The session row is gone (deleted, or never created because the
		// launch wrapper died before writing it) — the spawn can never
		// enroll. Drop the orphaned pending row so the table doesn't leak.
		slog.Info("pending-spawn sweeper: session row gone; dropping pending spawn",
			"label", ps.Label)
		deletePendingSpawnRow(ps.Label)
		return
	}
	if ps.Launching {
		if err := db.MarkPendingSpawnLaunched(ps.Label); err != nil {
			slog.Warn("pending-spawn sweeper: failed to clear launch marker",
				"label", ps.Label, "error", err)
			return // transient — retry next tick
		}
		ps.Launching = false
	}

	// The first-turn hook is the authoritative, per-spawn conv-id signal: it
	// writes the conv-id onto THIS session row, keyed by the spawned process's
	// TCLAUDE_SESSION_ID=label. We deliberately do NOT fall back to conv-store
	// discovery here. Discovery resolves by cwd + launch-time with no
	// label→conv-id linkage, so with two gated spawns sharing one cwd it could
	// grab the wrong conv and collapse two agents into one — and a pending row
	// can linger for minutes (until a human clears the gate), so that window is
	// wide open. The exact hook path suffices: a pending spawn is the delayed
	// case by definition, and by the time its gate clears and it takes its
	// first turn, the hook has written the conv-id. (executeSpawn's inline poll
	// keeps discovery as its fast path for a trusted-dir Codex, where the race
	// window is seconds, not minutes.)
	convID := sess.ConvID
	if convID == "" {
		// No conv-id yet. If the pane has exited it will never take its first
		// turn — drop the dead row; otherwise leave it for the operator to
		// clear the startup gate, which may take a while.
		if sess.Status == session.StatusExited {
			slog.Info("pending-spawn sweeper: session exited before conv-id; dropping pending spawn",
				"label", ps.Label)
			deletePendingSpawnRow(ps.Label)
		}
		return
	}

	// Conv-id materialised. Resolve the target group.
	g, err := db.GetAgentGroupByID(ps.GroupID)
	if err != nil {
		slog.Warn("pending-spawn sweeper: load group failed",
			"label", ps.Label, "group_id", ps.GroupID, "error", err)
		return // transient — retry
	}
	if g == nil {
		// The group was deleted while the spawn waited — nothing to enroll
		// into. Drop the row.
		slog.Warn("pending-spawn sweeper: group gone; dropping pending spawn",
			"label", ps.Label, "group_id", ps.GroupID)
		deletePendingSpawnRow(ps.Label)
		return
	}

	// Atomically replace the durable reservation with the exact actor binding.
	// This closes the claim→enroll gap where a hook/reaper could otherwise mint
	// a competing identity after the pending row disappeared.
	claimed, err := db.ClaimPendingSpawnAndBindAgent(ps.Label, convID, ps.AgentID, "spawn")
	if err != nil {
		slog.Warn("pending-spawn sweeper: claim failed",
			"label", ps.Label, "conv", convID, "error", err)
		return // transient — retry
	}
	if !claimed {
		return // another back-fill path claimed it
	}

	// Idempotency: a prior enrollment may have completed its membership and
	// one-shot welcome but failed before clearing the pending row. The atomic
	// claim above has now cleared it; do not deliver the welcome twice.
	if m, err := db.FindMemberInGroup(g.ID, convID); err != nil {
		slog.Warn("pending-spawn sweeper: membership check failed",
			"label", ps.Label, "conv", convID, "error", err)
		requeuePendingSpawn(ps.Label, ps)
		return
	} else if m != nil {
		// A prior attempt got past the membership write but may have failed
		// before the task-ref write — repair the link before dropping the row
		// so the requested binding isn't lost with it.
		ensurePendingTaskRefBound(convID, ps)
		slog.Info("pending-spawn sweeper: already enrolled; cleared pending row",
			"label", ps.Label, "conv", convID)
		return
	}

	// Reconstruct the spawnParams subset finishSpawnEnrollment consumes from
	// the persisted intent, then back-fill the enrollment. This runs the same
	// membership add + pending-name + inbox briefing + post-init (/rename +
	// welcome) the inline path runs — and, because the conv-id now exists, it
	// only send-keys to a Codex pane that has cleared its startup gates.
	p := spawnParams{
		AgentID:          ps.AgentID,
		EffectiveSandbox: ps.EffectiveSandbox,
		Role:             ps.Role,
		Descr:            ps.Descr,
		Name:             ps.Name,
		InitialMessage:   ps.InitialMessage,
		GroupContext:     ps.GroupContext,
		ReplyToConv:      ps.ReplyToConv,
		SpawnedByConv:    ps.SpawnedByConv,
		// The durable agent_id companions (JOH-321 F2): minutes have passed since
		// the spawn was recorded, so the spawner may have rotated. These let
		// finishSpawnEnrollment re-resolve its LIVE generation for the briefing
		// reply-target + welcome attribution rather than the stale recorded conv.
		ReplyToAgent:   ps.ReplyToAgent,
		SpawnedByAgent: ps.SpawnedByAgent,
		WorktreePath:   ps.WorktreePath,
		WorktreeBranch: ps.WorktreeBranch,
		// The per-agent task-reference link the spawn requested (TCL-568).
		// Persisted on the pending row precisely so this delayed-
		// materialization path binds it like the inline paths do; dropping
		// it here was how `spawn --task` lost the link whenever enrollment
		// went through the sweeper. (SpawnConfigJSON is still not carried —
		// it is a best-effort audit snapshot, not caller-visible state.)
		// Birth-time access controls: the sweeper applies the same
		// owner grant + permission overrides the inline paths do, now that the
		// conv-id exists. enrollSpawnedConv reads these off spawnParams.
		TaskURL:             sweeperTaskRefURL(ps),
		TaskLabel:           ps.TaskLabel,
		IsOwner:             ps.IsOwner,
		PermissionOverrides: ps.PermissionOverrides,
		ProcessCommandID:    ps.ProcessCommandID,
	}
	if fail := finishSpawnEnrollment(g, p, convID); fail != nil {
		if err := db.InsertPendingSpawn(ps); err != nil {
			slog.Warn("pending-spawn sweeper: enrollment failed and requeue failed",
				"label", ps.Label, "conv", convID, "enroll_error", fail.Msg, "requeue_error", err)
			return
		}
		slog.Warn("pending-spawn sweeper: enrollment failed; will retry",
			"label", ps.Label, "conv", convID, "error", fail.Msg)
		return // leave the row; retry next tick
	}
	slog.Info("pending-spawn sweeper: enrolled pending spawn",
		"label", ps.Label, "conv", convID, "group", g.Name)
}

// sweeperTaskRefURL returns a pending row's task-reference URL, re-applying
// the write-path scheme guard the spawn boundary enforced when the row was
// written. The row is trusted state, so a failure here is defence in depth
// (every path into agents.task_ref_url stays validated); it logs and drops
// the link rather than wedging the enrollment.
func sweeperTaskRefURL(ps *db.PendingSpawn) string {
	if ps.TaskURL == "" {
		return ""
	}
	if err := validateTaskRefURL(ps.TaskURL); err != nil {
		slog.Warn("pending-spawn sweeper: dropping invalid task-reference link",
			"label", ps.Label, "error", err)
		return ""
	}
	return ps.TaskURL
}

// ensurePendingTaskRefBound re-applies a pending spawn's task-reference link
// on the "already enrolled" idempotency path: a prior enrollment attempt may
// have committed the membership and then failed before its task-ref write,
// and re-running the full enrollment would double the one-shot welcome. Set
// only when the agent currently has NO link, so a link the operator has since
// set or edited is never clobbered. Best-effort: the agent is enrolled and
// the row is being dropped either way, so a failure here only logs.
func ensurePendingTaskRefBound(convID string, ps *db.PendingSpawn) {
	taskURL := sweeperTaskRefURL(ps)
	if taskURL == "" {
		return
	}
	agentID, err := db.AgentIDForConv(convID)
	if err != nil || agentID == "" {
		slog.Warn("pending-spawn sweeper: cannot repair task-reference link (no actor)",
			"label", ps.Label, "conv", convID, "error", err)
		return
	}
	ref, err := db.GetAgentTaskRef(agentID)
	if err != nil {
		slog.Warn("pending-spawn sweeper: task-reference repair read failed",
			"label", ps.Label, "agent", agentID, "error", err)
		return
	}
	if ref.URL != "" {
		return // already bound (or since edited) — leave it alone
	}
	if _, err := db.SetAgentTaskRef(agentID, taskURL, ps.TaskLabel); err != nil {
		slog.Warn("pending-spawn sweeper: task-reference repair write failed",
			"label", ps.Label, "agent", agentID, "error", err)
		return
	}
	slog.Info("pending-spawn sweeper: repaired task-reference link on already-enrolled spawn",
		"label", ps.Label, "agent", agentID)
}

// deletePendingSpawnRow removes a pending row, logging (not bubbling) a
// failure — a row left behind is simply re-processed next tick, which the
// membership idempotency guard above makes safe.
func deletePendingSpawnRow(label string) {
	if err := db.DeletePendingSpawn(label); err != nil {
		slog.Warn("pending-spawn sweeper: delete pending row failed",
			"label", label, "error", err)
	}
}
