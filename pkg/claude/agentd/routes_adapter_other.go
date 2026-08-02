//go:build !darwin

package agentd

import (
	"context"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/routebroker"
)

func routeAdapterPublish(context.Context, *db.AgentRoute) (bool, error) { return false, nil }

func routeAdapterOpen(context.Context, *db.AgentRoute, *db.AgentRouteLease) (string, bool, error) {
	return "", false, nil
}

func routeAdapterCloseRoute(string)             {}
func routeAdapterCloseLease(string)             {}
func routeAdapterCloseAll()                     {}
func routeAdapterBrokerEvent(routebroker.Event) {}
