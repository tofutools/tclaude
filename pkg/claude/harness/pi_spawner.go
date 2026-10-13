package harness

import (
	"os"
	"path/filepath"
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
//   - A managed launch's skills and rules are registered per launch
//     (piManagedInstructionArgs).
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
	args = append(args, piManagedInstructionArgs(spec.ManagedInstructionsRoot)...)
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
	cmd := spec.EnvExports + prefix + spec.PreLaunchScript + piFilteredNetworkPrefix(spec.NetworkFiltered) +
		piWalledAgentDirPrefix(spec.HarnessStateDir) + binary
	if len(quoted) > 0 {
		cmd += " " + strings.Join(quoted, " ")
	}
	return cmd
}

// piManagedInstructionArgs registers a managed launch's skills and rules for
// this launch only, never in pi's own configuration:
//
//   - Skills: the launch directory is loaded as a local pi package (its
//     conventional `skills/` directory; it holds no `extensions/`). pi loads a
//     command-line package's skills before its ambient roots and keeps the
//     first skill of a name, so a managed skill shadows a same-named one in
//     ~/.agents/skills or a project, while every other ambient skill, project
//     trust and the operator's packages stay as they are. `--skill` would not
//     do: pi loads those after the ambient roots, so the ambient skill wins.
//   - Rules: RULES.md is appended to the system prompt, which pi rebuilds
//     every turn, so the rules survive a compaction. pi reads the argument as
//     a file only when one exists, so it is passed only for a regular file.
func piManagedInstructionArgs(root string) []string {
	if root == "" {
		return nil
	}
	var args []string
	if info, err := os.Stat(filepath.Join(root, "skills")); err == nil && info.IsDir() {
		args = append(args, "--extension", root)
	}
	rules := filepath.Join(root, "RULES.md")
	if info, err := os.Stat(rules); err == nil && info.Mode().IsRegular() {
		args = append(args, "--append-system-prompt", rules)
	}
	return args
}

// piWalledAgentDirPrefix points a walled launch at its walled agent
// directory, after the forwarded exports and the pre-launch script so neither
// can point it back at the operator's.
func piWalledAgentDirPrefix(dir string) string {
	if dir == "" {
		return ""
	}
	return "export " + PiAgentDirEnvVar + "=" + clcommon.ShellQuoteArg(dir) + "; "
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
