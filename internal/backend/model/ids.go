package model

import (
	"fmt"
	"regexp"
)

type (
	AgentID                  string
	GroupID                  string
	ConversationID           string
	ExecutionID              string
	OperationID              string
	MessageID                string
	RecipientID              string
	AttachmentID             string
	AttachmentClaimID        string
	RequestID                string
	HistoryPointID           string
	HistoryUseID             string
	WorkspaceID              string
	WorkspaceUseID           string
	WorkRunID                string
	WorkEvidenceID           string
	DefinitionID             string
	DefinitionRevisionID     string
	ProgramProfileID         string
	ProgramProfileRevisionID string
	DeploymentID             string
	AutomationRuleID         string
	AutomationRuleRevisionID string
	OccurrenceID             string
	WorkNodeID               string
	WorkActivationID         string
	WorkIssuanceID           string
	DecisionID               string
)

var stableIDPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)

func ValidateStableID(kind string, value string) error {
	if !stableIDPattern.MatchString(value) {
		return fmt.Errorf("%s must match %s", kind, stableIDPattern.String())
	}
	return nil
}

func (id AgentID) Validate() error        { return ValidateStableID("agent id", string(id)) }
func (id GroupID) Validate() error        { return ValidateStableID("group id", string(id)) }
func (id ConversationID) Validate() error { return ValidateStableID("conversation id", string(id)) }
func (id ExecutionID) Validate() error    { return ValidateStableID("execution id", string(id)) }
func (id OperationID) Validate() error    { return ValidateStableID("operation id", string(id)) }
func (id MessageID) Validate() error      { return ValidateStableID("message id", string(id)) }
func (id RecipientID) Validate() error    { return ValidateStableID("recipient id", string(id)) }
func (id AttachmentID) Validate() error   { return ValidateStableID("attachment id", string(id)) }
func (id AttachmentClaimID) Validate() error {
	return ValidateStableID("attachment claim id", string(id))
}
func (id RequestID) Validate() error      { return ValidateStableID("request id", string(id)) }
func (id HistoryPointID) Validate() error { return ValidateStableID("history point id", string(id)) }
func (id HistoryUseID) Validate() error   { return ValidateStableID("history use id", string(id)) }
func (id WorkspaceID) Validate() error    { return ValidateStableID("workspace id", string(id)) }
func (id WorkspaceUseID) Validate() error { return ValidateStableID("workspace use id", string(id)) }
func (id WorkRunID) Validate() error      { return ValidateStableID("work run id", string(id)) }
func (id WorkEvidenceID) Validate() error { return ValidateStableID("work evidence id", string(id)) }
func (id DefinitionID) Validate() error   { return ValidateStableID("definition id", string(id)) }
func (id DefinitionRevisionID) Validate() error {
	return ValidateStableID("definition revision id", string(id))
}
func (id ProgramProfileID) Validate() error {
	return ValidateStableID("program profile id", string(id))
}
func (id ProgramProfileRevisionID) Validate() error {
	return ValidateStableID("program profile revision id", string(id))
}
func (id DeploymentID) Validate() error { return ValidateStableID("deployment id", string(id)) }
func (id AutomationRuleID) Validate() error {
	return ValidateStableID("automation rule id", string(id))
}
func (id AutomationRuleRevisionID) Validate() error {
	return ValidateStableID("automation rule revision id", string(id))
}
func (id OccurrenceID) Validate() error { return ValidateStableID("occurrence id", string(id)) }
func (id WorkNodeID) Validate() error   { return ValidateStableID("work node id", string(id)) }
func (id WorkActivationID) Validate() error {
	return ValidateStableID("work activation id", string(id))
}
func (id WorkIssuanceID) Validate() error { return ValidateStableID("work issuance id", string(id)) }
func (id DecisionID) Validate() error     { return ValidateStableID("decision id", string(id)) }
