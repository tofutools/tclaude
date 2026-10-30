package agentd

import (
	"net/http"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

func collabRoutesEnabled() bool {
	cfg, err := config.LoadShared()
	return err == nil && cfg.CollabEnabled()
}

// collabDisabledCode marks a collab route answering while features.collab is
// off. As with processRoute the status stays 404: a disabled surface is
// absent, and 403 means permission-denied on an enabled route.
const collabDisabledCode = "collab_disabled"

func collabRoute(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !collabRoutesEnabled() {
			writeError(w, http.StatusNotFound, collabDisabledCode, config.CollabDisabledMessage)
			return
		}
		next(w, r)
	}
}
