package model

// SpawnLineage is application-produced evidence for a group owner's delegated
// creation. It is not an operator-authored grant or a public request field.
// Settlement rechecks the live parent association and immutable launch spec.
type SpawnLineage struct {
	GroupID           GroupID
	ParentAgentID     AgentID
	ParentExecutionID ExecutionID
	ParentSpec        ResolvedExecutionSpec
	Authored          DesiredConfiguration
	Resolved          DesiredConfiguration
	PreparedSandbox   *SandboxSelection
}
