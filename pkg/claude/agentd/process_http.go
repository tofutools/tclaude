package agentd

import (
	"encoding/json"
	"net/http"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

func processRoutesEnabled() bool {
	cfg, err := config.Load()
	return err == nil && cfg.ProcessesEnabled()
}

// processDisabledCode is the stable machine-readable code the daemon returns
// on every process route while the feature flag is off. The CLI keys on this
// (not the HTTP status) to distinguish a feature-disabled daemon from an
// ordinary not-found, so it can render config.ProcessesDisabledMessage instead
// of a bare 404. The status stays 404 — a disabled surface is genuinely
// absent, and 403 already means permission-denied on an enabled route.
const processDisabledCode = "processes_disabled"

func processRoute(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !processRoutesEnabled() {
			writeError(w, http.StatusNotFound, processDisabledCode, config.ProcessesDisabledMessage)
			return
		}
		next(w, r)
	}
}

func writeProcessJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
