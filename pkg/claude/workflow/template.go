// Package workflow defines, parses, and validates tclaude workflow templates.
//
// A workflow template is user data on disk: a directory holding a mermaid flow
// chart (flow.mmd), a metadata file (workflow.yaml), and one YAML file per node
// under nodes/ keyed by the mermaid node id. The chart is the topology — which
// node leads to which, including branches, joins, parallel fan-out and loops —
// and each node's YAML is the detail: who executes it and how it is verified.
//
// This package only handles the static definition (load + validate). Running
// instances and their per-node state live in SQLite (pkg/claude/common/db) and
// are advanced by agentd; see the Workflows epic on Linear (JOH-9).
package workflow

import "gopkg.in/yaml.v3"

// ExecutorKind names who performs a node's work.
type ExecutorKind string

const (
	ExecHuman   ExecutorKind = "human"   // a person does it and reports completion
	ExecAI      ExecutorKind = "ai"      // a tclaude agent does it
	ExecTool    ExecutorKind = "tool"    // run a command; its exit code is the signal
	ExecProgram ExecutorKind = "program" // like tool, but a longer-running program
)

// ValidExecutorKinds is the set accepted by the loader.
var ValidExecutorKinds = []ExecutorKind{ExecHuman, ExecAI, ExecTool, ExecProgram}

// VerifyKind names how a node's definition-of-done is decided.
type VerifyKind string

const (
	VerifyNone    VerifyKind = "none"    // executor's own success is the verdict
	VerifyHuman   VerifyKind = "human"   // a human approves
	VerifyAI      VerifyKind = "ai"      // an AI judge rules pass/fail
	VerifyTool    VerifyKind = "tool"    // a command exits 0 to pass
	VerifyProgram VerifyKind = "program" // like tool
	VerifyEnum    VerifyKind = "enum"    // the produced value selects the outgoing edge
	VerifyFormat  VerifyKind = "format"  // the output matches a regex
)

// ValidVerifyKinds is the set accepted by the loader.
var ValidVerifyKinds = []VerifyKind{
	VerifyNone, VerifyHuman, VerifyAI, VerifyTool, VerifyProgram, VerifyEnum, VerifyFormat,
}

// Reserved node outcomes. Every node produces an outcome string when it
// settles; an outgoing edge whose label equals the outcome is followed, and an
// unlabeled edge is followed on OutcomePass. Enum-verified nodes produce one of
// their declared values instead of OutcomePass.
const (
	OutcomePass = "pass" // succeeded; also the target of unlabeled edges
	OutcomeFail = "fail" // failed verification or execution
)

// Mode values for AI executors.
const (
	ModeInteractive = "interactive"
	ModeAutonomous  = "autonomous"
)

// OnFail values.
const (
	OnFailStop     = "stop"     // a failed node halts the instance (default)
	OnFailContinue = "continue" // follow the |fail| edge instead of halting
)

// Join values — how a node with multiple incoming edges becomes ready.
const (
	JoinAll = "all" // every predecessor on a taken path must be done (default)
	JoinAny = "any" // any one predecessor being done is enough
)

// Executor describes who performs a node. Exactly one Kind applies; the other
// fields are interpreted per-kind (see the field comments).
type Executor struct {
	Kind ExecutorKind `yaml:"kind"`

	// ai
	Agent  string `yaml:"agent,omitempty"`  // profile/role hint for the spawned agent
	Mode   string `yaml:"mode,omitempty"`   // interactive | autonomous
	Prompt string `yaml:"prompt,omitempty"` // the task handed to the agent (interpolated)
	Group  string `yaml:"group,omitempty"`  // optional group override

	// human
	Instructions string `yaml:"instructions,omitempty"` // shown on the dashboard

	// tool / program
	Run     string `yaml:"run,omitempty"`     // command to run (interpolated)
	Workdir string `yaml:"workdir,omitempty"` // working dir for the command
}

// Verify describes a node's definition-of-done.
type Verify struct {
	Kind    VerifyKind `yaml:"kind,omitempty"`    // defaults to none
	Run     string     `yaml:"run,omitempty"`     // tool/program: verification command
	Workdir string     `yaml:"workdir,omitempty"` // tool/program: working dir
	Values  []string   `yaml:"values,omitempty"`  // enum: allowed outcomes
	Pattern string     `yaml:"pattern,omitempty"` // format: regex the output must match
	Prompt  string     `yaml:"prompt,omitempty"`  // ai: instruction for the judge agent
}

