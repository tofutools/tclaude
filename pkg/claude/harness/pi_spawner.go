package harness

import (
	"sort"
	"strings"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
)

// piSpawner builds the `pi` invocation that runs inside the tmux pane. Every
// option was observed against the real binary at PiMinimumVersion:
//
//   - `--session-id <id>` opens the session with exactly that id, creating it
//     when absent: the fresh-launch enrollment path.
//   - `--session <path|id>` resumes. By id, pi searches the current project
//     first and, when the session belongs to ANOTHER working directory, stops
//     on a "Fork this session into current directory? [y/N]" prompt, which
//     would park a detached pane. A path resumes from any directory, so the
//     spawner resolves the id to its session file and passes the path; the
//     id is only the fallback when no file is found.
//   - `--name`, `--model`, `--thinking` set the session name, model and
//     thinking level.
//   - `--extension <path>` loads tclaude's status extension (pi_extension.go),
//     which runs the hook callback at $TCLAUDE_PI_HOOK_BIN, and under the
//     `ask` posture its permission extension too. Explicit extensions load
//     even under a pass-through `--no-extensions`.
//   - Positional arguments after `--` are messages: the first turn is
//     submitted at launch and the TUI stays open. A message starting with `@`
//     would be read as a file to attach, which is pi's own syntax.
//
// The working directory comes from tmux (`new-session -c`), as for every
// other spawner.
type piSpawner struct{}

func (piSpawner) Binary() string { return "pi" }

// piSessionFileLookup finds the session file a resume should pass. It is a
// variable so spawner tests stay independent of the operator's pi store.
var piSessionFileLookup = func(cwd, id string) string {
	dir, err := piSessionsDir(nil, cwd)
	if err != nil {
		return ""
	}
	return piFindSessionFile(dir, id)
}

// BuildCommand assembles the pi invocation: env exports, the binary, either a
// resume or a fresh launch's `--session-id`, then name, model, thinking level,
// the approval posture's tool set, pass-through args, and finally the first
// turn after `--`.
//
// Fields with no pi flag are ignored rather than approximated: the sandbox
// mode (pi has none, see pi_sandbox.go), auto-review, permission profiles,
// remote control and the settings payload all belong to contracts the
// descriptor leaves nil or that resolve to nothing for pi.
func (piSpawner) BuildCommand(spec SpawnSpec) string {
	binary := "pi"
	if spec.ExecutablePath != "" {
		binary = clcommon.ShellQuoteArg(spec.ExecutablePath)
		if spec.ExecutableInterpreter != "" {
			binary = clcommon.ShellQuoteArg(spec.ExecutableInterpreter) + " " + binary
		}
	}
	args := []string{}
	prefix := ""
	if spec.StatusExtensionPath != "" {
		args = append(args, "--extension", spec.StatusExtensionPath)
		if spec.TclaudeExecutable != "" {
			prefix = "export " + PiHookBinEnvVar + "=" + clcommon.ShellQuoteArg(spec.TclaudeExecutable) + "; "
		}
	}
	if spec.ResumeID != "" {
		session := spec.ResumeID
		if path := piSessionFileLookup(spec.Cwd, spec.ResumeID); path != "" {
			session = path
		}
		args = append(args, "--session", session)
	} else if spec.SessionID != "" {
		args = append(args, "--session-id", spec.SessionID)
	}
	if spec.Name != "" {
		args = append(args, "--name", spec.Name)
	}
	if spec.Model != "" {
		args = append(args, "--model", spec.Model)
	}
	if spec.Effort != "" {
		args = append(args, "--thinking", spec.Effort)
	}
	args = append(args, piApprovalArgs(spec.ApprovalPolicy)...)
	if strings.TrimSpace(spec.ApprovalPolicy) == PiApprovalAsk {
		// ask is the permission extension beside the status extension. A
		// launch without one cannot ask, so it gets the read-only tool set
		// instead: never more than the human would have approved.
		if spec.StatusExtensionPath != "" {
			args = append(args, "--extension", PiPermissionExtensionPath(spec.StatusExtensionPath))
		} else {
			args = append(args, "--tools", strings.Join(piReadOnlyTools, ","))
		}
	}
	args = append(args, spec.ExtraArgs...)
	if spec.InitialPrompt != "" {
		args = append(args, "--", spec.InitialPrompt)
	}
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = clcommon.ShellQuoteArg(a)
	}
	// Under a filtered network pi runs offline, and that goes last before the
	// binary so neither the forwarded exports nor a pre-launch script can
	// turn it back on.
	cmd := spec.EnvExports + prefix + spec.PreLaunchScript + piFilteredNetworkPrefix(spec.NetworkFiltered) + binary
	if len(quoted) > 0 {
		cmd += " " + strings.Join(quoted, " ")
	}
	return cmd
}

// piFilteredNetworkPrefix exports PiFilteredNetworkEnv for a filtered launch.
func piFilteredNetworkPrefix(filtered bool) string {
	if !filtered {
		return ""
	}
	env := PiFilteredNetworkEnv()
	names := make([]string, 0, len(env))
	for name := range env {
		names = append(names, name)
	}
	sort.Strings(names)
	out := ""
	for _, name := range names {
		out += "export " + name + "=" + clcommon.ShellQuoteArg(env[name]) + "; "
	}
	return out
}
