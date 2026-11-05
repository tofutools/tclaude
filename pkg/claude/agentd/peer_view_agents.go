package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

// Agent peer views: a sending node's agent opens a peer view to act on a
// peer's agents and groups, as the sending node's operator can.
//
// Two gates bound it, and both must pass:
//
//   - On the sending node, the agent holds the route's slug with a
//     peer=<inst>/<group> scope covering the route's footprint on the peer
//     (servePeerViewForAgent). Nothing is granted by default.
//   - On the receiving node, the request runs under an agent principal: the
//     peer's own grants still decide, as for the peer's operator, but an
//     agent principal never takes a local-operator path, not even from an
//     unrestricted peer, and the receiving operator can turn agent views off
//     for the peer (federation peer agent-views).
//
// Both nodes hold the same route allowlist, agentPeerViewRoutes: a newer or
// modified sender cannot reach more on this node than this node lists.

// agentPeerViewTarget is what a route's footprint on the peer is.
type agentPeerViewTarget int

const (
	// agentViewNode is the peer's whole view (its snapshot, its group list).
	agentViewNode agentPeerViewTarget = iota
	// agentViewGroup is the group the route names ({name}).
	agentViewGroup
	// agentViewAgent is every group the target agent ({id}) is in.
	agentViewAgent
)

type agentPeerViewRoute struct {
	// slug is what the sending agent holds, peer-scoped. A group settings
	// update uses fields instead.
	slug   string
	target agentPeerViewTarget
	// fields maps the body fields an agent may send to the slug each needs;
	// any other field is refused (operator-only over a peer view).
	fields map[string]string
	// destructive marks a route that ends sessions; the grant warns.
	destructive bool
}

// agentPeerViewRoutes are the peer view routes an agent principal may use,
// keyed by their peerViewRules pattern. Everything else stays the remote
// operator's: moves and teleports, spawns (agents have the spawn envelope),
// sessions and shells, node management, environment and sandbox, and every
// setting that shapes the group's future members (startup context, saved
// prompts, default directory, profile and spawn target, member permissions,
// owners, nesting).
var agentPeerViewRoutes = map[string]agentPeerViewRoute{
	// Discovery: what the agent may act on.
	"GET /api/snapshot":      {slug: PermAgentsStatusRead, target: agentViewNode},
	"GET /api/groups":        {slug: PermAgentsStatusRead, target: agentViewNode},
	"GET /api/groups/{name}": {slug: PermAgentsStatusRead, target: agentViewGroup},
	"GET /api/agents/{id}":   {slug: PermAgentsStatusRead, target: agentViewAgent},
	// Member lifecycle.
	"POST /api/agents/{id}/stop":    {slug: PermAgentStop, target: agentViewAgent, destructive: true},
	"POST /api/agents/{id}/resume":  {slug: PermAgentResume, target: agentViewAgent},
	"POST /api/agents/{id}/restart": {slug: PermAgentResume, target: agentViewAgent},
	"POST /api/agents/{id}/retire":  {slug: PermAgentRetire, target: agentViewAgent, destructive: true},
	"POST /api/agents/{id}/clone":   {slug: PermAgentClone, target: agentViewAgent},
	// Member settings. Notify has no slug of its own; the task link's
	// covers it, as both are the agent's own dashboard settings.
	"POST /api/agents/{id}/rename":            {slug: PermAgentRename, target: agentViewAgent},
	"POST /api/agents/{id}/tags":              {slug: PermAgentTags, target: agentViewAgent},
	"POST /api/agents/{id}/task":              {slug: PermAgentTask, target: agentViewAgent},
	"POST /api/agents/{id}/notify":            {slug: PermAgentTask, target: agentViewAgent},
	"PATCH /api/groups/{name}/members/{conv}": {slug: PermGroupsMembersUpdate, target: agentViewGroup},
	// Group lifecycle.
	"POST /api/groups/{name}/shutdown": {slug: PermGroupsMembersStop, target: agentViewGroup, destructive: true},
	"POST /api/groups/{name}/power-on": {slug: PermGroupsMembersResume, target: agentViewGroup},
	// Group settings that do not shape future members.
	"PATCH /api/groups/{name}": {target: agentViewGroup, fields: map[string]string{
		"descr":          PermGroupsSettingsDescription,
		"max_members":    PermGroupsSettingsMaxMembers,
		"notify_enabled": PermGroupsSettingsNotifications,
	}},
	"POST /api/groups/{name}/rename":     {slug: PermGroupsRename, target: agentViewGroup},
	"GET /api/groups/{name}/attachment":  {slug: PermGroupsAttachment, target: agentViewGroup},
	"POST /api/groups/{name}/attachment": {slug: PermGroupsAttachment, target: agentViewGroup},
}

