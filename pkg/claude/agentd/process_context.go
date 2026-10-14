package agentd

import (
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/strictjson"
)

// process_context.go is the read model of runs in context (tcl-f2cpfb): for
// each group its active, needs-you and planned runs; for each agent the runs
// it takes part in with its place in each. The Groups, Terminals and editor
// overlays poll GET /v1/process/context alongside the dashboard snapshot.
//
// It is bounded at every level: at most db.MaxProcessRunContextRuns scoped
// runs are examined per read (most recently updated first), each group lists
// at most processContextPerGroup runs per bucket, and each run projects at
// most processProjectionMaxNodes authored nodes. Every run id a group or
// agent entry names resolves in the top-level runs map, so a consumer gets
// one full mini-graph projection per run without duplication.

const (
	processContextPerGroup = 8
	// processContextActivePerScope is how many non-terminal runs one group or
	// agent contributes per read: room for a full active and needs-you bucket.
	processContextActivePerScope = 2 * processContextPerGroup
	processProjectionMaxNodes    = 48
	processProjectionCacheSize   = 256
)

// processRunProjection is the mini-graph a run renders as in context.
type processRunProjection struct {
	RunID       string                  `json:"runId"`
	TemplateID  string                  `json:"templateId"`
	TemplateRef string                  `json:"templateRef"`
	Name        string                  `json:"name"`
	Status      engine.RunStatus        `json:"status"`
	Action      string                  `json:"action"`
	NeedsYou    bool                    `json:"needsYou"`
	Scope       processRunScopeView     `json:"scope"`
	Paused      *processRunPauseView    `json:"paused,omitempty"`
	Nodes       []processProjectionNode `json:"nodes"`
	Edges       []processProjectionEdge `json:"edges"`
	Current     []string                `json:"current"`
	Truncated   bool                    `json:"truncated,omitempty"`
	StartedAt   time.Time               `json:"startedAt"`
	UpdatedAt   time.Time               `json:"updatedAt"`
	stepOrder   map[string]int          // node id → 1-based work-step index
	steps       int                     // work steps in the projection
}

// processProjectionNode is one authored node. Kind is the template node type
// verbatim (task, decision, wait, parallel, start, end, …); a task's
// performer kind says whether an agent, a human or a program performs it.
// State is the engine node state, except a decision waiting on a human
// reads "awaiting". Index is the node's 1-based work-step position (tasks,
// decisions and waits in start-first breadth order), 0 for structural nodes.
type processProjectionNode struct {
	ID        string                     `json:"id"`
	Label     string                     `json:"label"`
	Kind      string                     `json:"kind"`
	State     string                     `json:"state"`
	Index     int                        `json:"index,omitempty"`
	Performer processProjectionPerformer `json:"performer"`
	// Dispatch is the agent node's latest attempt as performer dispatch
	// recorded it: who has it, which attempt (the loop count), and when the
	// turn was injected. Absent until the node is first dispatched.
	Dispatch *processProjectionDispatch `json:"dispatch,omitempty"`
	// Description is the authored node description, clipped to
	// processProjectionDescriptionRunes so a run's projection stays small.
	Description string `json:"description,omitempty"`
}

const processProjectionDescriptionRunes = 160

func clipProjectionText(text string, limit int) string {
	text = strings.TrimSpace(text)
	if runes := []rune(text); len(runes) > limit {
		return strings.TrimSpace(string(runes[:limit-1])) + "…"
	}
	return text
}

// processProjectionPerformer names who performs a node. Kind is agent,
// human, program or engine. Ref is, for an agent node, the agent its latest
// attempt is dispatched to, else the bound agent_id of an agent-scoped run,
// else "unassigned"; for a program node it is the program profile.
type processProjectionPerformer struct {
	Kind string `json:"kind"`
	Ref  string `json:"ref,omitempty"`
}

