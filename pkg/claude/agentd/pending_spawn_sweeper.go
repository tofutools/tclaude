package agentd

import (
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
	// TCLAUDE_SESSION_ID=label).
	sess, err := db.LoadSession(ps.Label)
	if err != nil {
		slog.Warn("pending-spawn sweeper: load session failed",
			"label", ps.Label, "error", err)
		return // transient — retry next tick
	}
	if sess == nil {
		// The session row is gone — the spawn can never enroll. Drop the
		// orphaned pending row so the table doesn't leak.
		slog.Info("pending-spawn sweeper: session row gone; dropping pending spawn",
			"label", ps.Label)
		deletePendingSpawnRow(ps.Label)
		return
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

	// Idempotency: a prior sweep may have enrolled this conv but failed to
	// delete the pending row. If the conv is already a member, the enrollment
	// — and its one-shot /rename + welcome injection — already ran; don't run
	// it again, just clear the leftover row.
	if m, err := db.FindMemberInGroup(g.ID, convID); err != nil {
		slog.Warn("pending-spawn sweeper: membership check failed",
			"label", ps.Label, "conv", convID, "error", err)
		return // transient — retry
	} else if m != nil {
		slog.Info("pending-spawn sweeper: already enrolled; clearing pending row",
			"label", ps.Label, "conv", convID)
		deletePendingSpawnRow(ps.Label)
		return
	}

	claimed, err := db.ClaimPendingSpawn(ps.Label)
	if err != nil {
		slog.Warn("pending-spawn sweeper: claim failed",
			"label", ps.Label, "conv", convID, "error", err)
		return // transient — retry
	}
	if !claimed {
		return // another back-fill path claimed it
	}

	// Reconstruct the spawnParams subset finishSpawnEnrollment consumes from
	// the persisted intent, then back-fill the enrollment. This runs the same
	// membership add + pending-name + inbox briefing + post-init (/rename +
	// welcome) the inline path runs — and, because the conv-id now exists, it
	// only send-keys to a Codex pane that has cleared its startup gates.
	p := spawnParams{
		Role:           ps.Role,
		Descr:          ps.Descr,
		Name:           ps.Name,
		InitialMessage: ps.InitialMessage,
		GroupContext:   ps.GroupContext,
		ReplyToConv:    ps.ReplyToConv,
		SpawnedByConv:  ps.SpawnedByConv,
		// The durable agent_id companions (JOH-321 F2): minutes have passed since
		// the spawn was recorded, so the spawner may have rotated. These let
		// finishSpawnEnrollment re-resolve its LIVE generation for the briefing
		// reply-target + welcome attribution rather than the stale recorded conv.
		ReplyToAgent:   ps.ReplyToAgent,
		SpawnedByAgent: ps.SpawnedByAgent,
		WorktreePath:   ps.WorktreePath,
		WorktreeBranch: ps.WorktreeBranch,
		// NOTE: the per-agent task-reference link (TaskURL/TaskLabel) is
		// NOT carried here — the same treatment SpawnConfigJSON gets (it's
		// not persisted on the pending row either). A spawn that reaches
		// enrollment via this async sweeper (chiefly Codex, whose conv-id
		// materialises late) therefore lands with no task link; the inline
		// CC path — the common lead-spawns-worker case — persists it fine.
		// Carrying it would mean two more pending_spawns columns; deferred
		// until the async-spawn-at-task path actually needs it.
		// Birth-time access controls: the sweeper applies the same
		// owner grant + permission overrides the inline paths do, now that the
		// conv-id exists. enrollSpawnedConv reads these off spawnParams.
		IsOwner:             ps.IsOwner,
		PermissionOverrides: ps.PermissionOverrides,
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

// deletePendingSpawnRow removes a pending row, logging (not bubbling) a
// failure — a row left behind is simply re-processed next tick, which the
// membership idempotency guard above makes safe.
func deletePendingSpawnRow(label string) {
	if err := db.DeletePendingSpawn(label); err != nil {
		slog.Warn("pending-spawn sweeper: delete pending row failed",
			"label", label, "error", err)
	}
}