// agentPeerViewSlugs lists the sender slugs agentPeerViewRoutes uses, which
// accept a peer= scope (initAgentPeerViewScopes).
func agentPeerViewSlugs() []string {
	var out []string
	for _, route := range agentPeerViewRoutes {
		if route.slug != "" {
			out = append(out, route.slug)
		}
		for _, slug := range route.fields {
			out = append(out, slug)
		}
	}
	sort.Strings(out)
	return slices.Compact(out)
}

// agentPeerViewDestructive reports a slug whose agent peer view route ends
// sessions on the peer, so a peer-scoped grant of it warns.
func agentPeerViewDestructive(slug string) bool {
	for _, route := range agentPeerViewRoutes {
		if route.destructive && route.slug == slug {
			return true
		}
	}
	return false
}

// initAgentPeerViewScopes lets every slug an agent peer view route needs be
// granted peer=<peer>/<group>. A peer-scoped grant never authorizes a local
// action (permissionScopeSatisfied), so this widens nothing locally.
func initAgentPeerViewScopes(registry []PermSlug) {
	slugs := agentPeerViewSlugs()
	for i := range registry {
		if slices.Contains(slugs, registry[i].Slug) && !containsScopeDim(registry[i].ScopeDims, ScopeDimPeer) {
			registry[i].ScopeDims = append(slices.Clone(registry[i].ScopeDims), ScopeDimPeer)
		}
	}
}

// agentPeerViewRouteSlugs is the slugs a request on route needs: its slug,
// or for a settings update one per body field. A field the route does not
// list is refused, naming it.
func agentPeerViewRouteSlugs(route agentPeerViewRoute, body []byte) ([]string, error) {
	if route.fields == nil {
		return []string{route.slug}, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || len(fields) == 0 {
		return nil, fmt.Errorf("expected a JSON object of group settings")
	}
	var out []string
	for name := range fields {
		slug, ok := route.fields[name]
		if !ok {
			return nil, fmt.Errorf("an agent may not change %q over a peer view; it stays the operator's", name)
		}
		out = append(out, slug)
	}
	sort.Strings(out)
	return slices.Compact(out), nil
}

// matchAgentPeerViewRoute resolves a request on the peer's path to its
// allowlisted pattern and path values.
func matchAgentPeerViewRoute(method, path string) (pattern string, values map[string]string, ok bool) {
	agentPeerViewMuxOnce.Do(func() {
		agentPeerViewMux = http.NewServeMux()
		for p := range agentPeerViewRoutes {
			agentPeerViewMux.HandleFunc(p, func(_ http.ResponseWriter, r *http.Request) {
				m := r.Context().Value(agentPeerViewMatchKey{}).(*agentPeerViewMatch)
				m.pattern = r.Pattern
				m.values = map[string]string{"name": r.PathValue("name"), "id": r.PathValue("id")}
			})
		}
	})
	m := &agentPeerViewMatch{}
	req := (&http.Request{Method: method, URL: &url.URL{Path: path}, Header: make(http.Header)}).WithContext(context.WithValue(context.Background(), agentPeerViewMatchKey{}, m))
	agentPeerViewMux.ServeHTTP(discardResponse{}, req)
	return m.pattern, m.values, m.pattern != ""
}

var (
	agentPeerViewMuxOnce sync.Once
	agentPeerViewMux     *http.ServeMux
)

type agentPeerViewMatchKey struct{}
type agentPeerViewMatch struct {
	pattern string
	values  map[string]string
}

type discardResponse struct{}

func (discardResponse) Header() http.Header         { return http.Header{} }
func (discardResponse) Write(b []byte) (int, error) { return len(b), nil }
func (discardResponse) WriteHeader(int)             {}

// Per-agent limits on agent peer views, on top of the per-peer stream
// reservation: one agent cannot exhaust its operator's peer view budget, and
// refused attempts count too, so an agent cannot probe for what it holds.
const (
	agentPeerViewRateWindow = time.Minute
	agentPeerViewRateLimit  = 60
	agentPeerViewInFlight   = 4
)

// agentPeerViewNow is the limiter's clock; tests replace it.
var agentPeerViewNow = time.Now

type agentPeerViewLimiter struct {
	mu       sync.Mutex
	attempts map[string][]time.Time
	inFlight map[string]int
}

var agentPeerViewLimits = &agentPeerViewLimiter{attempts: map[string][]time.Time{}, inFlight: map[string]int{}}

// admit counts an attempt and reports whether it may run; release ends an
// admitted one.
func (l *agentPeerViewLimiter) admit(agentID string) (release func(), reason string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := agentPeerViewNow()
	recent := l.attempts[agentID][:0]
	for _, at := range l.attempts[agentID] {
		if now.Sub(at) < agentPeerViewRateWindow {
			recent = append(recent, at)
		}
	}
	if len(recent) >= agentPeerViewRateLimit {
		l.attempts[agentID] = recent
		return nil, "rate"
	}
	l.attempts[agentID] = append(recent, now)
	if l.inFlight[agentID] >= agentPeerViewInFlight {
		return nil, "concurrency"
	}
	l.inFlight[agentID]++
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.inFlight[agentID]--; l.inFlight[agentID] <= 0 {
			delete(l.inFlight, agentID)
		}
	}, ""
}

