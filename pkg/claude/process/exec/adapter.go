package processexec

import (
	"context"
	"fmt"
	"strings"

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
