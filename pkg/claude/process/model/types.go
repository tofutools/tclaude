package model

const (
	APIVersion = "tclaude.dev/v1alpha1"
	Kind       = "ProcessTemplate"

	HashAlgorithm = "sha256"
)

type NodeType string

const (
	NodeTypeTask     NodeType = "task"
	NodeTypeDecision NodeType = "decision"
	NodeTypeWait     NodeType = "wait"
	NodeTypeStart    NodeType = "start"
	NodeTypeEnd      NodeType = "end"
)

type PerformerKind string

const (
	PerformerHuman   PerformerKind = "human"
	PerformerAgent   PerformerKind = "agent"
	PerformerProgram PerformerKind = "program"
)

type Template struct {
	APIVersion  string           `json:"apiVersion" yaml:"apiVersion"`
	Kind        string           `json:"kind" yaml:"kind"`
	ID          string           `json:"id" yaml:"id"`
	Name        string           `json:"name,omitempty" yaml:"name,omitempty"`
	Description string           `json:"description,omitempty" yaml:"description,omitempty"`
	Doc         string           `json:"doc,omitempty" yaml:"doc,omitempty"`
	Params      map[string]Param `json:"params,omitempty" yaml:"params,omitempty"`
	Start       string           `json:"start" yaml:"start"`
	Nodes       map[string]Node  `json:"nodes" yaml:"nodes"`
	Layout      *Layout          `json:"layout,omitempty" yaml:"layout,omitempty"`
}

type Param struct {
	Type        string `json:"type" yaml:"type"`
	Name        string `json:"name,omitempty" yaml:"name,omitempty"`
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
	Doc         string `json:"doc,omitempty" yaml:"doc,omitempty"`
	Required    *bool  `json:"required,omitempty" yaml:"required,omitempty"`
	Default     any    `json:"default,omitempty" yaml:"default,omitempty"`
}

type Node struct {
	Type        NodeType     `json:"type" yaml:"type"`
	Name        string       `json:"name,omitempty" yaml:"name,omitempty"`
	Description string       `json:"description,omitempty" yaml:"description,omitempty"`
	Doc         string       `json:"doc,omitempty" yaml:"doc,omitempty"`
	Performer   *Performer   `json:"performer,omitempty" yaml:"performer,omitempty"`
	Plan        *Step        `json:"plan,omitempty" yaml:"plan,omitempty"`
	Checks      []Step       `json:"checks,omitempty" yaml:"checks,omitempty"`
	Review      *Step        `json:"review,omitempty" yaml:"review,omitempty"`
	Retry       *RetryPolicy `json:"retry,omitempty" yaml:"retry,omitempty"`
	Wait        *WaitConfig  `json:"wait,omitempty" yaml:"wait,omitempty"`
	Next        Next         `json:"next,omitempty" yaml:"next,omitempty"`
	Result      string       `json:"result,omitempty" yaml:"result,omitempty"`
	// Captures names the outputs a task node publishes for downstream nodes
	// (design §2: performer input = instructions + upstream captures). Names
	// only in v1 — the runtime capture plumbing is a later ticket; validation
	// keeps names id-shaped and unique so they can become references later.
	Captures []string `json:"captures,omitempty" yaml:"captures,omitempty"`
	Metadata Metadata `json:"metadata,omitempty" yaml:"metadata,omitempty"`
}

type Step struct {
	ID          string    `json:"id,omitempty" yaml:"id,omitempty"`
	Name        string    `json:"name,omitempty" yaml:"name,omitempty"`
	Description string    `json:"description,omitempty" yaml:"description,omitempty"`
	Doc         string    `json:"doc,omitempty" yaml:"doc,omitempty"`
	Performer   Performer `json:"performer" yaml:"performer"`
	// Approval is only valid on plan steps: human requires an explicit
	// plan-approval gate before work starts, auto (the default) does not.
	Approval string `json:"approval,omitempty" yaml:"approval,omitempty"`
	// ApprovalRetry is the synthesized plan-approval gate's failed-verdict
	// budget. It is only meaningful with Approval set to human.
	ApprovalRetry *RetryPolicy `json:"approvalRetry,omitempty" yaml:"approvalRetry,omitempty"`
	Retry         *RetryPolicy `json:"retry,omitempty" yaml:"retry,omitempty"`
}

