package model

// SpawnLineage is application-produced evidence for delegated new-member
// creation. It is not an operator-authored grant or a public request field.
// Settlement rechecks the live parent association and immutable launch spec.
type SpawnLineage struct {
	Atomic            bool
	ChildAgentID      AgentID
	GroupID           GroupID
	ParentAgentID     AgentID
	ParentExecutionID ExecutionID
	ParentSpec        ResolvedExecutionSpec
	Authored          DesiredConfiguration
	Resolved          DesiredConfiguration
	PreparedSandbox   *SandboxSelection
}
