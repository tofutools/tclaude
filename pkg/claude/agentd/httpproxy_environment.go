package agentd

import (
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

type httpProxyLaunchRowKey struct{}
type httpProxyLaunchGroup struct {
	groupID int64
	expires time.Time
}

var httpProxyLaunchGroups sync.Map

// Seed-based harnesses do not have a conv-id until they start. Preserve the
// daemon's group assignment long enough to project launch-time permissions;
// it is never used to authorize an upstream request.
func rememberHTTPProxyLaunchGroup(label string, group *db.AgentGroup) {
	if group == nil {
		return
	}
	now := time.Now()
	httpProxyLaunchGroups.Range(func(key, value any) bool {
		if value.(httpProxyLaunchGroup).expires.Before(now) {
			httpProxyLaunchGroups.Delete(key)
		}
		return true
	})
	httpProxyLaunchGroups.Store(label, httpProxyLaunchGroup{groupID: group.ID, expires: now.Add(10 * time.Minute)})
}

// handleHTTPProxyEnvironment projects names, never credentials or upstream
// URLs. A pre-harness caller must prove its live launch pane; an established
// agent uses the ordinary identity. Revocations are still enforced per request.
func handleHTTPProxyEnvironment(w http.ResponseWriter, r *http.Request) {
	cfg, err := config.Load()
	if err != nil {
		writeError(w, 500, "io", "could not load HTTP proxy configuration")
		return
	}
	row, _ := r.Context().Value(httpProxyLaunchRowKey{}).(*db.SessionRow)
	var verdict permVerdict
	conv := peerFromContext(r.Context()).ConvID
	if row != nil && row.ConvID == "" {
		src := permSources{resolvable: true, group: map[string][]string{}}
		if value, ok := httpProxyLaunchGroups.Load(row.ID); ok {
			launch := value.(httpProxyLaunchGroup)
			if time.Now().Before(launch.expires) {
				grants, readErr := db.ListAgentGroupPermissionRows(launch.groupID)
				if readErr != nil {
					writeError(w, 500, "io", "could not resolve HTTP proxy permissions")
					return
				}
				for _, grant := range grants {
					src.group[grant.Slug] = append(src.group[grant.Slug], grant.ScopeJSON)
				}
			}
		}
		verdict = resolvePermissionVerdictFrom(src, PermHTTP, cfg.HasDefaultPermission(PermHTTP))
	} else {
		if _, ok := requireAgent(w, r); !ok {
			return
		}
		verdict = resolvePermissionVerdictForRequest(r, conv, PermHTTP)
	}
	names := []string{}
	if verdict.Resolution == permAllow && cfg.Agent != nil {
		for name := range cfg.Agent.HTTPProxies {
			if evalPermissionScope(verdict, conv, ActionContext{HTTPProxy: name}).Satisfied {
				names = append(names, name)
			}
		}
	}
	sort.Strings(names)
	writeJSON(w, http.StatusOK, map[string]any{"names": names})
}
