package agentd

import (
	"net/http"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

func triggerRoutesEnabled() bool {
	cfg, err := config.Load()
	return err == nil && cfg.TriggersEnabled()
}

const triggerDisabledCode = "triggers_disabled"

func triggerRoute(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !triggerRoutesEnabled() {
			writeError(w, http.StatusNotFound, triggerDisabledCode, config.TriggersDisabledMessage)
			return
		}
		next(w, r)
	}
}
