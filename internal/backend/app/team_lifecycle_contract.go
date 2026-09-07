package app

import (
	"context"

	"github.com/tofutools/tclaude/internal/backend/model"
)

// RebriefDeploymentRequest selects a new immutable team definition revision
// for follow-up briefings. It does not change the deployment's launch
// configuration, roster, workspaces, roles, or rhythms.
type RebriefDeploymentRequest struct {
	Context          RequestContext
	DeploymentID     model.DeploymentID
	ExpectedRevision model.Revision
	Definition       model.DefinitionRef
}

type AdvanceAdvisoryPhaseRequest struct {
	Context          RequestContext
	DeploymentID     model.DeploymentID
	ExpectedRevision model.Revision
}

type StandDownDeploymentRequest struct {
	Context          RequestContext
	DeploymentID     model.DeploymentID
	ExpectedRevision model.Revision
	Reason           string
}

// These methods intentionally publish the transport seam before the durable
// lifecycle implementation lands in team_work.go.
func (s *Service) RebriefDeployment(context.Context, RebriefDeploymentRequest) (TeamDeploymentResult, error) {
	return TeamDeploymentResult{}, fail(ErrUnsupported, "team rebrief is not implemented")
}

func (s *Service) AdvanceAdvisoryPhase(context.Context, AdvanceAdvisoryPhaseRequest) (TeamDeploymentResult, error) {
	return TeamDeploymentResult{}, fail(ErrUnsupported, "advisory phase advancement is not implemented")
}

func (s *Service) StandDownDeployment(context.Context, StandDownDeploymentRequest) (TeamDeploymentResult, error) {
	return TeamDeploymentResult{}, fail(ErrUnsupported, "team stand-down is not implemented")
}
