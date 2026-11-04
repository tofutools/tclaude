package agentd

import (
	"context"
	"net/http"
	"strconv"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// The spawn preview (tcl-ezvrww) resolves a spawn request without spawning:
// it runs the spawn handler itself (handleGroupSpawn) up to its first side
// effect and answers with the resolved launch, each field's value and the
// tier that decided it. There is no second resolver to drift from the spawn;
// the dashboard's spawn dialog asks it instead of walking the profile tiers
// itself. It is the dashboard's for now, run as the human (as a dashboard
// spawn is), so no agent-caller challenge or ask-human popup can fire.

type spawnPreviewKey struct{}

func withSpawnPreview(r *http.Request) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), spawnPreviewKey{}, true))
}

func isSpawnPreview(r *http.Request) bool {
	preview, _ := r.Context().Value(spawnPreviewKey{}).(bool)
	return preview
}

// spawnPreviewResponse is what a spawn preview answers.
type spawnPreviewResponse struct {
	Resolved *agent.ResolvedLaunch `json:"resolved"`
}

// dashboardSpawnPreview is POST /api/groups/{name}/spawn-preview, taking the
// body POST /api/groups/{name}/spawn takes.
func dashboardSpawnPreview(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	handleGroupSpawn(w, withSpawnPreview(withSpawnQueueOrigin(asDashboardSpawnPeer(r), "dashboard")), g)
}

// resolvedToggle is a resolved launch toggle as the echo carries it.
func resolvedToggle(value bool, source string) *agent.ResolvedField {
	if source == "" {
		source = agent.ProvHarnessDefault
	}
	return &agent.ResolvedField{Value: strconv.FormatBool(value), Source: source}
}