// processProjectionDispatch is one agent node's latest dispatched attempt.
type processProjectionDispatch struct {
	// NodeID is the executable node the attempt was dispatched under: the
	// authored id for a plain task, a stage or body id otherwise. Acting on
	// the attempt (reassign) names this id.
	NodeID      string     `json:"nodeId"`
	AgentID     string     `json:"agentId,omitempty"`
	Name        string     `json:"name,omitempty"`
	Attempt     int        `json:"attempt"`
	State       string     `json:"state"`
	WaitReason  string     `json:"waitReason,omitempty"`
	DeliveredAt *time.Time `json:"deliveredAt,omitempty"`
}

// processProjectionEdge is one authored (from, label) → to edge with its
// run-time disposition. Back marks an edge into a node at or before its
// source in start-first order — a retry or loop.
type processProjectionEdge struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Label string `json:"label"`
	State string `json:"state"`
	Back  bool   `json:"back,omitempty"`
}

type processContextView struct {
	Runs      map[string]processRunProjection `json:"runs"`
	Groups    []processGroupContext           `json:"groups"`
	Agents    []processAgentContext           `json:"agents"`
	Orphaned  []string                        `json:"orphaned"`
	Truncated bool                            `json:"truncated,omitempty"`
}

// processGroupContext lists a group's runs by run id. Active excludes runs
// that need a human, which are in NeedsYou instead. Done holds the most
// recently finished runs. Planned stays empty until automations can start
// process runs.
type processGroupContext struct {
	GroupID       int64               `json:"groupId"`
	Group         string              `json:"group"`
	Active        []string            `json:"active"`
	NeedsYou      []string            `json:"needsYou"`
	Done          []string            `json:"done"`
	Planned       []processPlannedRun `json:"planned"`
	ActiveCount   int                 `json:"activeCount"`
	NeedsYouCount int                 `json:"needsYouCount"`
}

type processPlannedRun struct {
	Source     string    `json:"source"`
	ID         string    `json:"id"`
	TemplateID string    `json:"templateId"`
	Name       string    `json:"name"`
	NextAt     time.Time `json:"nextAt"`
}

type processAgentContext struct {
	AgentID string                `json:"agentId"`
	Runs    []processAgentRunStep `json:"runs"`
}

// processAgentRunStep places an agent in one run: its current node there and
// that node's index out of the run's work steps. A run with no current node
// reports its last step.
type processAgentRunStep struct {
	RunID  string `json:"runId"`
	NodeID string `json:"nodeId,omitempty"`
	Index  int    `json:"index"`
	Total  int    `json:"total"`
	State  string `json:"state"`
}

// processContextNeedsYou is the set of run actions that wait on a human.
var processContextNeedsYou = map[string]bool{
	"awaiting_decision": true, "blocked": true,
	processCommandNeedsReconcile: true, "orphaned": true,
	// Performer dispatch (tcl-09xlwr) parks a node without a resolvable
	// performer under this action.
	"needs_performer": true,
}

func handleProcessContext(w http.ResponseWriter, r *http.Request) {
	visible, ok := processContextVisibility(w, r)
	if !ok {
		return
	}
	view, err := buildProcessContext(visible)
	if err != nil {
		writeProcessRuntimeError(w, err)
		return
	}
	writeProcessJSON(w, http.StatusOK, view)
}

// processContextVisible decides which runs a caller sees. all is the human
// and anyone holding process.runs.read (group owners hold it); otherwise an
// agent sees its own groups' runs and the runs bound to itself.
type processContextVisible struct {
	all     bool
	groups  map[int64]bool
	agentID string
}

// query pushes the visibility into the store read, so a busy scope the
// caller cannot see never takes the place of one it can.
func (v processContextVisible) query(perScope int) db.ProcessRunContextQuery {
	query := db.ProcessRunContextQuery{All: v.all, AgentID: v.agentID, PerScope: perScope}
	for id := range v.groups {
		query.GroupIDs = append(query.GroupIDs, id)
	}
	sort.Slice(query.GroupIDs, func(i, j int) bool { return query.GroupIDs[i] < query.GroupIDs[j] })
	return query
}

func (v processContextVisible) allows(scope db.ProcessRunScope) bool {
	switch {
	case v.all:
		return true
	case scope.Kind == db.ProcessRunScopeGroup:
		return v.groups[scope.GroupID]
	case scope.Kind == db.ProcessRunScopeAgent:
		return v.agentID != "" && scope.AgentID == v.agentID
	}
	return false
}