// Node is the per-node definition (one nodes/<id>.yaml file).
type Node struct {
	ID        string   `yaml:"-"`                    // set from the filename / mermaid id
	Label     string   `yaml:"label,omitempty"`      // human label; falls back to mermaid text, then id
	Executor  Executor `yaml:"executor"`             //
	Verify    Verify   `yaml:"verify,omitempty"`     //
	Capture   string   `yaml:"capture,omitempty"`    // name to store this node's output under
	Retries   int      `yaml:"retries,omitempty"`    // re-runs on failure before the node fails
	MaxVisits int      `yaml:"max_visits,omitempty"` // loop guard: max executions (0 = engine default cap; -1 = unbounded)
	OnFail    string   `yaml:"on_fail,omitempty"`    // stop | continue
	Join      string   `yaml:"join,omitempty"`       // all | any
}

// Param is a workflow instantiation parameter.
type Param struct {
	Name     string `yaml:"name"`
	Required *bool  `yaml:"required,omitempty"` // defaults to true when omitted
	Default  string `yaml:"default,omitempty"`
}

// IsRequired reports whether the param must be supplied at instantiation.
// Params are required by default; a param is optional only when it either sets
// required: false or supplies a default.
func (p Param) IsRequired() bool {
	if p.Required != nil {
		return *p.Required
	}
	return p.Default == ""
}

// Edge is a directed edge parsed from the mermaid chart. Label is the (possibly
// empty) pipe-form edge label, e.g. the "approved" in `review -->|approved| x`.
type Edge struct {
	From  string
	To    string
	Label string
}

// MermaidNode is a node declaration parsed from the mermaid chart. Text and
// Shape are cosmetic (used for the label fallback); only ID is structural.
type MermaidNode struct {
	ID    string
	Text  string
	Shape string // rect, round, stadium, subroutine, circle, diamond, hexagon, ...
}

// Source identifies where a template was resolved from.
type Source string

const (
	SourceProject Source = "project"
	SourceUser    Source = "user"
	SourceExample Source = "example"
	SourceDir     Source = "dir" // a plain on-disk directory (external, dir:<path>)
	SourceGit     Source = "git" // a git repo, fetched+cached (external, git:<url>...)
)

// IsExternal reports whether a template came from an external, third-party
// source (a dir: path or a git: repo) rather than a trusted local/embedded one.
// The execution engine and node approval gates use this seam to require
// confirmation before running an externally-sourced tool/program node; see the
// trust-model note in fetch.go.
func (s Source) IsExternal() bool {
	return s == SourceDir || s == SourceGit
}

// Template is a fully-loaded, validated workflow template.
type Template struct {
	Ref         string // resolved reference, e.g. "user:foo" or "example:foo"
	Source      Source // project | user | example | dir | git
	Dir         string // absolute source dir ("" for the embedded example)
	Name        string
	Description string
	Params      []Param
	Entry       []string // node ids that start ready (computed if not declared)
	Mermaid     string   // raw flow.mmd contents (rendered verbatim by the dashboard)
	Warnings    []string // non-fatal topology smells found during load (sorted, deterministic)

	Nodes        map[string]*Node       // keyed by node id
	Edges        []Edge                 // parsed from the mermaid chart
	MermaidNodes map[string]MermaidNode // node declarations parsed from the chart
	Direction    string                 // flowchart direction (TD/LR/...), cosmetic
}

// DisplayLabel returns the best human label for a node id: the node YAML's
// label, else the mermaid node text, else the id itself.
func (t *Template) DisplayLabel(id string) string {
	if n := t.Nodes[id]; n != nil && n.Label != "" {
		return n.Label
	}
	if mn, ok := t.MermaidNodes[id]; ok && mn.Text != "" {
		return mn.Text
	}
	return id
}

// OutEdges returns the edges leaving a node, in chart order.
func (t *Template) OutEdges(id string) []Edge {
	var out []Edge
	for _, e := range t.Edges {
		if e.From == id {
			out = append(out, e)
		}
	}
	return out
}

// stringList accepts either a YAML scalar or a sequence of strings, so a field
// like `entry: plan` and `entry: [a, b]` both work.
type stringList []string

func (s *stringList) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		*s = []string{value.Value}
		return nil
	}
	var list []string
	if err := value.Decode(&list); err != nil {
		return err
	}
	*s = list
	return nil
}
