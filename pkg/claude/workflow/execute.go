package workflow

// execute.go defines the engine's executor + verifier seam: the kind→impl
// contract the autonomous runner (agentd, Step 6) dispatches through, and that
// composite/sub-workflow/reflection steps (JOH-14/15/16) extend by adding a
// kind rather than editing a dispatcher.
//
// Design boundary. The *decision* logic — which executor/verifier a node uses,
// how a command's exit + output map to an outcome, how an enum value selects a
// branch — lives here and is pure and unit-tested. The *effects* — running a
// process, spawning an agent, writing the DB — are injected: an Executor is
// handed a Runner (for shell commands) and returns an ExecResult the caller
// persists. Nothing in this file imports os/exec, a DB, or agentd; the engine
// wires the concrete Runner and applies the results.

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// ExecOutcome is the engine-facing disposition of running (or attempting to
// run) a node's executor — distinct from the node's *branch* outcome (the
// pass/fail/enum string that selects an edge), which verification produces.
type ExecOutcome int

const (
	// ExecRan — the executor ran to a definite result (success or failure
	// captured in ExecResult); the engine proceeds to verification.
	ExecRan ExecOutcome = iota
	// ExecDefer — the executor does not run synchronously in the tick: an AI
	// node hands off to a spawned agent, a human node waits for the dashboard.
	// The engine leaves the node where the caller put it (running / awaiting)
	// and revisits on a later tick.
	ExecDefer
	// ExecError — the executor could not even start (bad interpolation, missing
	// command). The engine fails the node.
	ExecError
)

// ExecResult is what an Executor returns: the disposition, the captured output
// (stdout/stderr summary for a command), whether the run itself succeeded
// (exit 0), and an error string when ExecError/failed. Output is what a capture
// stores and what verification inspects.
type ExecResult struct {
	Outcome ExecOutcome
	Output  string
	Success bool   // the run's own success (exit 0); meaningful when Outcome==ExecRan
	Err     string // populated for ExecError or a failed run, for the event log
}

// Runner runs an interpolated shell command in a working directory and returns
// its combined output and whether it exited 0. The engine injects a real
// implementation (bash -c via executil); tests inject a fake. Kept tiny so the
// executor logic stays testable without spawning processes.
type Runner interface {
	Run(ctx context.Context, command, workdir string) (output string, exit0 bool, err error)
}

// RunExecutor dispatches a node's executor by kind and returns an ExecResult.
// It interpolates the command against scope first. tool/program run via the
// Runner; ai/human defer (ExecDefer) — the engine spawns the agent / waits for
// the human elsewhere. An unknown kind is an ExecError.
//
// This is the single dispatch point new executor kinds register against
// (JOH-14/15/16): add a case, return an ExecResult; the engine loop is
// unchanged.
func RunExecutor(ctx context.Context, n *Node, scope Scope, runner Runner) ExecResult {
	if n == nil {
		return ExecResult{Outcome: ExecError, Err: "nil node"}
	}
	switch n.Executor.Kind {
	case ExecTool, ExecProgram:
		cmd, missing := scope.Interpolate(n.Executor.Run)
		if len(missing) > 0 {
			return ExecResult{Outcome: ExecError,
				Err: "unresolved references in run command: " + strings.Join(missing, ", ")}
		}
		if strings.TrimSpace(cmd) == "" {
			return ExecResult{Outcome: ExecError, Err: "empty run command after interpolation"}
		}
		workdir, _ := scope.Interpolate(n.Executor.Workdir)
		out, exit0, err := runner.Run(ctx, cmd, workdir)
		res := ExecResult{Outcome: ExecRan, Output: out, Success: exit0}
		if err != nil && !exit0 {
			// A non-zero exit is a normal failure (Success=false); an actual
			// run error (couldn't start, timeout) also surfaces here.
			res.Err = err.Error()
		}
		return res
	case ExecAI, ExecHuman:
		// The engine handles these out-of-band: ai spawns/observes an agent,
		// human waits for the dashboard. Not run synchronously in the tick.
		return ExecResult{Outcome: ExecDefer}
	case "":
		return ExecResult{Outcome: ExecError, Err: "node has no executor.kind"}
	default:
		return ExecResult{Outcome: ExecError, Err: fmt.Sprintf("unknown executor.kind %q", n.Executor.Kind)}
	}
}