func processContextVisibility(w http.ResponseWriter, r *http.Request) (processContextVisible, bool) {
	p := peerFromContext(r.Context())
	switch classify(p) {
	case classHuman:
		return processContextVisible{all: true}, true
	case classAgent:
	default:
		// Unidentified and unconfirmed callers get the ordinary gate's answer.
		_, ok := requirePermission(w, r, PermProcessRunsRead)
		return processContextVisible{all: ok}, ok
	}
	if allowed, _, err := permissionAllowsAction(r, p.ConvID, PermProcessRunsRead, ActionContext{}); err == nil && allowed {
		return processContextVisible{all: true}, true
	}
	agentID := peerAgentID(p.ConvID)
	groups, err := db.ListGroupsForAgent(agentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "process_context", err.Error())
		return processContextVisible{}, false
	}
	visible := processContextVisible{groups: make(map[int64]bool, len(groups)), agentID: agentID}
	for _, g := range groups {
		visible.groups[g.ID] = true
	}
	return visible, true
}

func buildProcessContext(visible processContextVisible) (processContextView, error) {
	view := processContextView{
		Runs: map[string]processRunProjection{}, Groups: []processGroupContext{},
		Agents: []processAgentContext{}, Orphaned: []string{},
	}
	live := newProcessLiveness()
	candidates, err := db.ListScopedActiveProcessRuns(visible.query(processContextActivePerScope))
	if err != nil {
		return view, err
	}
	view.Truncated = len(candidates) == db.MaxProcessRunContextRuns
	groups := map[int64]*processGroupContext{}
	agents := map[string]*processAgentContext{}
	for i := range candidates {
		if !visible.allows(candidates[i].Scope) {
			continue
		}
		projection, err := projectProcessRun(candidates[i].ID, live)
		if err != nil {
			// One unreadable run must not blank every overlay.
			slog.Warn("process context: run projection failed", "run", candidates[i].ID, "error", err)
			continue
		}
		scope := candidates[i].Scope
		if scope.Orphaned() {
			if len(view.Orphaned) < processContextPerGroup {
				view.Orphaned = append(view.Orphaned, projection.RunID)
				view.Runs[projection.RunID] = projection
			}
			continue
		}
		switch scope.Kind {
		case db.ProcessRunScopeGroup:
			if scope.GroupName == "" {
				// The group is gone but the orphan catch-up has not run yet.
				continue
			}
			g := groups[scope.GroupID]
			if g == nil {
				g = newProcessGroupContext(scope)
				groups[scope.GroupID] = g
			}
			bucket, count := &g.Active, &g.ActiveCount
			if projection.NeedsYou {
				bucket, count = &g.NeedsYou, &g.NeedsYouCount
			}
			*count++
			if len(*bucket) < processContextPerGroup {
				*bucket = append(*bucket, projection.RunID)
				view.Runs[projection.RunID] = projection
			}
		case db.ProcessRunScopeAgent:
			a := agents[scope.AgentID]
			if a == nil {
				a = &processAgentContext{AgentID: scope.AgentID, Runs: []processAgentRunStep{}}
				agents[scope.AgentID] = a
			}
			if len(a.Runs) < processContextPerGroup {
				a.Runs = append(a.Runs, projection.agentStep())
				view.Runs[projection.RunID] = projection
			}
		}
	}
	done, err := db.ListRecentTerminalGroupProcessRuns(visible.query(processContextPerGroup))
	if err != nil {
		return view, err
	}
	for i := range done {
		scope := done[i].Scope
		if !visible.allows(scope) || scope.GroupName == "" {
			continue
		}
		g := groups[scope.GroupID]
		if g == nil {
			g = newProcessGroupContext(scope)
			groups[scope.GroupID] = g
		}
		if len(g.Done) >= processContextPerGroup {
			continue
		}
		projection, err := projectProcessRun(done[i].ID, live)
		if err != nil {
			slog.Warn("process context: run projection failed", "run", done[i].ID, "error", err)
			continue
		}
		g.Done = append(g.Done, projection.RunID)
		view.Runs[projection.RunID] = projection
	}
	for _, g := range groups {
		view.Groups = append(view.Groups, *g)
	}
	sort.Slice(view.Groups, func(i, j int) bool { return view.Groups[i].Group < view.Groups[j].Group })
	for _, a := range agents {
		view.Agents = append(view.Agents, *a)
	}
	sort.Slice(view.Agents, func(i, j int) bool { return view.Agents[i].AgentID < view.Agents[j].AgentID })
	return view, nil
}

