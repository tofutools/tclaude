package app

import (
	"context"
	"encoding/json"
	"math"
	"time"

	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

// The v1 wave runner gives each preceding wave eight minutes by default.
const defaultTeamWaveMaxWait = 8 * time.Minute

const teamWaveActivationEvidence model.WorkEvidenceKind = "team_wave_activation"

func teamWaveGateID(wave string) model.WorkNodeID { return model.WorkNodeID("wave_idle_" + wave) }

func teamWaveMaxWait(wave model.TeamWave) time.Duration {
	if wave.MaxWaitSeconds == 0 {
		return defaultTeamWaveMaxWait
	}
	return time.Duration(wave.MaxWaitSeconds) * time.Second
}

func teamWaveRunBudget(team model.TeamDefinition) (time.Duration, error) {
	budget := admittedEffectTimeout
	for _, wave := range team.Waves {
		if wave.MaxWaitSeconds < 0 || wave.MaxWaitSeconds > math.MaxInt64/int64(time.Second) {
			return 0, fail(ErrInvalid, "wave maximum wait must be a nonnegative representable number of seconds")
		}
		if !wave.WaitForIdle {
			continue
		}
		wait := teamWaveMaxWait(wave)
		if wait > time.Duration(math.MaxInt64)-budget {
			return 0, fail(ErrInvalid, "combined wave wait is too long")
		}
		budget += wait
	}
	return budget, nil
}

// reconcileTeamWaveGate recognizes only a gate in the deployment's own run.
// Generic process waits, even with similar names, retain ordinary wait behavior.
func (s *Service) reconcileTeamWaveGate(ctx context.Context, record WorkRunRecord, attempt model.WorkNodeAttempt) (WorkRunRecord, bool, error) {
	if record.Run.Scope.DeploymentID == "" {
		return record, false, nil
	}
	deployment, err := s.store.TeamDeployment(ctx, record.Run.Scope.DeploymentID)
	if err != nil {
		return record, false, err
	}
	if deployment.WorkRunID != record.Run.ID || deployment.GroupID != record.Run.Scope.GroupID {
		return record, false, nil
	}
	revision, err := s.store.DefinitionRevision(ctx, deployment.Definition.RevisionID)
	if err != nil {
		return record, false, err
	}
	if revision.Team == nil {
		return record, false, ErrConflict
	}
	var selected *model.TeamWave
	for i := range revision.Team.Waves {
		wave := &revision.Team.Waves[i]
		if wave.WaitForIdle && teamWaveGateID(wave.ID) == attempt.Ref.NodeID {
			selected = wave
			break
		}
	}
	if selected == nil {
		return record, false, nil
	}
	activated := map[model.ExecutionID]bool{}
	for _, evidence := range record.NodeEvidence {
		if evidence.Kind != teamWaveActivationEvidence || evidence.Attempt != attempt.Ref {
			continue
		}
		var ids []model.ExecutionID
		if err := json.Unmarshal([]byte(evidence.Detail), &ids); err != nil {
			return record, true, err
		}
		for _, id := range ids {
			activated[id] = true
		}
	}
	ids := make([]model.ExecutionID, 0, len(selected.MemberKeys))
	observations := map[model.ExecutionID]ports.Observation{}
	for _, key := range selected.MemberKeys {
		var executionID model.ExecutionID
		for _, memberAttempt := range record.Run.NodeAttempts {
			if memberAttempt.Ref.NodeID == model.WorkNodeID("member_"+key) {
				executionID = memberAttempt.ExecutionID
			}
		}
		ids = append(ids, executionID)
		if executionID == "" {
			continue
		}
		execution, readErr := s.store.Execution(ctx, executionID)
		if readErr != nil {
			continue
		}
		if execution.State == model.ExecutionExited || execution.State == model.ExecutionFailed {
			observations[executionID] = ports.Observation{Workload: ports.WorkloadExited}
			continue
		}
		runtime, runtimeErr := s.runtimeFor(ctx, execution)
		if runtimeErr != nil {
			continue
		}
		observation, observeErr := runtime.Observe(ctx)
		if observeErr == nil {
			observations[executionID] = observation
		}
	}
	now := s.now().UTC()
	released, newly := teamWaveGate(now, attempt.ReadyAt.Add(teamWaveMaxWait(*selected)), ids, activated, observations)
	if !released && len(newly) == 0 {
		return record, true, nil
	}
	transition := GraphTransition{WorkRunID: record.Run.ID, ExpectedRevision: record.Run.Revision, RunState: record.Run.State, ControlState: record.Run.ControlState, RunOutcome: record.Run.Outcome, At: now}
	if released {
		transition = s.graphOutcomeTransition(record, attempt, model.WorkOutcomeVerified, "wave settled or maximum wait elapsed")
	}
	if len(newly) > 0 {
		encoded, _ := json.Marshal(newly)
		transition.Evidence = &model.WorkNodeEvidence{ID: model.WorkEvidenceID(s.newID("evidence_")), RequestID: model.RequestID(s.newID("request_")), Attempt: attempt.Ref, Reporter: model.OperatorPrincipal(), Kind: teamWaveActivationEvidence, Detail: string(encoded), RecordedAt: now, Revision: 1}
	}
	updated, err := s.store.ApplyGraphTransition(ctx, transition)
	return updated, true, err
}

// teamWaveGate evaluates one sampling pass. Activated contains execution IDs,
// not agent IDs: a previous execution's work must not settle a fresh launch.
// The caller persists newly observed activation before acting on the result.
// Missing observations are unknown, including a provider read that failed.
func teamWaveGate(now, deadline time.Time, executions []model.ExecutionID, activated map[model.ExecutionID]bool, observations map[model.ExecutionID]ports.Observation) (released bool, newlyActivated []model.ExecutionID) {
	allSettled := true
	for _, id := range executions {
		observation, ok := observations[id]
		if !ok {
			allSettled = false
			continue
		}
		if observation.Workload == ports.WorkloadExited {
			continue
		}
		switch observation.AgentActivity {
		case ports.AgentActivityActive:
			if !activated[id] {
				newlyActivated = append(newlyActivated, id)
			}
			allSettled = false
		case ports.AgentActivityIdle, ports.AgentActivityAwaitingInput:
			if !activated[id] {
				allSettled = false
			}
		default:
			allSettled = false
		}
	}
	return allSettled || (!deadline.IsZero() && !now.Before(deadline)), newlyActivated
}
