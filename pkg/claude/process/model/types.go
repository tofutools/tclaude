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
	Metadata    Metadata     `json:"metadata,omitempty" yaml:"metadata,omitempty"`
}

type Step struct {
	ID          string       `json:"id,omitempty" yaml:"id,omitempty"`
	Name        string       `json:"name,omitempty" yaml:"name,omitempty"`
	Description string       `json:"description,omitempty" yaml:"description,omitempty"`
	Doc         string       `json:"doc,omitempty" yaml:"doc,omitempty"`
	Performer   Performer    `json:"performer" yaml:"performer"`
	Retry       *RetryPolicy `json:"retry,omitempty" yaml:"retry,omitempty"`
}

type Performer struct {
	Kind    PerformerKind `json:"kind" yaml:"kind"`
	Profile string        `json:"profile,omitempty" yaml:"profile,omitempty"`
	Prompt  string        `json:"prompt,omitempty" yaml:"prompt,omitempty"`
	Ask     string        `json:"ask,omitempty" yaml:"ask,omitempty"`
	Run     string        `json:"run,omitempty" yaml:"run,omitempty"`
	Args    []string      `json:"args,omitempty" yaml:"args,omitempty"`
	Timeout string        `json:"timeout,omitempty" yaml:"timeout,omitempty"`
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

type ParsedTemplate struct {
	Template     *Template
	Edges        []Edge
	Diagnostics  Diagnostics
	SemanticHash string
	SourceHash   string
	Ref          string
}
