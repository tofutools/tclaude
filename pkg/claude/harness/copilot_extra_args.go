package harness

import (
	"fmt"
	"sort"
	"strings"
)

// ValidateLaunchExtraArgs refuses pass-through args that would make a Copilot
// pane disagree with the launch tclaude RECORDED.
//
// The problem it closes is specific to Copilot and specific to this ticket.
// tclaude renders a Copilot launch from a SpawnSpec — permission flags from the
// approval token, `--session-id`/`--resume=` for identity, `--name`/`--model`/
// `--effort` for metadata, `-i` for the first turn — and writes what it rendered
// into the session row. Pass-through args are appended to that same command
// line, so an arg naming any of those same options produces a pane whose real
// launch differs from the record, and every later decision made from the record
// is then made about a launch that did not happen.
//
// Three distinct consequences, all the same class:
//
//   - PERMISSION. `-- --allow-all-paths` runs broader than the recorded token,
//     and approval lineage and relaunch both reason from that token.
//   - IDENTITY. `-- --resume=<other-id>` attaches the pane to a different
//     conversation than the one tclaude enrolled, so hooks, status, the
//     conversation index and the agent record all describe the wrong session.
//     `--continue`/`--connect` and a duplicate `-i` are the same failure: a
//     different conversation, or a second first turn nobody recorded.
//   - METADATA. A duplicate `--model`/`--effort`/`--name` makes the dashboard,
//     the usage accounting or the conversation title describe a launch the pane
//     is not running.
//
// Ordering is deliberately NOT the defence for any of them. tclaude's own flags
// happen to precede ExtraArgs today, but nothing in the permission matrix
// establishes what Copilot 1.0.77 does with a duplicated or contradictory
// option, and "the last one wins" is exactly the kind of plausible guess this
// ticket keeps getting wrong. A launch whose outcome would depend on unmeasured
// duplicate-flag semantics is refused instead.
//
// It is a REFUSAL rather than a filter for the same reason tclaude refuses
// elsewhere: silently dropping an argument an operator passed would leave them
// believing a flag took effect. The error names the flag AND the dedicated
// option that does the same job honestly, so the fix is a rewrite rather than a
// dead end.
//
// KNOWN LIMIT, stated rather than papered over: this matches flag NAMES exactly
// (plus the `=value` and glued-short-value spellings). It therefore assumes
// 1.0.77's parser accepts no other way of writing them — no prefix
// abbreviation, no camelCase expansion, no `--no-` negation. Copilot's own
// option table (`-r, --resume[=VALUE]`) is the shape of a parser that does none
// of those, and no scenario has produced a counterexample, but no scenario has
// looked either. It is cheap to fixture with the existing rig and worth doing
// before this audit is relied on as a boundary rather than a guardrail.
//
// Harnesses other than Copilot are unaffected — they have their own launch
// plumbing and their own gates, and widening this to them is not in TCL-973's
// scope. Ordinary args (`--log-level=debug`, `--banner`, `--no-color`, anything
// tclaude neither renders nor records) keep working.
func ValidateLaunchExtraArgs(h *Harness, args []string) error {
	if h == nil || h.Name != CopilotName || len(args) == 0 {
		return nil
	}
	for _, arg := range args {
		flag, owned, ok := copilotOwnedArg(arg)
		if !ok {
			continue
		}
		return fmt.Errorf(
			"pass-through argument %s names %s, which tclaude renders and records for this launch, "+
				"so the pane would run differently from the launch that was written down; %s "+
				"(tclaude-owned Copilot flags: %s)",
			flag, owned.axis, owned.remedy, strings.Join(copilotOwnedFlagNames(), " "))
	}
	return nil
}

// copilotOwnedFlag describes one option tclaude renders and records.
type copilotOwnedFlag struct {
	// axis names what the flag moves, in the error message's voice.
	axis string
	// remedy names the dedicated way to get the same thing honestly. A refusal
	// that cannot say "do this instead" trains operators to work around it.
	remedy string
}

