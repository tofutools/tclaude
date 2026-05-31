package workflow

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// templateMeta is the workflow.yaml schema.
type templateMeta struct {
	Name        string     `yaml:"name"`
	Description string     `yaml:"description,omitempty"`
	Params      []Param    `yaml:"params,omitempty"`
	Entry       stringList `yaml:"entry,omitempty"`
}

// ValidationError aggregates every problem found in a template, so a single
// load reports all of them rather than failing on the first.
type ValidationError struct {
	Ref      string
	Problems []string
}

func (e *ValidationError) Error() string {
	head := fmt.Sprintf("%d problem(s)", len(e.Problems))
	if e.Ref != "" {
		head = fmt.Sprintf("workflow %q has %d problem(s)", e.Ref, len(e.Problems))
	}
	return head + ":\n  - " + strings.Join(e.Problems, "\n  - ")
}

// LoadDir loads and validates a template from an on-disk directory.
func LoadDir(dir string, ref string, source Source) (*Template, error) {
	return LoadFS(os.DirFS(dir), ref, source, dir)
}

// LoadFS loads and validates a template from a filesystem rooted at the
// template directory (so it holds workflow.yaml, flow.mmd and nodes/). dir is
// recorded as Template.Dir (the absolute path on disk, or "" when embedded).
func LoadFS(fsys fs.FS, ref string, source Source, dir string) (*Template, error) {
	t := &Template{Ref: ref, Source: source, Dir: dir, Nodes: map[string]*Node{}}

	metaRaw, err := fs.ReadFile(fsys, "workflow.yaml")
	if err != nil {
		return nil, fmt.Errorf("read workflow.yaml: %w", err)
	}
	var meta templateMeta
	if err := yaml.Unmarshal(metaRaw, &meta); err != nil {
		return nil, fmt.Errorf("parse workflow.yaml: %w", err)
	}
	t.Name = meta.Name
	t.Description = meta.Description
	t.Params = meta.Params
	t.Entry = []string(meta.Entry)

	mmd, err := fs.ReadFile(fsys, "flow.mmd")
	if err != nil {
		return nil, fmt.Errorf("read flow.mmd: %w", err)
	}
	t.Mermaid = string(mmd)
	dirn, mnodes, edges, err := parseMermaid(t.Mermaid)
	if err != nil {
		return nil, fmt.Errorf("parse flow.mmd: %w", err)
	}
	t.Direction = dirn
	t.MermaidNodes = mnodes
	t.Edges = edges

	entries, err := fs.ReadDir(fsys, "nodes")
	if err != nil {
		return nil, fmt.Errorf("read nodes/: %w", err)
	}
	var preProblems []string
	for _, ent := range entries {
		name := ent.Name()
		if ent.IsDir() {
			continue
		}
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
			continue
		}
		nodeRaw, err := fs.ReadFile(fsys, path.Join("nodes", name))
		if err != nil {
			return nil, fmt.Errorf("read nodes/%s: %w", name, err)
		}
		var n Node
		if err := yaml.Unmarshal(nodeRaw, &n); err != nil {
			return nil, fmt.Errorf("parse nodes/%s: %w", name, err)
		}
		n.ID = strings.TrimSuffix(strings.TrimSuffix(name, ".yaml"), ".yml")
		if _, dup := t.Nodes[n.ID]; dup {
			preProblems = append(preProblems, fmt.Sprintf("duplicate node definition for %q (both nodes/%s.yaml and nodes/%s.yml present)", n.ID, n.ID, n.ID))
		}
		t.Nodes[n.ID] = &n
	}

	problems := append(preProblems, t.validate()...)
	if len(problems) > 0 {
		return nil, &ValidationError{Ref: ref, Problems: problems}
	}
	return t, nil
}

// validate cross-checks the assembled template and returns every problem found.
func (t *Template) validate() []string {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	if strings.TrimSpace(t.Name) == "" {
		add("workflow.yaml: name is required")
	}
	if len(t.MermaidNodes) == 0 {
		add("flow.mmd: the chart declares no nodes")
	}

	// Every chart node needs a definition, and every definition needs a chart node.
	for _, id := range sortedMermaidIDs(t.MermaidNodes) {
		if _, ok := t.Nodes[id]; !ok {
			add("node %q appears in flow.mmd but has no nodes/%s.yaml", id, id)
		}
	}
	for _, id := range sortedNodeIDs(t.Nodes) {
		if _, ok := t.MermaidNodes[id]; !ok {
			add("nodes/%s.yaml has no matching node %q in flow.mmd", id, id)
		}
	}

	// Entry: declared ids must exist; otherwise compute and require a source node.
	if len(t.Entry) > 0 {
		for _, id := range t.Entry {
			if _, ok := t.MermaidNodes[id]; !ok {
				add("entry node %q is not declared in flow.mmd", id)
			}
		}
	} else {
		t.Entry = t.computeEntry()
		if len(t.MermaidNodes) > 0 && len(t.Entry) == 0 {
			add("no entry node: every node has an incoming edge (a pure cycle). Declare `entry:` in workflow.yaml")
		}
	}

	// Params: non-empty, unique names.
	seenParam := map[string]bool{}
	for _, p := range t.Params {
		if strings.TrimSpace(p.Name) == "" {
			add("workflow.yaml: a param has an empty name")
			continue
		}
		if seenParam[p.Name] {
			add("workflow.yaml: duplicate param %q", p.Name)
		}
		seenParam[p.Name] = true
	}

	// Per-node validation (only for nodes that have a matching chart node).
	for _, id := range sortedNodeIDs(t.Nodes) {
		if _, ok := t.MermaidNodes[id]; !ok {
			continue
		}
		t.validateNode(id, t.Nodes[id], add)
	}

	// Static graph topology: reachability, can-reach-terminal, terminal sanity
	// (problems) and enum-coverage (warnings). Runs last, after Entry is settled.
	t.analyzeGraph(add)
	return problems
}

