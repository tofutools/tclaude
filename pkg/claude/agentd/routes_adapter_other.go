//go:build !linux && !darwin

package agentd

import (
	"context"
	"errors"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/routebroker"
)

// Group routes are activated on Linux and Darwin only. Everywhere else the
// capability must refuse explicitly: silently reporting success would leave a
// durable route row that no adapter can ever honour, which reads to an operator
// as a working route with a weaker boundary.
var errGroupRoutesUnsupported = errors.New("group routes are unsupported on this platform; route activation is limited to Linux and Darwin")

func routeAdapterPublish(context.Context, *db.AgentRoute) (bool, error) {
	return true, errGroupRoutesUnsupported
}

func routeAdapterOpen(context.Context, *db.AgentRoute, *db.AgentRouteLease) (string, bool, error) {
	return "", true, errGroupRoutesUnsupported
}

func routeAdapterCloseRoute(string)             {}
func routeAdapterCloseLease(string)             {}
func routeAdapterCloseAll()                     {}
func routeAdapterBrokerEvent(routebroker.Event) {}
