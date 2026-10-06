package agentd

import (
	"net/http"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

type httpProxyLaunchRowKey struct{}
type httpProxyLaunchGroup struct {
	groupID   int64
	expires   time.Time
	overrides map[string]overridePermSource
}

var httpProxyLaunchGroups sync.Map

// Seed-based harnesses do not have a conv-id until they start. Preserve the
// daemon's group assignment long enough to project launch-time permissions;
// it is never used to authorize an upstream request.
func rememberHTTPProxyLaunchGroup(label string, group *db.AgentGroup, overrides ...map[string]db.PermissionOverride) {
	groupID := int64(0)
	if group != nil {
		groupID = group.ID
	}
	overrideSources := map[string]overridePermSource{}
	if len(overrides) > 0 {
		for slug, override := range overrides[0] {
			overrideSources[slug] = overridePermSource{Effect: override.Effect, ScopeJSON: override.Scope}
		}
	}
	now := time.Now()
	httpProxyLaunchGroups.Range(func(key, value any) bool {
		if value.(httpProxyLaunchGroup).expires.Before(now) {
			httpProxyLaunchGroups.Delete(key)
		}
		return true
	})
	httpProxyLaunchGroups.Store(label, httpProxyLaunchGroup{groupID: groupID, expires: now.Add(10 * time.Minute), overrides: overrideSources})
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
				src.override = launch.overrides
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
		if row != nil {
			conv = row.ConvID
		} else if _, ok := requireAgent(w, r); !ok {
			return
		}
		verdict = resolvePermissionVerdictForRequest(r, conv, PermHTTP)
	}
	names := []string{}
	environment := map[string]string{}
	if verdict.Resolution == permAllow && cfg.Agent != nil {
		for name, instance := range cfg.Agent.HTTPProxies {
			if evalPermissionScope(verdict, conv, ActionContext{HTTPProxy: name}).Satisfied {
				names = append(names, name)
				if instance.EnvironmentVariable != "" {
					environment[name] = instance.EnvironmentVariable
				}
			}
		}
	}
	sort.Strings(names)
	response := map[string]any{"names": names}
	if len(environment) > 0 {
		response["environment_variables"] = environment
	}
	writeJSON(w, http.StatusOK, response)
}

// httpProxyRuntimeCaller verifies the socket peer is in the managed server's
// recorded process tree. Bootstrap can project names before the server binds;
// spending credentials additionally requires live endpoint ownership.
func httpProxyRuntimeCaller(pid int, label string) (*db.SessionRow, string) {
	openCodeProcesses.Lock()
	process := openCodeProcesses.bySession[label]
	root, conv := 0, ""
	stopping := process != nil && (process.exited || process.stopping)
	if process != nil && !stopping {
		root, conv = process.pid, process.convID
	}
	openCodeProcesses.Unlock()
	if stopping {
		return nil, ""
	}
	// A healthy server survives daemon restart. Re-adopt only its recorded,
	// endpoint-owning root, never the daemon itself (whose subtree includes all
	// managed servers).
	if process == nil {
		runtime, err := db.GetOpenCodeRuntime(label)
		if err != nil || runtime == nil || runtime.PID == os.Getpid() || !openCodeRuntimeVerified(*runtime) {
			return nil, ""
		}
		root, conv = runtime.PID, runtime.ConvID
	}
	if root <= 1 || root == os.Getpid() {
		return nil, ""
	}
	current := pid
	proved := false
	for range 16 {
		if current <= 1 {
			break
		}
		if current == root {
			proved = true
			break
		}
		current = procParent(current)
	}
	if !proved {
		return nil, ""
	}
	runtime, err := db.GetOpenCodeRuntime(label)
	if err == nil && runtime != nil && runtime.PID == root {
		conv = runtime.ConvID
		if conv != "" && openCodeRuntimeVerified(*runtime) {
			return &db.SessionRow{ID: label, ConvID: conv}, conv
		}
	}
	return &db.SessionRow{ID: label, ConvID: conv}, ""
}

// The first proof on a Unix connection uses the global backstop. After that
// proof, repeated gateway requests are charged to its verified subject. The
// identity itself is still proved on every request; this is only rate routing.
type httpProxyProofSubjectKey struct{}
type httpProxyProofSubject struct {
	sync.Mutex
	claim   string
	subject string
}

func httpProxyRateKey(r *http.Request, claim string) string {
	cache, _ := r.Context().Value(httpProxyProofSubjectKey{}).(*httpProxyProofSubject)
	if cache != nil {
		cache.Lock()
		defer cache.Unlock()
		if cache.claim == claim && cache.subject != "" {
			return brokerProofKeyForRow(cache.subject)
		}
	}
	return brokerProofKey
}
func rememberHTTPProxyProofSubject(r *http.Request, claim, subject string) {
	cache, _ := r.Context().Value(httpProxyProofSubjectKey{}).(*httpProxyProofSubject)
	if cache != nil {
		cache.Lock()
		cache.claim, cache.subject = claim, subject
		cache.Unlock()
	}
}
