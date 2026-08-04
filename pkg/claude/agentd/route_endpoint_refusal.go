package agentd

import (
	"errors"

	"github.com/tofutools/tclaude/pkg/claude/routebroker"
)

// routeEndpointRefusalDetail maps a broker attach failure onto the short,
// printable detail stored on the consumer endpoint. Both adapter paths — the
// Linux channel handler and the Darwin listener adapter — refuse for the same
// broker reasons, so they share one vocabulary and an agent reading a lease
// gets the same wording whichever platform refused it.
func routeEndpointRefusalDetail(err error) string {
	switch {
	case errors.Is(err, routebroker.ErrConsumerLimit):
		return "route adapter capacity exhausted"
	case errors.Is(err, routebroker.ErrUnauthorized):
		return "route adapter authorization refused"
	default:
		return "route adapter attachment refused"
	}
}
