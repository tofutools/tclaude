package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
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
//   - On the sending node, the agent holds the route's slugs with an explicit
//     peer=<inst>[/<group>] scope covering the route's footprint on the peer
//     (servePeerViewForAgent). Unscoped grants, group grants without a peer
//     and agent defaults never count, whatever this node's trust in the peer.
//   - On the receiving node, the request runs under an agent principal: the
//     peer's own grants still decide, as for the peer's operator, but an
//     agent principal never takes a local-operator path, not even from an
//     unrestricted peer; the request may touch only the footprint the sender
//     checked; and the receiving operator can turn agent views off for the
//     peer (tclaude federation agent-views).
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
	// slugs are what the sending agent holds, peer-scoped, all of them. A
	// group settings update uses fields instead.
	slugs  []string
	target agentPeerViewTarget
	// fields maps the body fields an agent may send to the slug each needs;
	// any other field is refused (operator-only over a peer view).
	fields map[string]string
	// intoGroup marks a route whose body may name a further group the action
	// lands in (a clone's "group"), which joins the footprint.
	intoGroup bool
	// destructive marks a route that ends sessions; granting its slugs warns.
	destructive bool
	// runScope marks a process run start, whose footprint and slug come
	// from the body's scope (agentPeerViewRunScope); slugs lists both.
	runScope bool
	// cronScope marks a cron job create, whose footprint and slug come from
	// the body's target: group:<name> (groups.messages.schedule there) or an
	// agent (agent.schedule, in every group the agent is in).
	cronScope bool
}

