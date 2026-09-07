package app

import "github.com/tofutools/tclaude/internal/backend/model"

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
