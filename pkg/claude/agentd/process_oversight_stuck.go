package agentd

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
)

// Engine stuck signals (tk_e2fa2bd0): a step that stays stuck longer than its
// patience raises a concern on its own, to the run's overseer, as an agent in
// the run would. One concern per stuck spell: answering it ("carry on") does
// not bring it back while the step stays as it was, and it settles itself
// ("cleared") when the step moves.
const (
	// processSignalBlocked: the step's branch is parked (retries spent).
	processSignalBlocked = "blocked"
	// processSignalNoPerformer: nobody can take the step.
	processSignalNoPerformer = "no_performer"
	// processSignalNoReport: the step was handed out and not reported within
	// its patience. Only a step (or template) with a patience of its own
	// raises it: agent work has no natural length.
	processSignalNoReport = "no_report"
	// processConcernCleared settles an engine concern whose step moved on.
	processConcernCleared = "cleared"
)

// processStuckSpell is one step stuck one way since a moment.
type processStuckSpell struct {
	NodeID string
	Signal string
	Since  time.Time
	Detail string
}

func (s processStuckSpell) key() string { return s.NodeID + "\x00" + s.Signal }

// processRunStuckSpells lists how a run's steps are stuck now.
func processRunStuckSpells(run *db.ProcessRun, oversight processTemplateOversight) []processStuckSpell {
	var spells []processStuckSpell
	var checkpoint engine.Checkpoint
	if err := run.DecodeCheckpoint(&checkpoint); err == nil && len(checkpoint.Blocked) > 0 {
		since := map[string]time.Time{}
		if events, err := db.ListLatestProcessRunNodeEvents(run.ID, "node_blocked"); err == nil {
			for _, e := range events {
				since[e.NodeID] = e.OccurredAt
			}
		}
		for _, b := range checkpoint.Blocked {
			at := since[b.NodeID]
			if at.IsZero() {
				at = run.UpdatedAt
			}
			spells = append(spells, processStuckSpell{NodeID: b.NodeID, Signal: processSignalBlocked, Since: at, Detail: b.Reason})
		}
	}
	rows, err := db.ListProcessAgentDispatchesForRun(run.ID)
	if err != nil {
		return spells
	}
	seen := map[string]bool{}
	for _, row := range rows {
		var spell processStuckSpell
		switch row.State {
		case db.ProcessDispatchNeedsPerformer:
			spell = processStuckSpell{NodeID: row.NodeID, Signal: processSignalNoPerformer, Since: row.UpdatedAt, Detail: row.WaitReason}
		case db.ProcessDispatchDelivered:
			if !oversight.explicitPatience(row.NodeID) || row.DeliveredAt.IsZero() {
				continue
			}
			spell = processStuckSpell{NodeID: row.NodeID, Signal: processSignalNoReport, Since: row.DeliveredAt,
				Detail: "handed to " + processActorName(processAgentRef(row.AgentID))}
		default:
			continue
		}
		if seen[spell.key()] {
			continue
		}
		seen[spell.key()] = true
		spells = append(spells, spell)
	}
	return spells
}

// processStuckReason is the concern's text.
func processStuckReason(s processStuckSpell, stuckFor time.Duration) string {
	held := stuckFor.Round(time.Minute)
	var text string
	switch s.Signal {
	case processSignalBlocked:
		text = fmt.Sprintf("Step %s has been blocked for %s: its retries are spent.", s.NodeID, held)
	case processSignalNoPerformer:
		text = fmt.Sprintf("Step %s has had nobody to take it for %s.", s.NodeID, held)
	case processSignalNoReport:
		text = fmt.Sprintf("Step %s was handed out %s ago and has not been reported.", s.NodeID, held)
	}
	if detail := strings.TrimSpace(s.Detail); detail != "" {
		text += " (" + detail + ")"
	}
	return text
}

// sweepProcessStuckSignals raises a concern for each step stuck longer than
// its patience, and settles engine concerns whose step moved on. It runs on
// the process runtime's ticker.
func sweepProcessStuckSignals() {
	now := processOversightNow()
	after := ""
	for {
		runs, err := db.ListActiveProcessRuns(after, db.MaxProcessRunReadPage)
		if err != nil {
			slog.Warn("process oversight: active runs unreadable", "error", err)
			return
		}
		for i := range runs {
			checkProcessRunStuck(&runs[i], now)
		}
		if len(runs) < db.MaxProcessRunReadPage {
			return
		}
		after = runs[len(runs)-1].ID
	}
}

func checkProcessRunStuck(run *db.ProcessRun, now time.Time) {
	oversight := processRunTemplateOversight(run.ID)
	spells := processRunStuckSpells(run, oversight)
	live := map[string]bool{}
	for _, s := range spells {
		live[s.key()] = true
	}
	concerns, err := db.ListProcessRunConcerns(run.ID)
	if err != nil {
		return
	}
	for _, c := range concerns {
		if c.Open() && c.Source == db.ProcessConcernSourceEngine && !live[c.NodeID+"\x00"+c.Signal] {
			if resolved, err := db.ResolveProcessRunConcern(run.ID, c.ID, processConcernCleared, "", processOversightActor); err == nil {
				closeProcessConcernCard(resolved, processOversightActor)
			}
		}
	}
	// A paused run is someone's decision to hold it: its steps waiting is not
	// news to raise. Concerns already raised stay with their holders.
	if scope, err := db.GetProcessRunScope(run.ID); err != nil || scope.Paused() {
		return
	}
	for _, s := range spells {
		stuckFor := now.Sub(s.Since)
		if stuckFor < oversight.patienceFor(s.NodeID) || processSpellRaised(concerns, s) {
			continue
		}
		if _, err := raiseProcessConcern(run.ID, s.NodeID, db.ProcessConcernSourceEngine, s.Signal,
			processOversightActor, processStuckReason(s, stuckFor)); err != nil && !errors.Is(err, db.ErrProcessConcernLimit) {
			slog.Warn("process oversight: raise a stuck signal", "run", run.ID, "node", s.NodeID, "signal", s.Signal, "error", err)
		}
	}
}

// processSpellRaised reports whether this stuck spell already has a concern:
// one about the same step and signal raised since the spell began.
func processSpellRaised(concerns []db.ProcessRunConcern, s processStuckSpell) bool {
	for _, c := range concerns {
		if c.Source == db.ProcessConcernSourceEngine && c.NodeID == s.NodeID && c.Signal == s.Signal && !c.CreatedAt.Before(s.Since) {
			return true
		}
	}
	return false
}