// copilotOwnedFlags is every Copilot 1.0.77 option tclaude itself renders, plus
// every option that changes what the pane may do without asking.
//
// The permission half covers the whole permission surface rather than only the
// flags the catalog emits, because the risk is a MISMATCH between the recorded
// posture and the running pane — and a flag tclaude never emits creates exactly
// as large a mismatch as one it does.
var copilotOwnedFlags = map[string]copilotOwnedFlag{
	// --- Permission: tool approval ---
	"--allow-all-tools": {"tool approval", copilotUseApproval},
	"--allow-tool":      {"tool approval", copilotUseApproval},
	"--deny-tool":       {"tool approval", copilotUseApproval},
	// The advertised tool CATALOG, which is a different lever with the same
	// consequence. Removing a tool cannot prompt, so these look like they only
	// narrow — but the catalog is exactly how --no-ask-user closes the ask_user
	// deadlock (measured on the provider request body, contract: no-ask-user),
	// so a pass-through copy moves the same axis the approval token owns. They
	// also belong to the ToolGovernance contract, which is deliberately still
	// nil: a launch using them would run under a contract tclaude has not
	// implemented and records nothing about.
	"--available-tools": {"the advertised tool catalog", copilotUseApproval},
	"--excluded-tools":  {"the advertised tool catalog", copilotUseApproval},

	// --- Permission: the ask_user deadlock source ---
	"--no-ask-user": {"the ask_user tool", copilotUseApproval},

	// --- Permission: URL access ---
	"--allow-all-urls": {"URL access", copilotUseApproval},
	"--allow-url":      {"URL access", copilotUseApproval},
	"--deny-url":       {"URL access", copilotUseApproval},

	// --- Permission: path access ---
	// --add-dir is legitimate as a CATALOG-rendered flag, whose values come
	// from the resolved sandbox profile; a pass-through copy is not, because it
	// would widen the grant set beyond what was recorded.
	"--allow-all-paths":   {"directory access", copilotUsePaths},
	"--add-dir":           {"directory access", copilotUsePaths},
	"--disallow-temp-dir": {"directory access", copilotUsePaths},
	"--allow-all":         {"every permission axis at once", copilotUseApproval},
	"--yolo":              {"every permission axis at once", copilotUseApproval},

	// --- Agent mode ---
	// Not a posture tclaude records at all, so a launch that set it would be
	// unaccounted for in the same way. Autopilot is its own axis (with forced-
	// continuation semantics) and belongs to its own ticket, not to a
	// pass-through arg. --max-autopilot-continues is audited alongside the
	// selectors rather than argued harmless once they are refused: the argument
	// is decent — the continue budget only means anything in autopilot, which no
	// accepted launch can select — but it rests on tclaude knowing every route
	// into autopilot, and `stayInAutopilot` is a CONFIG key, so a pane can be in
	// autopilot with no flag saying so.
	"--mode":                    {"the agent mode", copilotNoAgentMode},
	"--plan":                    {"the agent mode", copilotNoAgentMode},
	"--autopilot":               {"the agent mode", copilotNoAgentMode},
	"--max-autopilot-continues": {"the agent mode", copilotNoAgentMode},

	// --- Headless mode ---
	// The spawner's own comment says `-p` must never appear in a TUI launch
	// because it exits after completion, but the consequence is bigger than a
	// pane that exits: the no-TTY tool-approval fallback auto-ALLOWS, while the
	// no-TTY PATH fallback auto-DENIES — two different fallbacks in the same
	// binary, neither of which describes a pane. Whether `-p` inside tmux counts
	// as no-TTY is not measured, so by this file's standard it is refused rather
	// than reasoned about.
	"-p":       {"headless mode, whose no-TTY permission fallbacks are unmeasured", copilotNoHeadless},
	"--prompt": {"headless mode, whose no-TTY permission fallbacks are unmeasured", copilotNoHeadless},

	// --- Identity: which conversation the pane actually is ---
	// The sharpest of the three consequences. tclaude pins the conv-id before
	// the pane starts (LaunchEnrollment) and enrolls the agent against it; a
	// pass-through --resume/--continue/--connect attaches the pane to a
	// DIFFERENT conversation, so hooks, status, the conversation index and the
	// agent record all describe a session that is not the one running. --resume
	// also accepts id prefixes and session names, so this need not even be a
	// deliberate act to land somewhere unintended.
	"--resume":     {"which conversation the pane attaches to", copilotUseResume},
	"-r":           {"which conversation the pane attaches to", copilotUseResume},
	"--session-id": {"the conversation id", copilotUseResume},
	"--continue":   {"which conversation the pane attaches to", copilotUseResume},
	"--connect":    {"which conversation the pane attaches to", copilotUseResume},

	// --- Identity: the first turn ---
	// tclaude emits `-i <prompt>` last and records the briefing it sent. A
	// second one submits a turn nothing recorded, into a pane whose first turn
	// the daemon is waiting on.
	"-i":            {"the submitted first turn", copilotUsePrompt},
	"--interactive": {"the submitted first turn", copilotUsePrompt},

	// --- Metadata: recorded and rendered in the dashboard ---
	// These do not move the permission boundary, and they are audited anyway:
	// tclaude has a dedicated SpawnSpec field for each, records the value, and
	// renders it into the argv. A duplicate makes the dashboard, the usage
	// accounting or the conversation title describe a launch the pane is not
	// running — under the same unmeasured duplicate-flag semantics the rest of
	// this file refuses to depend on.
	"--model":  {"the model", copilotUseModel},
	"--effort": {"the reasoning effort", copilotUseEffort},
	"--name":   {"the session name", copilotUseName},
}

