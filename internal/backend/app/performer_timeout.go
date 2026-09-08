package app

import (
	"context"
	"github.com/tofutools/tclaude/internal/backend/model"
	"strings"
	"time"
)

func performerTimeout(performer model.Performer) (time.Duration, error) {
	if strings.TrimSpace(performer.Timeout) == "" {
		return 0, nil
	}
	duration, err := time.ParseDuration(strings.TrimSpace(performer.Timeout))
	if err != nil || duration <= 0 {
		return 0, fail(ErrInvalid, "performer timeout must be a positive duration")
	}
	return duration, nil
}

func performerDeadline(performer *model.Performer, readyAt, limit time.Time) time.Time {
	if performer == nil {
		return limit
	}
	duration, err := performerTimeout(*performer)
	if err == nil && duration > 0 && readyAt.Add(duration).Before(limit) {
		return readyAt.Add(duration)
	}
	return limit
}

// Pin only explicitly authored activation timeouts. Profiles with no timeout keep
// their existing semantics; their host deadline remains the attempt deadline.
func (s *Service) pinProgramActivationTimeouts(ctx context.Context, graph *model.WorkGraph) error {
	for _, node := range graph.Nodes {
		if node.Performer == nil || node.Performer.Kind != model.PerformerProgram {
			continue
		}
		duration, err := performerTimeout(*node.Performer)
		if err != nil {
			return err
		}
		if duration == 0 {
			continue
		}
		ref := node.Performer.Program.Profile
		profile, err := s.store.ProgramProfileRevision(ctx, ref.RevisionID)
		if err != nil {
			return err
		}
		if profile.ProfileID != ref.ProfileID || profile.ContentHash != ref.ContentHash {
			return fail(ErrConflict, "program profile does not match pinned revision")
		}
		if profile.Timeout > 0 && profile.Timeout < duration {
			duration = profile.Timeout
		}
		if graph.ProgramActivationTimeouts == nil {
			graph.ProgramActivationTimeouts = make(map[model.WorkNodeID]time.Duration)
		}
		graph.ProgramActivationTimeouts[node.ID] = duration
	}
	return nil
}

func activationDeadline(performer *model.Performer, budget time.Duration, readyAt, limit time.Time) time.Time {
	limit = performerDeadline(performer, readyAt, limit)
	if budget > 0 && readyAt.Add(budget).Before(limit) {
		return readyAt.Add(budget)
	}
	return limit
}