type peerViewPrincipalKey struct{}

// withPeerViewPrincipal marks an outbound peer view as an agent's: the open
// envelope carries it (openPeerViewOnce).
func withPeerViewPrincipal(ctx context.Context, a *proto.PeerViewAgent) context.Context {
	return context.WithValue(ctx, peerViewPrincipalKey{}, a)
}

func peerViewPrincipalFrom(ctx context.Context) *proto.PeerViewAgent {
	a, _ := ctx.Value(peerViewPrincipalKey{}).(*proto.PeerViewAgent)
	return a
}

// servePeerViewForAgent is the sending node's gate for an agent's peer view.
func servePeerViewForAgent(w http.ResponseWriter, r *http.Request, caller string, peer *db.FederationPeer) {
	agentID, err := db.AgentIDForConv(caller)
	if err != nil || !proto.ValidAgentRef(agentID) {
		writeError(w, http.StatusForbidden, "auth", "a peer view needs a registered agent")
		return
	}
	principal := &proto.PeerViewAgent{ID: agentID, Name: proto.SafeName(agent.TitleFor(caller), false)}
	route := ""
	status := 0
	defer func() {
		_, _ = db.InsertAuditLog(db.AuditLogEntry{At: time.Now(), ActorKind: db.AuditActorAgent, ActorConv: caller, ActorAgent: agentID, ActorLabel: principal.Name,
			Verb: "federation.peer_view.agent.out", Detail: fmt.Sprintf("peer=%s route=%s", peer.InstanceID, route), Method: r.Method, Status: status, Source: "federation"})
	}()
	refuse := func(code int, reason, msg string) {
		status = code
		writeError(w, code, reason, msg)
	}
	release, limited := agentPeerViewLimits.admit(agentID)
	if limited != "" {
		refuse(http.StatusTooManyRequests, "agent_rate_limited", "too many peer views from this agent; wait and retry")
		return
	}
	defer release()
	tail := peerProxyPath(r.PathValue("tail"))
	pattern, values, ok := matchAgentPeerViewRoute(r.Method, tail)
	if !ok {
		route = r.Method + " (not allowlisted)"
		refuse(http.StatusForbidden, "agent_route_not_allowed", "this peer view route is the operator's; agents may not use it")
		return
	}
	route = pattern
	cat, _, err := fedCatalogFor(peer.InstanceID)
	if err != nil {
		refuse(http.StatusServiceUnavailable, "peer_unavailable", "could not read the peer's catalog")
		return
	}
	if cat == nil {
		cat = &proto.CatalogPayload{}
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, fedPeerViewBodyLimit+1))
	if err != nil || len(body) > fedPeerViewBodyLimit {
		refuse(http.StatusRequestEntityTooLarge, "request_too_large", "peer request body exceeds limit")
		return
	}
	spec := agentPeerViewRoutes[pattern]
	slugs, err := agentPeerViewRouteSlugs(spec, body)
	if err != nil {
		refuse(http.StatusForbidden, "agent_field_not_allowed", err.Error())
		return
	}
	groups := agentPeerViewFootprint(cat, spec.target, values)
	if len(groups) == 0 {
		refuse(http.StatusForbidden, "unknown_footprint", "the peer's catalog does not show the target in a group you can reach")
		return
	}
	gate := &statusRecorder{ResponseWriter: w}
	for _, slug := range slugs {
		for _, group := range groups {
			if _, ok := requirePermission(gate, r, slug, ActionContext{RemotePeer: peer.InstanceID, RemoteGroup: group}); !ok {
				status = gate.code()
				return
			}
		}
	}
	// Only a receiver that knows agent principals may see one: an older one
	// would serve the request as this node's operator's.
	if !cat.PeerViewAgents {
		refuse(http.StatusConflict, "peer_lacks_agent_views", "the peer does not serve agent peer views; its operator must upgrade it")
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	// What a peer returns is untrusted content for the agent reading it.
	w.Header().Set("X-Tclaude-Remote-Content", "untrusted")
	rec := &statusRecorder{ResponseWriter: w}
	servePeerViewProxyWith(rec, r.WithContext(withPeerViewPrincipal(r.Context(), principal)), false)
	status = rec.code()
}