func (t *Template) validateNode(id string, n *Node, add func(string, ...any)) {
	// Executor.
	switch n.Executor.Kind {
	case ExecHuman:
		// instructions optional
	case ExecAI:
		if strings.TrimSpace(n.Executor.Prompt) == "" {
			add("node %q: ai executor needs a prompt", id)
		}
		if m := n.Executor.Mode; m != "" && m != ModeInteractive && m != ModeAutonomous {
			add("node %q: executor.mode %q must be %q or %q", id, m, ModeInteractive, ModeAutonomous)
		}
	case ExecTool, ExecProgram:
		if strings.TrimSpace(n.Executor.Run) == "" {
			add("node %q: %s executor needs a run command", id, n.Executor.Kind)
		}
	case "":
		add("node %q: executor.kind is required (one of %s)", id, joinExecKinds())
	default:
		add("node %q: unknown executor.kind %q (one of %s)", id, n.Executor.Kind, joinExecKinds())
	}

	// Verify (defaults to none).
	switch n.Verify.Kind {
	case "", VerifyNone, VerifyHuman, VerifyAI:
	case VerifyTool, VerifyProgram:
		if strings.TrimSpace(n.Verify.Run) == "" {
			add("node %q: %s verification needs a run command", id, n.Verify.Kind)
		}
	case VerifyEnum:
		if len(n.Verify.Values) == 0 {
			add("node %q: enum verification needs a non-empty values list", id)
		}
	case VerifyFormat:
		if strings.TrimSpace(n.Verify.Pattern) == "" {
			add("node %q: format verification needs a pattern", id)
		} else if _, err := regexp.Compile(n.Verify.Pattern); err != nil {
			add("node %q: format pattern is not a valid regex: %v", id, err)
		}
	default:
		add("node %q: unknown verify.kind %q (one of %s)", id, n.Verify.Kind, joinVerifyKinds())
	}

	if n.OnFail != "" && n.OnFail != OnFailStop && n.OnFail != OnFailContinue {
		add("node %q: on_fail %q must be %q or %q", id, n.OnFail, OnFailStop, OnFailContinue)
	}
	if n.Join != "" && n.Join != JoinAll && n.Join != JoinAny {
		add("node %q: join %q must be %q or %q", id, n.Join, JoinAll, JoinAny)
	}
	if n.Retries < 0 {
		add("node %q: retries must be >= 0", id)
	}
	// max_visits: 0 = engine default cap, a positive N = that cap, and -1 = the
	// explicit truly-unbounded escape hatch (JOH-39 — see EffectiveMaxVisits). Any
	// other negative is meaningless.
	if n.MaxVisits < -1 {
		add("node %q: max_visits must be >= 0 (or -1 for unbounded)", id)
	}

	// Edge-label consistency: every labeled outgoing edge must name a valid
	// outcome for this node. Unlabeled edges always denote the success path.
	allowed := t.allowedOutcomes(n)
	hasFailEdge := false
	for _, e := range t.OutEdges(id) {
		if e.Label == "" {
			continue
		}
		if e.Label == OutcomeFail {
			hasFailEdge = true
		}
		if !allowed[e.Label] {
			add("node %q: edge -->|%s| %s labels an outcome that is not valid here (allowed: %s)",
				id, e.Label, e.To, joinSorted(allowed))
		}
	}
	if hasFailEdge && n.OnFail != OnFailContinue {
		add("node %q: has a -->|fail| edge but on_fail is not %q (the fail edge is dead)", id, OnFailContinue)
	}
}

// allowedOutcomes returns the set of edge labels valid for a node: its enum
// values plus "fail" for enum nodes, or {pass, fail} otherwise.
func (t *Template) allowedOutcomes(n *Node) map[string]bool {
	out := map[string]bool{}
	if n.Verify.Kind == VerifyEnum {
		for _, v := range n.Verify.Values {
			out[v] = true
		}
		out[OutcomeFail] = true
		return out
	}
	out[OutcomePass] = true
	out[OutcomeFail] = true
	return out
}

// computeEntry returns the node ids with no incoming edge, in chart order.
func (t *Template) computeEntry() []string {
	hasIncoming := map[string]bool{}
	for _, e := range t.Edges {
		hasIncoming[e.To] = true
	}
	var entry []string
	for _, id := range sortedMermaidIDs(t.MermaidNodes) {
		if !hasIncoming[id] {
			entry = append(entry, id)
		}
	}
	return entry
}

func sortedMermaidIDs(m map[string]MermaidNode) []string {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func sortedNodeIDs(m map[string]*Node) []string {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func joinSorted(set map[string]bool) string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

func joinExecKinds() string {
	ss := make([]string, len(ValidExecutorKinds))
	for i, k := range ValidExecutorKinds {
		ss[i] = string(k)
	}
	return strings.Join(ss, ", ")
}

func joinVerifyKinds() string {
	ss := make([]string, len(ValidVerifyKinds))
	for i, k := range ValidVerifyKinds {
		ss[i] = string(k)
	}
	return strings.Join(ss, ", ")
}