// agentPeerViewRunScope resolves a run start's route to the scope its body
// names: a group (process.runs.start there) or one agent (agent.process, in
// every group the agent is in). Any other route is returned as it is.
func agentPeerViewRunScope(route agentPeerViewRoute, values map[string]string, body []byte) (agentPeerViewRoute, map[string]string, error) {
	if route.cronScope {
		var in struct {
			Target string `json:"target"`
		}
		_ = json.Unmarshal(body, &in)
		out := maps.Clone(values)
		if out == nil {
			out = map[string]string{}
		}
		target := strings.TrimSpace(in.Target)
		if name, ok := strings.CutPrefix(target, "group:"); ok && strings.TrimSpace(name) != "" {
			route.target, route.slugs, out["name"] = agentViewGroup, []string{PermGroupsMessagesSchedule}, strings.TrimSpace(name)
			return route, out, nil
		}
		if target == "" || strings.HasPrefix(target, "group:") {
			return route, values, fmt.Errorf("a job an agent schedules on a peer targets group:<name> or one agent")
		}
		route.target, route.slugs, out["id"] = agentViewAgent, []string{PermAgentSchedule}, target
		return route, out, nil
	}
	if !route.runScope {
		return route, values, nil
	}
	var in struct {
		Group string `json:"group"`
		Agent string `json:"agent"`
	}
	_ = json.Unmarshal(body, &in)
	group, agent := strings.TrimSpace(in.Group), strings.TrimSpace(in.Agent)
	out := maps.Clone(values)
	if out == nil {
		out = map[string]string{}
	}
	switch {
	case group != "" && agent == "":
		route.target, route.slugs, out["name"] = agentViewGroup, []string{PermProcessRunsStart}, group
	case agent != "" && group == "":
		route.target, route.slugs, out["id"] = agentViewAgent, []string{PermAgentProcess}, agent
	default:
		return route, values, fmt.Errorf("a run an agent starts on a peer is scoped to one group or one agent")
	}
	return route, out, nil
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
	"GET /api/snapshot":      {slugs: []string{PermAgentsStatusRead}, target: agentViewNode},
	"GET /api/groups":        {slugs: []string{PermAgentsStatusRead}, target: agentViewNode},
	"GET /api/groups/{name}": {slugs: []string{PermAgentsStatusRead}, target: agentViewGroup},
	"GET /api/agents/{id}":   {slugs: []string{PermAgentsStatusRead}, target: agentViewAgent},
	"GET /api/fleet":         {slugs: []string{PermFleetRead}, target: agentViewNode},
	// Member lifecycle. A restart stops the session first, so it needs the
	// stop grant too, as the receiver does.
	"POST /api/agents/{id}/stop":    {slugs: []string{PermAgentStop}, target: agentViewAgent, destructive: true},
	"POST /api/agents/{id}/resume":  {slugs: []string{PermAgentResume}, target: agentViewAgent},
	"POST /api/agents/{id}/restart": {slugs: []string{PermAgentResume, PermAgentStop}, target: agentViewAgent, destructive: true},
	"POST /api/agents/{id}/retire":  {slugs: []string{PermAgentRetire}, target: agentViewAgent, destructive: true},
	"POST /api/agents/{id}/clone":   {slugs: []string{PermAgentClone}, target: agentViewAgent, intoGroup: true},
	// Member settings. Notify has no slug of its own; the task link's
	// covers it, as both are the agent's own dashboard settings.
	"POST /api/agents/{id}/rename":            {slugs: []string{PermAgentRename}, target: agentViewAgent},
	"POST /api/agents/{id}/tags":              {slugs: []string{PermAgentTags}, target: agentViewAgent},
	"POST /api/agents/{id}/task":              {slugs: []string{PermAgentTask}, target: agentViewAgent},
	"POST /api/agents/{id}/notify":            {slugs: []string{PermAgentTask}, target: agentViewAgent},
	"PATCH /api/groups/{name}/members/{conv}": {slugs: []string{PermGroupsMembersUpdate}, target: agentViewGroup},
	// Member controls. No text of the agent's is typed into the target's
	// pane: the receiver refuses a follow-up, a focus hint or a forced
	// reincarnation from a peer.
	"POST /api/agents/{id}/interrupt":   {slugs: []string{PermAgentInterrupt}, target: agentViewAgent},
	"POST /api/agents/{id}/compact":     {slugs: []string{PermAgentCompact}, target: agentViewAgent},
	"POST /api/agents/{id}/reincarnate": {slugs: []string{PermAgentReincarnate}, target: agentViewAgent},
	"GET /api/agents/{id}/context-info": {slugs: []string{PermAgentContextInfo}, target: agentViewAgent},
	"GET /api/agents/{id}/prs":          {slugs: []string{PermAgentPR}, target: agentViewAgent},
	"POST /api/agents/{id}/prs":         {slugs: []string{PermAgentPR}, target: agentViewAgent},
	// Group lifecycle.
	"POST /api/groups/{name}/shutdown": {slugs: []string{PermGroupsMembersStop}, target: agentViewGroup, destructive: true},
	"POST /api/groups/{name}/power-on": {slugs: []string{PermGroupsMembersResume}, target: agentViewGroup},
	// Group settings that do not shape future members.
	"PATCH /api/groups/{name}": {target: agentViewGroup, fields: map[string]string{
		"descr":          PermGroupsSettingsDescription,
		"max_members":    PermGroupsSettingsMaxMembers,
		"notify_enabled": PermGroupsSettingsNotifications,
	}},
	"POST /api/groups/{name}/rename":     {slugs: []string{PermGroupsRename}, target: agentViewGroup},
	"GET /api/groups/{name}/attachment":  {slugs: []string{PermGroupsAttachment}, target: agentViewGroup},
	"POST /api/groups/{name}/attachment": {slugs: []string{PermGroupsAttachment}, target: agentViewGroup},
	// Process runs (process_peer_runs.go): reads node-wide and redacted; a
	// start scoped to a group or one agent, owned by the agent's node.
	"GET /v1/process/runs":             {slugs: []string{PermProcessRunsRead}, target: agentViewNode},
	"GET /v1/process/runs/{id}":        {slugs: []string{PermProcessRunsRead}, target: agentViewNode},
	"GET /v1/process/runs/{id}/events": {slugs: []string{PermProcessRunsRead}, target: agentViewNode},
	"POST /v1/process/runs":            {slugs: []string{PermProcessRunsStart, PermAgentProcess}, runScope: true},
	// Manage verbs on program-free runs (process_peer_manage.go). An agent
	// never decides, a decision being a human gate, and never assigns: the
	// assignee is in the body, outside the footprint its node checked.
	"POST /v1/process/runs/{id}/pause":           {slugs: []string{PermProcessRunsManage}, target: agentViewNode},
	"POST /v1/process/runs/{id}/resume":          {slugs: []string{PermProcessRunsManage}, target: agentViewNode},
	"POST /v1/process/runs/{id}/resolve-blocked": {slugs: []string{PermProcessRunsManage}, target: agentViewNode},
	// Cron jobs (peer_cron_jobs.go): an agent schedules messages under its
	// target's grant, and lists, changes, fires, stops and deletes only the
	// jobs it scheduled, under self.schedule for the peer.
	"POST /v1/cron":                           {slugs: []string{PermGroupsMessagesSchedule, PermAgentSchedule}, cronScope: true},
	"GET /v1/cron":                            {slugs: []string{PermSelfSchedule}, target: agentViewNode},
	"GET /v1/cron/{id}/logs":                  {slugs: []string{PermSelfSchedule}, target: agentViewNode},
	"PATCH /v1/cron/{id}":                     {slugs: []string{PermSelfSchedule}, target: agentViewNode},
	"DELETE /v1/cron/{id}":                    {slugs: []string{PermSelfSchedule}, target: agentViewNode},
	"POST /v1/cron/{id}/enable":               {slugs: []string{PermSelfSchedule}, target: agentViewNode},
	"POST /v1/cron/{id}/disable":              {slugs: []string{PermSelfSchedule}, target: agentViewNode},
	"POST /v1/cron/{id}/run-now":              {slugs: []string{PermSelfSchedule}, target: agentViewNode},
	"GET /api/groups/{name}/process":          {slugs: []string{PermAgentsStatusRead}, target: agentViewGroup},
	"POST /api/groups/{name}/process/advance": {slugs: []string{PermProcessAdvance}, target: agentViewGroup},
	// A group's task board (peer_taskboard.go). The group routes are held per
	// group; a route addressed by a task id names no group in its path, so it
	// takes a node-wide grant here and the receiver's per-group peer grants
	// decide the task's own group.
	"GET /v1/groups/{name}/taskboard":             {slugs: []string{PermTaskboardRead}, target: agentViewGroup},
	"POST /v1/groups/{name}/taskboard/tasks":      {slugs: []string{PermTaskboardWrite}, target: agentViewGroup},
	"POST /v1/groups/{name}/taskboard/ask":        {slugs: []string{PermTaskboardWrite}, target: agentViewGroup},
	"GET /v1/taskboard/context":                   {slugs: []string{PermTaskboardRead}, target: agentViewNode},
	"GET /v1/taskboard/tasks/{id}":                {slugs: []string{PermTaskboardRead}, target: agentViewNode},
	"GET /v1/taskboard/tasks/{id}/events":         {slugs: []string{PermTaskboardRead}, target: agentViewNode},
	"PATCH /v1/taskboard/tasks/{id}":              {slugs: []string{PermTaskboardWrite}, target: agentViewNode},
	"POST /v1/taskboard/tasks/{id}/move":          {slugs: []string{PermTaskboardWrite}, target: agentViewNode},
	"POST /v1/taskboard/tasks/{id}/stage":         {slugs: []string{PermTaskboardWrite}, target: agentViewNode},
	"POST /v1/taskboard/tasks/{id}/status":        {slugs: []string{PermTaskboardWrite}, target: agentViewNode},
	"POST /v1/taskboard/tasks/{id}/move-to-group": {slugs: []string{PermTaskboardTransfer}, target: agentViewNode},
	"POST /v1/taskboard/tasks/{id}/assign":        {slugs: []string{PermTaskboardAssign}, target: agentViewNode},
	"POST /v1/taskboard/tasks/{id}/unassign":      {slugs: []string{PermTaskboardAssign}, target: agentViewNode},
	"GET /v1/groups/{name}/taskboard/writers":     {slugs: []string{PermTaskboardRead}, target: agentViewGroup},
	"POST /v1/groups/{name}/taskboard/writers":    {slugs: []string{PermTaskboardManage}, target: agentViewGroup},
}