// agentPeerViewFootprint is the remote groups a route acts in, as the
// sending node's latest catalog of the peer shows them: "" for the whole
// view, the named group, or every group the target agent is listed in.
func agentPeerViewFootprint(cat *proto.CatalogPayload, target agentPeerViewTarget, values map[string]string) []string {
	switch target {
	case agentViewNode:
		return []string{""}
	case agentViewGroup:
		for _, g := range cat.Groups {
			if g.Name == values["name"] {
				return []string{g.Name}
			}
		}
		return nil
	}
	var out []string
	for _, g := range cat.Groups {
		listed := false
		for _, m := range g.Members {
			listed = listed || m.Agent == values["id"]
		}
		for _, s := range g.AgentStatuses {
			listed = listed || s.Agent == values["id"]
		}
		if listed {
			out = append(out, g.Name)
		}
	}
	return out
}

// statusRecorder remembers the status written through it.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusRecorder) code() int {
	if s.status == 0 {
		return http.StatusOK
	}
	return s.status
}

// agentPeerViewRefusal is the receiving node's check of an agent principal's
// request on pattern, before any rule runs: the peer's agent view policy,
// the shared allowlist and, for a settings update, its fields. It returns
// the status and reason to answer, or 0.
func agentPeerViewRefusal(instanceID, pattern string, r *http.Request) (int, string, string) {
	policy, err := db.GetFederationPeerAgentViews(instanceID)
	if err != nil {
		return http.StatusServiceUnavailable, "peer_unavailable", "could not read the agent view policy"
	}
	if policy != db.FederationPeerAgentViewsAllow {
		return http.StatusForbidden, "agent_views_off", "this node does not serve peer views to your agents"
	}
	spec, ok := agentPeerViewRoutes[pattern]
	if !ok {
		return http.StatusForbidden, "agent_principal", "this route is the remote operator's; an agent may not use it"
	}
	if spec.fields != nil {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			return http.StatusBadRequest, "invalid_arg", "could not read request body"
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		if _, err := agentPeerViewRouteSlugs(spec, raw); err != nil {
			return http.StatusForbidden, "agent_field_not_allowed", err.Error()
		}
	}
	return 0, "", ""
}
