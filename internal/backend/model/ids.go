package model

import (
	"fmt"
	"regexp"
)

type (
	AgentID        string
	GroupID        string
	ConversationID string
	ExecutionID    string
	OperationID    string
	MessageID      string
	RecipientID    string
	RequestID      string
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
func (id RequestID) Validate() error      { return ValidateStableID("request id", string(id)) }
