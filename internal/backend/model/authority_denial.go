package model

import "time"

type DenialID string

func (id DenialID) Validate() error { return ValidateStableID("denial id", string(id)) }

// AuthorityDenial overrides the subject's grants for an action on every
// resource. Like v1's explicit deny, it has no resource scope or configuration
// bounds. Removing it restores evaluation of the subject's current grants.
type AuthorityDenial struct {
	ID        DenialID
	Subject   AuthoritySubject
	Action    Action
	Revision  Revision
	CreatedAt time.Time
	UpdatedAt time.Time
}