func newProcessGroupContext(scope db.ProcessRunScope) *processGroupContext {
	return &processGroupContext{GroupID: scope.GroupID, Group: scope.GroupName,
		Active: []string{}, NeedsYou: []string{}, Done: []string{}, Planned: []processPlannedRun{}}
}

// agentStep places the run's performer at its earliest current work step;
// Index stays 0 while no work step is current.
func (p processRunProjection) agentStep() processAgentRunStep {
	step := processAgentRunStep{RunID: p.RunID, Total: p.steps, State: string(p.Status)}
	best := 0
	for _, id := range p.Current {
		index := p.stepOrder[id]
		if index == 0 || (best != 0 && index >= best) {
			continue
		}
		best = index
		step.NodeID, step.Index = id, index
		for _, node := range p.Nodes {
			if node.ID == id {
				step.State = node.State
			}
		}
	}
	return step
}

// projectProcessRun reads one run through the same live-claim-aware view the
// run surfaces use, so action and needs-you agree with `process show`.
func projectProcessRun(runID string, live processLiveness) (processRunProjection, error) {
	record, accounted, claimed, err := processRuns.snapshotRunView(runID)
	if err != nil {
		return processRunProjection{}, err
	}
	runView, err := processRunViewWith(record, accounted, claimed, live)
	if err != nil {
		return processRunProjection{}, err
	}
	skeleton, err := processProjectionSkeletonFor(record)
	if err != nil {
		return processRunProjection{}, err
	}
	return skeleton.project(runView), nil
}

// processProjectionSkeleton is the run-independent part of a projection,
// derived once per immutable template ref.
type processProjectionSkeleton struct {
	templateID string
	name       string
	nodes      []processProjectionNode
	edges      []processProjectionEdge
	ids        []string // authored ids, longest first, for stage → parent mapping
	stepOrder  map[string]int
	steps      int
	truncated  bool
}

var processProjectionSkeletons = struct {
	sync.Mutex
	byRef map[string]*processProjectionSkeleton
}{byRef: map[string]*processProjectionSkeleton{}}

func processProjectionSkeletonFor(record *db.ProcessRun) (*processProjectionSkeleton, error) {
	processProjectionSkeletons.Lock()
	cached := processProjectionSkeletons.byRef[record.TemplateRef]
	processProjectionSkeletons.Unlock()
	if cached != nil {
		return cached, nil
	}
	var tmpl model.Template
	if err := strictjson.Decode(record.TemplateSnapshotJSON, &tmpl); err != nil {
		return nil, fmt.Errorf("%w: decode template snapshot: %v", db.ErrProcessRunCorrupt, err)
	}
	skeleton := newProcessProjectionSkeleton(&tmpl)
	processProjectionSkeletons.Lock()
	if len(processProjectionSkeletons.byRef) >= processProjectionCacheSize {
		processProjectionSkeletons.byRef = map[string]*processProjectionSkeleton{}
	}
	processProjectionSkeletons.byRef[record.TemplateRef] = skeleton
	processProjectionSkeletons.Unlock()
	return skeleton, nil
}

