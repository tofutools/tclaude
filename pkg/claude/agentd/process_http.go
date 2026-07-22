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

func processRoute(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !processRoutesEnabled() {
			http.NotFound(w, r)
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
