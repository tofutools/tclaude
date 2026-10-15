package executor

import "github.com/tofutools/tclaude/pkg/claude/process/engine"

// Program sandbox labels recorded in ProgramBounds.Sandbox.
const (
	ProgramUnconfined = "unconfined"
	// ProgramTclaudeLayer is tclaude's own OS sandbox (bwrap on Linux,
	// Seatbelt on macOS).
	ProgramTclaudeLayer = "tclaude-layer"
)

// ProgramBounds is where one program attempt runs: the working directory and
// the confinement its run's scope narrows it to. The daemon resolves it when
// the attempt is planned (see Run.SetProgramBounds) and the executor records it
// on the attempt's program_prepared / program_reissued evidence, so a reader
// can tell where each attempt ran.
//
// A nil *ProgramBounds is the unscoped behaviour: a plain child of the daemon
// in the daemon's working directory.
type ProgramBounds struct {
	// Scope is the run scope kind the bounds came from: agent or group.
	Scope string `json:"scope"`
	// Cwd is the attempt's working directory; empty keeps the daemon's.
	Cwd string `json:"cwd,omitempty"`
	// Sandbox is ProgramUnconfined or ProgramTclaudeLayer.
	Sandbox string `json:"sandbox"`
	// Profiles names the sandbox profiles the confinement applies.
	Profiles []string `json:"profiles,omitempty"`
	// Reason says why the attempt is unconfined, or which sandbox it inherits.
	Reason string `json:"reason,omitempty"`
	// Error, when set, means the scope's bounds cannot be enforced. The attempt
	// fails closed with it and never starts unconfined instead.
	Error string `json:"error,omitempty"`
	// Wrap renders the argv that runs the program inside these bounds. It runs
	// on the worker, immediately before the program starts, and an error fails
	// the attempt closed. Nil runs the argv as is.
	Wrap func(argv []string) ([]string, error) `json:"-"`
}

// ProgramBoundsResolver resolves the bounds one program command runs in. It
// is called by the run's owner while planning, before the command commits.
type ProgramBoundsResolver func(command engine.Command) *ProgramBounds

// SetProgramBounds installs the resolver the run's owner consults for every
// program command it plans or reissues. Without one, programs run unbounded
// exactly as an unscoped run's do.
func (r *Run) SetProgramBounds(resolver ProgramBoundsResolver) { r.bounds = resolver }

func (r *Run) programBounds(command *engine.Command) *ProgramBounds {
	if r.bounds == nil || command == nil || command.Kind == engine.CommandAgent {
		return nil
	}
	return r.bounds(*command)
}