func newProcessProjectionSkeleton(tmpl *model.Template) *processProjectionSkeleton {
	skeleton := &processProjectionSkeleton{templateID: tmpl.ID, name: tmpl.Name, stepOrder: map[string]int{}}
	if skeleton.name == "" {
		skeleton.name = tmpl.ID
	}
	// A depth-first walk from start over sorted outcome labels marks back
	// edges — into a node still on the walk's stack, i.e. a retry or loop —
	// and the reverse post-order of the forward edges is a topological order,
	// so step indices follow execution order even where branches converge.
	// Nodes start cannot reach follow, walked from each in id order.
	const (
		unseen = iota
		onStack
		finished
	)
	mark := map[string]int{}
	back := map[[2]string]bool{}
	var post []string
	var walk func(id string)
	walk = func(id string) {
		mark[id] = onStack
		node := tmpl.Nodes[id]
		for _, label := range sortedNextLabels(node.Next) {
			to := node.Next[label]
			if _, exists := tmpl.Nodes[to]; !exists {
				continue
			}
			switch mark[to] {
			case onStack:
				back[[2]string{id, label}] = true
			case unseen:
				walk(to)
			}
		}
		mark[id] = finished
		post = append(post, id)
	}
	roots := make([]string, 0, len(tmpl.Nodes))
	for id := range tmpl.Nodes {
		roots = append(roots, id)
	}
	sort.Strings(roots)
	if _, exists := tmpl.Nodes[tmpl.Start]; exists {
		roots = append([]string{tmpl.Start}, roots...)
	}
	order := make([]string, 0, len(tmpl.Nodes))
	for _, root := range roots {
		if mark[root] != unseen {
			continue
		}
		post = post[:0]
		walk(root)
		for k := len(post) - 1; k >= 0; k-- {
			order = append(order, post[k])
		}
	}
	if len(order) > processProjectionMaxNodes {
		order, skeleton.truncated = order[:processProjectionMaxNodes], true
	}
	kept := make(map[string]bool, len(order))
	for _, id := range order {
		kept[id] = true
		node := tmpl.Nodes[id]
		projected := processProjectionNode{
			ID: id, Label: node.Name, Kind: string(node.Type), Performer: processProjectionPerformerOf(node),
			Description: clipProjectionText(node.Description, processProjectionDescriptionRunes),
		}
		if projected.Label == "" {
			projected.Label = id
		}
		switch node.Type {
		case model.NodeTypeTask, model.NodeTypeDecision, model.NodeTypeWait:
			skeleton.steps++
			projected.Index = skeleton.steps
			skeleton.stepOrder[id] = skeleton.steps
		}
		skeleton.nodes = append(skeleton.nodes, projected)
	}
	for _, from := range order {
		for _, label := range sortedNextLabels(tmpl.Nodes[from].Next) {
			to := tmpl.Nodes[from].Next[label]
			if !kept[to] {
				skeleton.truncated = true
				continue
			}
			skeleton.edges = append(skeleton.edges, processProjectionEdge{
				From: from, To: to, Label: label, Back: back[[2]string{from, label}],
			})
		}
	}
	skeleton.ids = append(skeleton.ids, order...)
	sort.Slice(skeleton.ids, func(i, j int) bool { return len(skeleton.ids[i]) > len(skeleton.ids[j]) })
	return skeleton
}