// agentPeerViewSlugs lists the sender slugs agentPeerViewRoutes uses, which
// accept a peer= scope (initAgentPeerViewScopes).
func agentPeerViewSlugs() []string {
	var out []string
	for _, route := range agentPeerViewRoutes {
		out = append(out, route.slugs...)
		for _, slug := range route.fields {
			out = append(out, slug)
		}
	}
	sort.Strings(out)
	return slices.Compact(out)
}

// agentPeerViewDestructive reports a slug that opens an agent peer view
// route ending sessions on the peer, so a peer-scoped grant of it warns.
func agentPeerViewDestructive(slug string) bool {
	for _, route := range agentPeerViewRoutes {
		if route.destructive && slices.Contains(route.slugs, slug) {
			return true
		}
	}
	return false
}

// agentPeerViewDestructiveWarning is the warning a peer-scoped grant of a
// session-ending slug carries.
func agentPeerViewDestructiveWarning(slug, peer string) string {
	return fmt.Sprintf("%s lets the holder end sessions on %s through a peer view (where that node's grants allow it)", slug, peer)
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

// agentPeerViewRouteSlugs is the slugs a request on route needs: its slugs,
// or for a settings update one per body field. A field the route does not
// list is refused, naming it.
func agentPeerViewRouteSlugs(route agentPeerViewRoute, body []byte) ([]string, error) {
	if route.fields == nil {
		return route.slugs, nil
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

// agentPeerViewIntoGroup is the further group a route's body names for the
// action to land in, if the route takes one ("" when none).
func agentPeerViewIntoGroup(route agentPeerViewRoute, body []byte) string {
	if !route.intoGroup || len(bytes.TrimSpace(body)) == 0 {
		return ""
	}
	var in struct {
		Group string `json:"group"`
	}
	_ = json.Unmarshal(body, &in)
	return strings.TrimSpace(in.Group)
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
	// agentPeerViewSweepAt is the tracked agent count past which idle ones
	// are forgotten.
	agentPeerViewSweepAt = 128
)

// agentPeerViewNow is the limiter's clock; tests replace it.
var agentPeerViewNow = time.Now

type agentPeerViewLimiter struct {
	mu        sync.Mutex
	attempts  map[string][]time.Time
	inFlight  map[string]int
	refusedAt map[string]time.Time
}

var agentPeerViewLimits = &agentPeerViewLimiter{attempts: map[string][]time.Time{}, inFlight: map[string]int{}, refusedAt: map[string]time.Time{}}

// admit counts an attempt and reports whether it may run (reason ""); release
// ends an admitted one. A refusal reports audit for the first rate refusal of
// a window only, so a looping agent cannot flood the audit log.
func (l *agentPeerViewLimiter) admit(agentID string) (release func(), reason string, audit bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := agentPeerViewNow()
	// Forget agents idle past the window, so retired agents do not
	// accumulate; the limit only ever looks one window back.
	if len(l.attempts) > agentPeerViewSweepAt {
		for id, at := range l.attempts {
			if id != agentID && (len(at) == 0 || now.Sub(at[len(at)-1]) >= agentPeerViewRateWindow) {
				delete(l.attempts, id)
				delete(l.refusedAt, id)
			}
		}
	}
	var recent []time.Time
	for _, at := range l.attempts[agentID] {
		if now.Sub(at) < agentPeerViewRateWindow {
			recent = append(recent, at)
		}
	}
	if len(recent) >= agentPeerViewRateLimit {
		l.attempts[agentID] = recent
		last, seen := l.refusedAt[agentID]
		audit = !seen || now.Sub(last) >= agentPeerViewRateWindow
		if audit {
			l.refusedAt[agentID] = now
		}
		return nil, "rate", audit
	}
	l.attempts[agentID] = append(recent, now)
	if l.inFlight[agentID] >= agentPeerViewInFlight {
		return nil, "concurrency", true
	}
	l.inFlight[agentID]++
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.inFlight[agentID]--; l.inFlight[agentID] <= 0 {
			delete(l.inFlight, agentID)
		}
	}, "", true
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

// servePeerViewForAgent is the sending node's gate for an agent's peer view
// of the peer node names. The limit and the audit come before the peer is
// resolved, so an agent cannot probe which peers exist uncounted.
func servePeerViewForAgent(w http.ResponseWriter, r *http.Request, caller, node string) {
	agentID, err := db.AgentIDForConv(caller)
	if err != nil || !proto.ValidAgentRef(agentID) {
		writeError(w, http.StatusForbidden, "auth", "a peer view needs a registered agent")
		return
	}
	principal := &proto.PeerViewAgent{ID: agentID, Name: proto.SafeName(agent.TitleFor(caller), false)}
	route := ""
	status := 0
	audit := true
	peerRef := proto.SafeName(node, false)
	defer func() {
		if !audit {
			return
		}
		_, _ = db.InsertAuditLog(db.AuditLogEntry{At: time.Now(), ActorKind: db.AuditActorAgent, ActorConv: caller, ActorAgent: agentID, ActorLabel: principal.Name,
			Verb: "federation.peer_view.agent.out", Detail: fmt.Sprintf("peer=%s route=%s", peerRef, route), Method: r.Method, Status: status, Source: "federation"})
	}()
	refuse := func(code int, reason, msg string) {
		status = code
		writeError(w, code, reason, msg)
	}
	release, limited, auditRefusal := agentPeerViewLimits.admit(agentID)
	if limited != "" {
		audit = auditRefusal
		route = "(" + limited + " limited)"
		refuse(http.StatusTooManyRequests, "agent_rate_limited", "too many peer views from this agent; wait and retry")
		return
	}
	defer release()
	// Only operator-chosen labels and pinned identities select a destination.
	peer, err := resolveFederationPeerOpt(node, false)
	if err != nil {
		status = http.StatusNotFound
		writeFedErr(w, err)
		return
	}
	peerRef = peer.InstanceID
	r.SetPathValue("node", peer.InstanceID)
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
	// The body stays readable for an approval popup's preview and the proxy.
	r.Body = io.NopCloser(bytes.NewReader(body))
	spec, values, err := agentPeerViewRunScope(agentPeerViewRoutes[pattern], values, body)
	if err != nil {
		refuse(http.StatusForbidden, "agent_run_scope", err.Error())
		return
	}
	slugs, err := agentPeerViewRouteSlugs(spec, body)
	if err != nil {
		refuse(http.StatusForbidden, "agent_field_not_allowed", err.Error())
		return
	}
	groups, unknown, ok := agentPeerViewFootprint(cat, spec, values, body)
	if !ok {
		refuse(http.StatusForbidden, "unknown_footprint", agentPeerViewUnknownFootprint(node, unknown, slugs))
		return
	}
	// Every (slug, group) needs an explicit peer= grant. One gap may still be
	// approved once by the human; more are refused, so a popup never stands
	// for groups it does not name.
	type need struct{ slug, group string }
	var missing []need
	for _, slug := range slugs {
		for _, group := range groups {
			actx := ActionContext{RemotePeer: peer.InstanceID, RemoteGroup: group, strictPeerScope: true}
			allowed, _, err := permissionAllowsAction(r, caller, slug, actx)
			if err != nil {
				refuse(http.StatusServiceUnavailable, "permissions_unavailable", "could not read permissions")
				return
			}
			if !allowed {
				missing = append(missing, need{slug, group})
			}
		}
	}
	if len(missing) > 1 {
		var names []string
		for _, m := range missing {
			names = append(names, m.slug+" for peer="+peer.InstanceID+"/"+m.group)
		}
		refuse(http.StatusForbidden, "permission", "requires "+strings.Join(names, ", "))
		return
	}
	if len(missing) == 1 {
		gate := &statusRecorder{ResponseWriter: w}
		actx := ActionContext{RemotePeer: peer.InstanceID, RemoteGroup: missing[0].group, strictPeerScope: true}
		if _, ok := requirePermission(gate, r, missing[0].slug, actx); !ok {
			status = gate.code()
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
	}
	// Only a receiver that knows agent principals may see one: an older one
	// would serve the request as this node's operator's. The receiver also
	// confirms the principal in its answer (openPeerViewOnce).
	if !cat.PeerViewAgents {
		refuse(http.StatusConflict, "peer_lacks_agent_views", "the peer does not serve agent peer views; its operator must upgrade it")
		return
	}
	for _, g := range groups {
		if g != "" {
			principal.Groups = append(principal.Groups, g)
		}
	}
	// What a peer returns is untrusted content for the agent reading it.
	w.Header().Set("X-Tclaude-Remote-Content", "untrusted")
	rec := &statusRecorder{ResponseWriter: w}
	servePeerViewProxyWith(rec, r.WithContext(withPeerViewPrincipal(r.Context(), principal)), false)
	status = rec.code()
}

// agentPeerViewFootprint is the remote groups a route acts in, as the
// sending node's latest catalog of the peer shows them: "" for the whole
// view, the named group, or every group the target agent is listed in, plus
// any group the body names for the action to land in. It is not ok when a
// group is unknown or the catalog lists the target in none. The receiver
// refuses anything outside the footprint the sender sends, so a stale or
// partial catalog makes a request fail there rather than widen. When a named
// group is what the catalog lacks, unknown names it.
func agentPeerViewFootprint(cat *proto.CatalogPayload, route agentPeerViewRoute, values map[string]string, body []byte) (groups []string, unknown string, ok bool) {
	known := func(name string) bool {
		return slices.ContainsFunc(cat.Groups, func(g proto.CatalogGroup) bool { return g.Name == name })
	}
	var out []string
	switch route.target {
	case agentViewNode:
		return []string{""}, "", true
	case agentViewGroup:
		if !known(values["name"]) {
			return nil, values["name"], false
		}
		out = []string{values["name"]}
	case agentViewAgent:
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
		if len(out) == 0 {
			return nil, "", false
		}
	}
	if into := agentPeerViewIntoGroup(route, body); into != "" && !slices.Contains(out, into) {
		if !known(into) {
			return nil, into, false
		}
		out = append(out, into)
	}
	return out, "", true
}

// agentPeerViewUnknownFootprint explains a footprint the peer's catalog does
// not show. A group missing from it is one the peer grants this node nothing
// on (or a catalog older than the grant), so the message names the grant the
// peer's operator would give. The group is the one the agent itself named.
func agentPeerViewUnknownFootprint(node, group string, slugs []string) string {
	if group == "" {
		return "the peer's catalog does not show every group this touches"
	}
	slug := "<slug>"
	if len(slugs) > 0 {
		slug = slugs[0]
	}
	return fmt.Sprintf("%s has not granted this node anything on group %q (or its catalog here predates the grant). "+
		"Its operator grants it there with: tclaude federation grant <this node> %s --scope group=%s",
		node, group, slug, group)
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
// the shared allowlist, a settings update's fields, and that the request
// touches no group outside the footprint the sender checked. It returns the
// status and reason to answer, or 0.
func agentPeerViewRefusal(instanceID, pattern string, r *http.Request, principal *proto.PeerViewAgent) (int, string, string) {
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
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return http.StatusBadRequest, "invalid_arg", "could not read request body"
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	spec, values, err := agentPeerViewRunScope(spec, map[string]string{"name": r.PathValue("name"), "id": r.PathValue("id")}, raw)
	if err != nil {
		return http.StatusForbidden, "agent_run_scope", err.Error()
	}
	if spec.fields != nil {
		if _, err := agentPeerViewRouteSlugs(spec, raw); err != nil {
			return http.StatusForbidden, "agent_field_not_allowed", err.Error()
		}
	}
	var touched []string
	switch spec.target {
	case agentViewGroup:
		touched = append(touched, values["name"])
	case agentViewAgent:
		// An unknown target is the rule's own 404.
		if target, err := db.GetAgent(values["id"]); err == nil && target != nil && target.CurrentConvID != "" {
			groups, err := db.ListGroupsForConv(target.CurrentConvID)
			if err != nil {
				return http.StatusServiceUnavailable, "snapshot_unavailable", "could not read the target's groups"
			}
			for _, g := range groups {
				if !g.IsArchived() && fedPeerGroupVisible(instanceID, g.ID) {
					touched = append(touched, g.Name)
				}
			}
		}
	}
	if into := agentPeerViewIntoGroup(spec, raw); into != "" {
		touched = append(touched, into)
	}
	for _, name := range touched {
		if !slices.Contains(principal.Groups, name) {
			return http.StatusForbidden, "agent_footprint", fmt.Sprintf("this touches group:%s, which your node did not check the agent's grants for", name)
		}
	}
	return 0, "", ""
}

// agentPeerViewMetadataScope is the features and peer grants agent peer view
// routes reach, which an agent principal's view metadata is limited to.
func agentPeerViewMetadataScope() (features, slugs map[string]bool) {
	features, slugs = map[string]bool{}, map[string]bool{}
	rules := peerViewRules()
	for pattern, route := range agentPeerViewRoutes {
		if rule, ok := rules[pattern]; ok {
			features[rule.feature] = true
			slugs[rule.requires] = true
			if route.runScope {
				// An agent-bound start is answered by the member grant.
				slugs[PermGroupsMembersProcess] = true
			}
			if route.cronScope {
				// An agent-targeted job is answered by the member grant.
				slugs[PermGroupsMembersSchedule] = true
			}
		}
	}
	return features, slugs
}

// footprintGrantLists limits an agent principal's grant lists to the groups
// its node checked its grants for and the agents in them: an agent learns
// nothing of the groups outside its footprint. A node-wide route's footprint
// has no groups, so it lists none.
func footprintGrantLists(principal *proto.PeerViewAgent, groups []*db.AgentGroup, groupGrants, agentGrants map[string][]string) (map[string][]string, map[string][]string) {
	if len(principal.Groups) == 0 {
		return nil, nil
	}
	outGroups := map[string][]string{}
	for name, held := range groupGrants {
		if slices.Contains(principal.Groups, name) {
			outGroups[name] = held
		}
	}
	if len(agentGrants) == 0 {
		return outGroups, agentGrants
	}
	var ids []int64
	for _, g := range groups {
		if !g.IsArchived() && slices.Contains(principal.Groups, g.Name) {
			ids = append(ids, g.ID)
		}
	}
	members, err := db.ListAgentGroupMembersBatch(ids)
	if err != nil {
		return outGroups, nil
	}
	outAgents := map[string][]string{}
	for _, id := range ids {
		for _, m := range members[id] {
			if aid, err := db.AgentIDForConv(m.ConvID); err == nil && agentGrants[aid] != nil {
				outAgents[aid] = agentGrants[aid]
			}
		}
	}
	return outGroups, outAgents
}

// filterGrantLists keeps, per key, only the grants in keep.
func filterGrantLists(in map[string][]string, keep map[string]bool) map[string][]string {
	if in == nil {
		return nil
	}
	out := map[string][]string{}
	for k, held := range in {
		var kept []string
		for _, slug := range held {
			if keep[slug] {
				kept = append(kept, slug)
			}
		}
		if len(kept) > 0 {
			out[k] = kept
		}
	}
	return out
}
