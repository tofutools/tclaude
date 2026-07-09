package state

import (
	"regexp"
	"strings"
)

var (
	humanActorPattern   = regexp.MustCompile(`^human:[A-Za-z0-9._@-]+$`)
	agentActorPattern   = regexp.MustCompile(`^agent:agt_[A-Za-z0-9]+$`)
	programActorPattern = regexp.MustCompile(`^program:.+@exit-?[0-9]+$`)
)

func ValidateActorRef(actor ActorRef) bool {
	value := strings.TrimSpace(string(actor))
	return humanActorPattern.MatchString(value) ||
		agentActorPattern.MatchString(value) ||
		programActorPattern.MatchString(value)
}