func sortedNextLabels(next model.Next) []string {
	labels := make([]string, 0, len(next))
	for label := range next {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	return labels
}

func processProjectionPerformerOf(node model.Node) processProjectionPerformer {
	switch {
	case node.Type == model.NodeTypeDecision:
		return processProjectionPerformer{Kind: string(model.PerformerHuman)}
	case node.Performer == nil:
		return processProjectionPerformer{Kind: "engine"}
	case node.Performer.Kind == model.PerformerProgram:
		return processProjectionPerformer{Kind: string(model.PerformerProgram), Ref: node.Performer.Profile}
	default:
		return processProjectionPerformer{Kind: string(node.Performer.Kind)}
	}
}

// authoredID maps an executable node id to the authored node that owns it:
// itself, the compound parent of a derived stage ("parent.stage…"), or the
// subprocess node whose body holds a namespaced node ("parent/inner…"). Body
// nodes are not projected one by one: their live state lifts onto the
// subprocess node, which projects with kind "subprocess".
func (s *processProjectionSkeleton) authoredID(id string) (string, bool) {
	for _, authored := range s.ids {
		if id == authored || strings.HasPrefix(id, authored+".") || strings.HasPrefix(id, authored+"/") {
			return authored, true
		}
	}
	return "", false
}

// processNodeStateRank orders the states a compound parent or a subprocess
// node can take from the nodes it owns: the most operator-relevant one wins.
var processNodeStateRank = map[string]int{
	"awaiting": 6, string(engine.NodeBlocked): 5, string(engine.NodeRunning): 4,
	string(engine.NodeReady): 3, string(engine.NodeFailed): 2, string(engine.NodeCanceled): 2,
}

func (s *processProjectionSkeleton) project(run processRunView) processRunProjection {
	projection := processRunProjection{
		RunID: run.ID, TemplateID: s.templateID, TemplateRef: run.TemplateRef, Name: s.name,
		Status: run.Status, Action: run.Action, NeedsYou: processContextNeedsYou[run.Action],
		Scope: run.Scope, Paused: run.Paused, Edges: append([]processProjectionEdge(nil), s.edges...),
		Current: []string{}, Truncated: s.truncated, StartedAt: run.CreatedAt, UpdatedAt: run.UpdatedAt,
		stepOrder: s.stepOrder, steps: s.steps,
	}
	awaiting := make(map[string]bool, len(run.AwaitingDecisions))
	for _, decision := range run.AwaitingDecisions {
		awaiting[decision.NodeID] = true
	}
	stateOf := func(id string) string {
		if awaiting[id] {
			return "awaiting"
		}
		return string(run.Checkpoint.Nodes[id])
	}
	states := make(map[string]string, len(s.nodes))
	for _, node := range s.nodes {
		states[node.ID] = stateOf(node.ID)
	}
	// A derived stage of a compound task lifts its live state onto the
	// authored parent when it is more operator-relevant than the parent's own.
	for id := range run.Checkpoint.Nodes {
		authored, ok := s.authoredID(id)
		if !ok || authored == id {
			continue
		}
		if state := stateOf(id); processNodeStateRank[state] > processNodeStateRank[states[authored]] {
			states[authored] = state
		}
	}
	// A dispatch names the executable node — a compound stage or a
	// subprocess body node lifts onto its authored node, like its state. Per
	// authored node an open attempt wins over a closed one, and among equals
	// the later one (Performers lists attempts oldest first per node).
	dispatched := make(map[string]processRunPerformerView, len(run.Performers))
	for _, performer := range run.Performers {
		authored, ok := s.authoredID(performer.NodeID)
		if !ok {
			continue
		}
		if current, seen := dispatched[authored]; seen &&
			current.State != db.ProcessDispatchClosed && performer.State == db.ProcessDispatchClosed {
			continue
		}
		dispatched[authored] = performer
	}
	projection.Nodes = make([]processProjectionNode, len(s.nodes))
	for i, node := range s.nodes {
		node.State = states[node.ID]
		if node.State == "" {
			node.State = string(engine.NodePending)
		}
		if node.Performer.Kind == string(model.PerformerAgent) {
			node.Performer.Ref = "unassigned"
			if run.Scope.Kind == db.ProcessRunScopeAgent {
				node.Performer.Ref = run.Scope.AgentID
			}
			if latest, ok := dispatched[node.ID]; ok {
				node.Dispatch = &processProjectionDispatch{
					NodeID: latest.NodeID, AgentID: latest.AgentID, Name: latest.Name, Attempt: latest.Attempt,
					State: latest.State, WaitReason: latest.WaitReason, DeliveredAt: latest.DeliveredAt,
				}
				if latest.AgentID != "" {
					node.Performer.Ref = latest.AgentID
				}
			}
		}
		if processNodeStateRank[node.State] >= processNodeStateRank[string(engine.NodeReady)] &&
			node.State != string(engine.NodeFailed) && node.State != string(engine.NodeCanceled) {
			projection.Current = append(projection.Current, node.ID)
		}
		projection.Nodes[i] = node
	}
	for i, edge := range projection.Edges {
		projection.Edges[i].State = string(engine.EdgeUnresolved)
		if disposition, ok := run.Checkpoint.Edges[edge.From][edge.Label]; ok {
			projection.Edges[i].State = string(disposition)
		}
	}
	return projection
}
