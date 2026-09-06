package model

// MessageAudience is the authored collaboration audience before it is pinned
// to concrete recipients. Audience expansion is application/store owned so
// orchestration never needs to infer current group or role membership.
type MessageAudience struct {
	AgentIDs []AgentID
	GroupID  GroupID
	RoleID   RoleID
	Operator bool
}
