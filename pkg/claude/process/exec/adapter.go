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
// route it into retry prompts. EvidenceHash identifies the evidence content
// (the executor stamps it from the stored artifact's sha256 when the adapter
// provides evidence content); the evidence-unchanged gate short-circuit
// compares work-stage hashes across attempts.
type Observation struct {
	Actor        state.ActorRef
	Verdict      string
	Feedback     string
	Evidence     *Artifact
	EvidenceRef  string
	EvidenceHash string
	ExternalRef  string
}

type Adapter interface {
	Validate(Request) error
	Perform(context.Context, Request) (Observation, error)
}

// DispatchResult describes the durable wait created by an asynchronous
// performer. ExternalRef is the spawned agent id or obligation id; the
// executor records it on the issued command before returning control.
type DispatchResult struct {
	ExternalRef      string
	Assignee         string
	Summary          string
	AvailableActions []string
	DueAt            time.Time
	CreateObligation bool
}

type DeferredStatus string

const (
	DeferredMissing  DeferredStatus = "missing"
	DeferredInFlight DeferredStatus = "in_flight"
	DeferredObserved DeferredStatus = "observed"
)

// DeferredAdapter starts a durable external performer and later polls it.
// Dispatch must be idempotent by request.Command.ID. ReconcileDeferred must
// distinguish a discoverable in-flight side effect from a genuinely missing
// one; that distinction is what prevents resume from pausing healthy agents.
type DeferredAdapter interface {
	Adapter
	Dispatch(context.Context, Request) (DispatchResult, error)
	ReconcileDeferred(context.Context, Request) (Observation, DeferredStatus, error)
}

type Activity struct {
	Recovered       bool
	HumanInteracted bool
	// AutomatedDelivery identifies a UserPromptSubmit caused by tclaude's own
	// inbox delivery rather than a human typing in the performer pane.
	AutomatedDelivery bool
	At                time.Time
}

// ContactAdapter is the optional nudge/preemption surface for a deferred
// performer. Implementations route through existing agent-message or
// notify-human machinery; the engine only owns schedule state.
type ContactAdapter interface {
	Contact(context.Context, Request, bool) error
	Activity(context.Context, Request, time.Time) (Activity, error)
}

const (
	DefaultHumanContactCadence = 30 * time.Minute
	DefaultHumanContactBudget  = 5
	DefaultAgentContactCadence = 5 * time.Minute
	DefaultAgentContactBudget  = 3
)

func ContactScheduleFor(performer model.Performer) (time.Duration, int, string, error) {
	cadence := DefaultAgentContactCadence
	budget := DefaultAgentContactBudget
	if performer.Kind == model.PerformerHuman {
		cadence = DefaultHumanContactCadence
		budget = DefaultHumanContactBudget
	}
	escalation := "human:operator"
	if performer.Contact == nil {
		return cadence, budget, escalation, nil
	}
	parsed, err := time.ParseDuration(strings.TrimSpace(performer.Contact.Cadence))
	if err != nil || parsed <= 0 {
		return 0, 0, "", fmt.Errorf("invalid contact cadence %q", performer.Contact.Cadence)
	}
	if performer.Contact.Budget <= 0 {
		return 0, 0, "", fmt.Errorf("invalid contact budget %d", performer.Contact.Budget)
	}
	escalation = strings.TrimSpace(performer.Contact.EscalationTarget)
	if escalation == "" {
		return 0, 0, "", fmt.Errorf("contact escalation target is required")
	}
	return parsed, performer.Contact.Budget, escalation, nil
}

// ContactScheduleForOwner applies the human performer defaults to a typed
// blocked owner. Only human/role owners are accepted until agent and program
// block contacts have complete delivery, recovery, and escalation semantics.
func ContactScheduleForOwner(owner string) (state.WaitKind, time.Duration, int, string, error) {
	kind, ok := state.ContactKindForOwner(owner)
	if !ok {
		return "", 0, 0, "", fmt.Errorf("blocked owner %q has no contact kind", owner)
	}
	if kind != state.WaitKindHuman {
		return "", 0, 0, "", fmt.Errorf("blocked owner %q has unsupported contact kind %q; only human/role blocked owners are supported", owner, kind)
	}
	cadence, budget, escalation, err := ContactScheduleFor(model.Performer{Kind: model.PerformerHuman})
	return kind, cadence, budget, escalation, err
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
	if state.IsEngineActor(observation.Actor) {
		return fmt.Errorf("performer observation actor %q is reserved: engine actors mark engine-synthesized decisions, not performer results", observation.Actor)
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
