package model

import "time"

// TeamPhase is advisory coordination guidance. Role labels and completion prose
// do not grant authority or impose execution gates.
type TeamPhase struct {
	Name     string
	Roles    []string
	Criteria string
}

// ProcessPhases also reads the name-only format used by earlier v2 definitions.
// Keeping that authored field untouched preserves existing revision identities.
func (t TeamDefinition) ProcessPhases() []TeamPhase {
	if len(t.AdvisoryProcess) != 0 {
		return t.AdvisoryProcess
	}
	phases := make([]TeamPhase, len(t.AdvisoryPhases))
	for i, name := range t.AdvisoryPhases {
		phases[i].Name = name
	}
	return phases
}

// TeamPhaseTransition records an explicit advisory move, not an execution gate.
type TeamPhaseTransition struct {
	From         string
	To           string
	ActorKind    PrincipalKind
	ActorAgentID AgentID `json:",omitempty"`
	At           time.Time
}
