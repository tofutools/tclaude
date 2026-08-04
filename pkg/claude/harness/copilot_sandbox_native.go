package harness

// TCL-977 evaluated GitHub Copilot CLI's own "command sandboxing" feature for
// tclaude's SupportsBuiltinOSSandbox contract and answered NO. This file
// records that answer where the descriptor reads it, because the honest result
// of the evaluation is a refusal and a refusal with no reasoning attached is
// indistinguishable from an adapter nobody has written yet.
//
// What Copilot 1.0.77 actually ships (`copilot help sandbox`, plus the
// fixture-backed measurements in
// copilotfixture/sandbox_native_smoke_test.go):
//
//   - Shell commands run under Microsoft Execution Containers — bubblewrap on
//     Linux, Seatbelt on macOS, ProcessContainer on Windows. That half IS a
//     real OS sandbox, and this file does not dispute it.
//   - Built-in file edits are NOT OS-sandboxed. GitHub states this outright
//     ("Built-in file edits aren't OS-sandboxed, but still follow the same
//     policy on a best-effort basis"), and TestCopilotNativeSandboxBuiltinEdits
//     AreInProcessOnly measures it: with the OS backend unable to start at all,
//     the `create` tool still wrote its file. The write path never enters the
//     sandbox, so its confinement is an in-process check by the same program
//     the sandbox exists to contain.
//
// That is the whole reason this capability stays false. tclaude's contract is
// about the COMPLETE effective boundary, and callers that gate on
// SupportsBuiltinOSSandbox — the access-enforcement table, the spawn
// implementation selector, the effective-policy preview — go on to describe an
// OS-enforced posture to the operator. Describing one for a boundary whose
// file-write half is a userspace policy check would be a claim tclaude cannot
// honor: a bug in that check, a built-in tool it does not cover, or a future
// tool added without one is an unmediated write with the operator's full
// privileges. Claude Code's SRT and Codex's `--sandbox` confine their own edit
// tools through the OS; Copilot's does not, and the difference is the contract.
//
// Three further properties would each need their own answer even if the file
// half were closed, and are recorded so a later revisit starts from them:
//
//  1. The launch-time flags are unusable, which is not the same as absent —
//     TCL-1011 corrected an earlier "there is no launch flag" reading here.
//     `--sandbox` and `--no-sandbox` exist on 1.0.77 (added in 1.0.70, hidden
//     from `copilot --help`) and override the settings file for one launch
//     without persisting, but ONLY under `--experimental`; without it they are
//     parsed and ignored in both directions. `--experimental` is also what
//     registers the in-pane `/sandbox enable|disable`, so the only argv that
//     selects a posture is the same argv that lets the pane revoke it under a
//     running agent. Otherwise the posture lives in the `sandbox` key of
//     COPILOT_HOME/settings.json — and of the legacy COPILOT_HOME/config.json,
//     which the CLI migrates from at startup and which therefore WINS for the
//     launch that consumes it — plus the interactive `/sandbox` dialog and
//     organization policy. tclaude pins per-spawn postures through launch
//     arguments; it cannot pin this one without either owning the operator's
//     own config file or handing the pane that lever.
//  2. It is documented as experimental by its own vendor, so its shape is not
//     yet a contract to build a capability on.
//  3. Availability is host-conditional (Linux needs bwrap AND permitted
//     unprivileged user namespaces). The measured degradation is fail-closed —
//     shell commands error rather than silently running unconfined, see the
//     fail-closed arm of
//     TestCopilotNativeSandboxShellEnforcementIsHostConditional — which is the right
//     behavior, but it is a runtime property tclaude cannot verify at launch.
//
// None of this argues against Copilot under `tclaude-layer`, which is a
// separate wall tclaude owns and a separate ticket.
const CopilotBuiltinOSSandboxAbsenceReason = "GitHub Copilot CLI's command sandboxing " +
	"OS-confines shell commands only: its built-in file edits are checked by an in-process " +
	"policy rather than by the OS, the posture is an experimental setting whose only per-launch " +
	"flags require --experimental (which also lets the pane change it mid-session), and its " +
	"availability is host-conditional, so the effective boundary is not a complete OS sandbox " +
	"tclaude can advertise"