// VerifyDisposition is the result of verifying a settled-by-executor node: the
// branch Outcome string (drives the edge taken via Advance), whether the node
// is verified done, and whether the verification itself must run out-of-band
// (human approval, an ai-judge agent) rather than synchronously in the tick.
type VerifyDisposition struct {
	// Defer is true when the verdict is not decidable in the tick: verify.kind
	// human (dashboard approve/reject) or ai (a judge agent). The engine parks
	// the node awaiting_verify and resolves it elsewhere.
	Defer bool
	// Done is true when verification passed; false means it failed.
	Done bool
	// Outcome is the branch outcome string fed to Advance: an enum value, or
	// OutcomePass / OutcomeFail. Empty when Defer.
	Outcome string
	// Err carries a human-readable reason on failure / mis-verification.
	Err string
}

// RunVerifier decides a node's definition-of-done from its executor result.
// tool/program run a verification command (exit 0 = pass); enum parses the
// produced value out of the output and checks membership (the value becomes the
// branch outcome); format matches the output against a regex; none passes iff
// the executor itself succeeded; human/ai defer to an out-of-band verdict.
//
// execOK is the executor's own success (ExecResult.Success) — the signal `none`
// verification rides on, and the fallback a tool-less node uses.
//
// Like RunExecutor this is the single extension point for new verify kinds.
func RunVerifier(ctx context.Context, n *Node, scope Scope, output string, execOK bool, runner Runner) VerifyDisposition {
	if n == nil {
		return VerifyDisposition{Err: "nil node"}
	}
	switch n.Verify.Kind {
	case "", VerifyNone:
		return passFail(execOK, "executor reported failure")
	case VerifyTool, VerifyProgram:
		cmd, missing := scope.Interpolate(n.Verify.Run)
		if len(missing) > 0 {
			return VerifyDisposition{Outcome: OutcomeFail,
				Err: "unresolved references in verify command: " + strings.Join(missing, ", ")}
		}
		if strings.TrimSpace(cmd) == "" {
			return VerifyDisposition{Outcome: OutcomeFail, Err: "empty verify command after interpolation"}
		}
		workdir, _ := scope.Interpolate(n.Verify.Workdir)
		_, exit0, err := runner.Run(ctx, cmd, workdir)
		if err != nil && !exit0 {
			return VerifyDisposition{Outcome: OutcomeFail, Err: "verify command failed: " + err.Error()}
		}
		return passFail(exit0, "verify command exited non-zero")
	case VerifyEnum:
		return verifyEnum(n, output)
	case VerifyFormat:
		// An empty pattern matches everything (regexp.Compile("") is valid and
		// MatchString always true), which would silently pass-verify any output
		// — almost never the intent. Treat a missing pattern as a fail so a
		// mis-authored format node doesn't masquerade as "verified". (The loader
		// also rejects this, but the executor must not trust that.)
		if strings.TrimSpace(n.Verify.Pattern) == "" {
			return VerifyDisposition{Outcome: OutcomeFail, Err: "format verify has no pattern"}
		}
		re, err := regexp.Compile(n.Verify.Pattern)
		if err != nil {
			return VerifyDisposition{Outcome: OutcomeFail, Err: "invalid format pattern: " + err.Error()}
		}
		if re.MatchString(output) {
			return VerifyDisposition{Done: true, Outcome: OutcomePass}
		}
		return VerifyDisposition{Outcome: OutcomeFail, Err: "output did not match format pattern"}
	case VerifyHuman, VerifyAI:
		return VerifyDisposition{Defer: true}
	default:
		return VerifyDisposition{Outcome: OutcomeFail, Err: fmt.Sprintf("unknown verify.kind %q", n.Verify.Kind)}
	}
}

// passFail maps a boolean verdict onto a pass/fail disposition.
func passFail(ok bool, failReason string) VerifyDisposition {
	if ok {
		return VerifyDisposition{Done: true, Outcome: OutcomePass}
	}
	return VerifyDisposition{Outcome: OutcomeFail, Err: failReason}
}

// verifyEnum extracts the produced enum value and checks it against the node's
// declared values. The value is taken as the last non-empty line of the output
// (the command's final word on its verdict), trimmed — a convention that lets a
// tool print progress then end with its verdict. A value in the set becomes the
// branch outcome; anything else fails.
func verifyEnum(n *Node, output string) VerifyDisposition {
	val := lastNonEmptyLine(output)
	if val == "" {
		return VerifyDisposition{Outcome: OutcomeFail, Err: "enum verify: executor produced no value"}
	}
	if slices.Contains(n.Verify.Values, val) {
		return VerifyDisposition{Done: true, Outcome: val}
	}
	return VerifyDisposition{Outcome: OutcomeFail,
		Err: fmt.Sprintf("enum verify: produced value %q not in %v", val, n.Verify.Values)}
}

// lastNonEmptyLine returns the last non-blank line of s, trimmed. Empty when s
// has no non-blank line.
func lastNonEmptyLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			return t
		}
	}
	return ""
}
