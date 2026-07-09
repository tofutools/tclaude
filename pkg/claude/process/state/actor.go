package state

import (
	"regexp"
	"strings"
)

var (
	humanActorPattern   = regexp.MustCompile(`^human:[A-Za-z0-9._@-]+$`)
	agentActorPattern   = regexp.MustCompile(`^agent:agt_[A-Za-z0-9]+$`)
	programActorPattern = regexp.MustCompile(`^program:.+@exit-?[0-9]+$`)
	// engine:<slug> marks decisions the engine synthesizes itself (for
	// example the evidence-unchanged short-circuit) — no human, agent, or
	// program performed anything, and faking a program run would stamp
	// provenance on an execution that never happened.
	engineActorPattern = regexp.MustCompile(`^engine:[a-z0-9][a-z0-9._-]*$`)
)

// ActorEvidenceUnchanged is the engine actor for evidence-unchanged
// short-circuit decision records.
const ActorEvidenceUnchanged ActorRef = "engine:evidence-unchanged"

func ValidateActorRef(actor ActorRef) bool {
	value := string(actor)
	return humanActorPattern.MatchString(value) ||
		agentActorPattern.MatchString(value) ||
		programActorPattern.MatchString(value) ||
		engineActorPattern.MatchString(value)
}

// IsEngineActor reports whether an actor ref claims the reserved engine
// namespace. External inputs (CLI verdicts, performer observations) must
// never carry it: an engine actor asserts the ENGINE synthesized the
// decision, and accepting one from outside would forge short-circuit
// provenance without the hash check the gate_short_circuited reducer
// enforces.
func IsEngineActor(actor ActorRef) bool {
	return strings.HasPrefix(string(actor), "engine:")
}