// The remedies. Each names the dedicated option that does the same job while
// keeping the record honest.
const (
	copilotUseApproval = "select it with the approval policy (the `--ask-for-approval` option, " +
		"or the spawn profile's approval setting) so the posture is recorded"
	copilotUsePaths = "let the sandbox profile supply directory grants; tclaude renders one " +
		"--add-dir per granted directory from the profile it recorded"
	copilotNoAgentMode = "tclaude does not model Copilot's agent mode, so there is no recorded " +
		"launch a pass-through mode selector could agree with"
	copilotNoHeadless = "a tclaude pane is interactive by construction, so drop the flag"
	copilotUseResume  = "let tclaude own the conversation identity — it pins the id before the " +
		"pane starts and enrolls the agent against it; use `tclaude conv resume <id>` to attach " +
		"to a different conversation"
	copilotUsePrompt = "pass the first turn as the launch's initial message, which tclaude " +
		"renders as the single `-i` and records"
	copilotUseModel  = "use tclaude's own `--model` option, which it validates and records"
	copilotUseEffort = "use tclaude's own `--effort` option, which it validates and records"
	copilotUseName   = "use tclaude's own `--name` option, which it records as the conversation title"
)

// copilotOwnedArg reports whether one pass-through argument names a
// tclaude-owned flag, in any spelling the CLI accepts.
//
// Both long spellings are checked because Copilot's option table uses both —
// `--name=VALUE` and `--session-id ID` appear side by side — so an audit that
// knew only one form would be trivially bypassed by writing the other. The `=`
// form is split on the FIRST `=`, since a value may itself contain one
// (`--deny-tool=url(https://x?a=b)`).
func copilotOwnedArg(arg string) (string, copilotOwnedFlag, bool) {
	name := strings.TrimSpace(arg)
	if before, _, ok := strings.Cut(name, "="); ok {
		name = before
	}
	if owned, found := copilotOwnedFlags[name]; found {
		return name, owned, true
	}
	// Short flags may also be written glued to their value (`-pfoo`, `-rID`),
	// which the `=` split above does not reach. Only single-dash entries are
	// checked this way: a prefix match on the long flags would reject unrelated
	// arguments that merely start with an audited name.
	for flag, owned := range copilotOwnedFlags {
		if strings.HasPrefix(flag, "--") || !strings.HasPrefix(flag, "-") {
			continue
		}
		if strings.HasPrefix(name, flag) && len(name) > len(flag) {
			return flag, owned, true
		}
	}
	return "", copilotOwnedFlag{}, false
}

// copilotOwnedFlagNames returns the audited set, sorted, so the refusal message
// lists it deterministically.
func copilotOwnedFlagNames() []string {
	names := make([]string, 0, len(copilotOwnedFlags))
	for name := range copilotOwnedFlags {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