// Performer is the uniform slot contract (design §2): kind, profile, timeout,
// and contact apply to every kind; the remaining fields are explicitly
// kind-scoped and validated as such. The discipline rule for growing this
// struct: define a new field for all three kinds or kind-scope it here (types
// + validate + schema) before any UI surfaces it.
type Performer struct {
	Kind    PerformerKind `json:"kind" yaml:"kind"`
	Profile string        `json:"profile,omitempty" yaml:"profile,omitempty"`
	// Prompt is the instruction text for agent performers; human performers
	// may use it as long-form context alongside (or instead of) Ask.
	Prompt string `json:"prompt,omitempty" yaml:"prompt,omitempty"`
	// Ask, Choices, and Assignee are human-scoped: the question to put to the
	// human, an optional closed answer set, and an optional specific person
	// (defaults to whoever holds the profile). A decision node's choices realize
	// as its outcome edges; task-stage choices are constrained by the engine's
	// pass/fail routing vocabulary.
	Ask      string   `json:"ask,omitempty" yaml:"ask,omitempty"`
	Choices  []string `json:"choices,omitempty" yaml:"choices,omitempty"`
	Assignee string   `json:"assignee,omitempty" yaml:"assignee,omitempty"`
	// ChoiceOutcomes routes a human task-stage vocabulary onto the engine's
	// existing binary attempt outcomes. Decision performers remain edge-driven
	// and therefore reject this field.
	ChoiceOutcomes map[string]string `json:"choiceOutcomes,omitempty" yaml:"choiceOutcomes,omitempty"`
	// Model and Effort are agent-scoped overrides on top of the profile.
	// Freeform strings: legal values are harness-specific and are validated at
	// the process-agent spawn boundary.
	Model  string `json:"model,omitempty" yaml:"model,omitempty"`
	Effort string `json:"effort,omitempty" yaml:"effort,omitempty"`
	// Run and Args are program-scoped: command execution (design §10).
	Run     string           `json:"run,omitempty" yaml:"run,omitempty"`
	Args    []string         `json:"args,omitempty" yaml:"args,omitempty"`
	Timeout string           `json:"timeout,omitempty" yaml:"timeout,omitempty"`
	Contact *ContactSchedule `json:"contact,omitempty" yaml:"contact,omitempty"`
}

// ContactSchedule controls follow-up for an asynchronous performer slot. A
// nil schedule uses the performer kind's runtime default. Programs currently
// execute synchronously, but the shape is deliberately uniform so a future
// polling program adapter can use the same contract.
type ContactSchedule struct {
	Cadence          string `json:"cadence,omitempty" yaml:"cadence,omitempty"`
	Budget           int    `json:"budget,omitempty" yaml:"budget,omitempty"`
	EscalationTarget string `json:"escalationTarget,omitempty" yaml:"escalationTarget,omitempty"`
}

type RetryPolicy struct {
	MaxAttempts int    `json:"maxAttempts,omitempty" yaml:"maxAttempts,omitempty"`
	Backoff     string `json:"backoff,omitempty" yaml:"backoff,omitempty"`
	OnFail      string `json:"onFail,omitempty" yaml:"onFail,omitempty"`
}

type WaitConfig struct {
	Duration string `json:"duration,omitempty" yaml:"duration,omitempty"`
	Until    string `json:"until,omitempty" yaml:"until,omitempty"`
	Signal   string `json:"signal,omitempty" yaml:"signal,omitempty"`
}

type Metadata map[string]any

type Layout struct {
	Nodes map[string]LayoutNode `json:"nodes,omitempty" yaml:"nodes,omitempty"`
}

type LayoutNode struct {
	X float64 `json:"x" yaml:"x"`
	Y float64 `json:"y" yaml:"y"`
}

type Edge struct {
	From    string `json:"from"`
	Outcome string `json:"outcome"`
	To      string `json:"to"`
}

// ParsedTemplate is the result of parsing a process template source file.
// Callers must reject templates when Diagnostics.HasErrors reports true; hashes
// are still populated for invalid templates so tools can compare/edit sources.
type ParsedTemplate struct {
	Template     *Template
	Edges        []Edge
	Diagnostics  Diagnostics
	SemanticHash string
	SourceHash   string
	Ref          string
}
