package processexec

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/plan"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
)

// Input is the slot-agnostic process input supplied to every performer kind.
// Later compound-node expansion can add upstream captures without changing the
// adapter's command/performer contract.
type Input struct {
	RunID   string            `json:"runId"`
	NodeID  string            `json:"nodeId,omitempty"`
	Attempt int               `json:"attempt,omitempty"`
	Params  map[string]string `json:"params,omitempty"`
}

type Request struct {
	Command   plan.Command    `json:"command"`
	Performer model.Performer `json:"performer"`
	Input     Input           `json:"input"`
}

// Artifact is evidence content for the executor to persist before recording
// the observation. Adapters do not need to know which store implementation is
// hosting the run.
type Artifact struct {
	Name string
	Data []byte
}

// Observation is the uniform result contract for human, agent, and program
// performers. Feedback is intentionally adapter-neutral; phase 2 persists
// program feedback inside its evidence artifact, while later engine work can
// route it into retry prompts.
type Observation struct {
	Actor       state.ActorRef
	Verdict     string
	Feedback    string
	Evidence    *Artifact
	EvidenceRef string
	ExternalRef string
}

type Adapter interface {
	Validate(Request) error
	Perform(context.Context, Request) (Observation, error)
}

// ReconcileAdapter is implemented by performer adapters whose external side
// effect can be rediscovered by the command idempotency key after a host
// restart. found=false means the adapter completed its lookup but cannot prove
// an observation; the engine must not perform the command again implicitly.
type ReconcileAdapter interface {
	Reconcile(context.Context, Request) (observation Observation, found bool, err error)
}

// RateLimitError tells the engine that an adapter did not perform its side
// effect because quota was exhausted. The issued command remains retryable at
// Until without settling the node attempt or consuming retry budget.
type RateLimitError struct {
	Until time.Time
	Err   error
}

func (e *RateLimitError) Error() string {
	if e == nil {
		return "process performer rate limited"
	}
	if e.Err != nil {
		return fmt.Sprintf("process performer rate limited until %s: %v", e.Until.Format(time.RFC3339), e.Err)
	}
	return fmt.Sprintf("process performer rate limited until %s", e.Until.Format(time.RFC3339))
}

func (e *RateLimitError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func validateObservation(observation Observation) error {
	if !state.ValidateActorRef(observation.Actor) {
		return fmt.Errorf("invalid performer observation actor %q", observation.Actor)
	}
	if strings.TrimSpace(observation.Verdict) == "" {
		return fmt.Errorf("performer observation verdict is required")
	}
	if observation.Evidence != nil && strings.TrimSpace(observation.EvidenceRef) != "" {
		return fmt.Errorf("performer observation must provide evidence content or an evidence ref, not both")
	}
	if observation.Evidence != nil && strings.TrimSpace(observation.Evidence.Name) == "" {
		return fmt.Errorf("performer observation evidence name is required")
	}
	return nil
}
