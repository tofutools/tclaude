//go:build !linux && !darwin

package agentd

import (
	"context"
	"errors"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/routebroker"
)

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
