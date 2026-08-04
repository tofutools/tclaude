package harness

import (
	"fmt"
	"sort"
	"strings"
)

// ValidateLaunchExtraArgs refuses pass-through args that would move a posture
// tclaude RECORDS.
//
// The problem it closes is specific to Copilot and specific to this ticket. The
// approval catalog renders permission flags into the launch command, the spawn
// row records which token produced them, and approval lineage and relaunch both
// reason from that recorded token. Pass-through args are appended to the same
// command line, so an operator (or a spawn profile, or a group default) that
// slipped `--allow-all-paths` into ExtraArgs would run a pane whose effective
// posture is broader than the one tclaude wrote down — and every later
// delegation and relaunch decision would be made against the wrong record.
//
// Ordering is deliberately NOT the defence. The catalog's flags happen to
// precede ExtraArgs today, but nothing in the permission matrix establishes what
// Copilot 1.0.77 does with a duplicated or contradictory permission flag, and
// "the last one wins" is exactly the kind of plausible guess this ticket keeps
// getting wrong. A launch whose posture depends on unmeasured duplicate-flag
// semantics is refused instead.
//
// It is a REFUSAL rather than a filter for the same reason tclaude refuses
// elsewhere: silently dropping an argument an operator passed would leave them
// believing a flag took effect. The error names the flag so the fix is obvious.
//
// Harnesses other than Copilot are unaffected — they have their own posture
// plumbing and their own gates, and widening this to them is not in TCL-973's
// scope. Ordinary non-posture args (`--log-level=debug`, `--banner`, a model
// override, anything the operator wants) keep working.
func ValidateLaunchExtraArgs(h *Harness, args []string) error {
	if h == nil || h.Name != CopilotName || len(args) == 0 {
		return nil
	}
	for _, arg := range args {
		flag, ok := copilotPostureMovingArg(arg)
		if !ok {
			continue
		}
		return fmt.Errorf(
			"pass-through argument %s moves the Copilot permission posture that tclaude records "+
				"(%s), so the launch would run under a posture different from the one written down "+
				"and used for approval lineage and relaunch; select the posture with the approval "+
				"policy instead of passing the flag, and let the sandbox profile supply directory "+
				"grants (posture-moving flags: %s)",
			flag, copilotPostureAxis(flag), strings.Join(copilotPostureMovingFlagNames(), " "))
	}
	return nil
}

// copilotPostureMovingFlags are every Copilot 1.0.77 option that changes what
// the pane may do without asking, mapped to the axis it moves.
//
// It covers the whole permission surface rather than only the flags the catalog
// itself renders, because the risk is a MISMATCH between the recorded token and
// the running pane — and a flag tclaude never emits creates exactly as large a
// mismatch as one it does. The path movers are here for the reason the lead
// named: the catalog's `--add-dir` values are derived from the resolved sandbox
// profile, and a pass-through copy would grant a directory that set never
// contained.
var copilotPostureMovingFlags = map[string]string{
	// Tool approval.
	"--allow-all-tools": "tool approval",
	"--allow-tool":      "tool approval",
	"--deny-tool":       "tool approval",
	// The advertised tool CATALOG, which is a different lever with the same
	// consequence. Removing a tool cannot prompt, so these look like they only
	// narrow — but the catalog is exactly how --no-ask-user closes the ask_user
	// deadlock (measured on the provider request body, contract: no-ask-user),
	// so a pass-through copy moves the same axis the approval token owns. They
	// also belong to the ToolGovernance contract, which is deliberately still
	// nil: a launch that used them would be running under a contract tclaude
	// has not implemented and records nothing about.
	"--available-tools": "the advertised tool catalog",
	"--excluded-tools":  "the advertised tool catalog",
	// The ask_user deadlock source.
	"--no-ask-user": "the ask_user tool",
	// URL access.
	"--allow-all-urls": "URL access",
	"--allow-url":      "URL access",
	"--deny-url":       "URL access",
	// Path access. --add-dir is allowed as a CATALOG-rendered flag, whose
	// values come from the resolved sandbox profile; a pass-through copy is
	// not, because it would widen the grant set beyond what was recorded.
	"--allow-all-paths":   "directory access",
	"--add-dir":           "directory access",
	"--disallow-temp-dir": "directory access",
	"--allow-all":         "every permission axis at once",
	"--yolo":              "every permission axis at once",
	// Agent mode changes how autonomously the pane runs and is not a posture
	// tclaude records, so a launch that sets it would be unaccounted for in the
	// same way. Autopilot is its own axis (with forced-continuation semantics)
	// and belongs to its own ticket, not to a pass-through arg.
	"--mode":      "agent mode",
	"--plan":      "agent mode",
	"--autopilot": "agent mode",
	// Audited alongside the mode selectors rather than argued to be harmless
	// once they are refused. It would be a decent argument — the continue
	// budget only means something in autopilot, which no accepted launch can
	// select — but it rests on tclaude knowing every way autopilot can be
	// entered, and `stayInAutopilot` is a CONFIG key, so a pane could be in
	// autopilot without any flag saying so. Auditing the flag costs nothing and
	// does not depend on that reasoning holding.
	"--max-autopilot-continues": "agent mode",
}

// copilotPostureMovingArg reports whether one pass-through argument names a
// posture-moving flag, in either spelling the CLI accepts.
//
// Both spellings are checked because Copilot's option table uses both — `--name=
// VALUE` and `--session-id ID` appear side by side — so an audit that only knew
// one form would be trivially bypassed by writing the other. The `=` form is
// split on the FIRST `=`, since a value may itself contain one
// (`--deny-tool=url(https://x?a=b)`).
func copilotPostureMovingArg(arg string) (string, bool) {
	name := strings.TrimSpace(arg)
	if before, _, ok := strings.Cut(name, "="); ok {
		name = before
	}
	if _, found := copilotPostureMovingFlags[name]; found {
		return name, true
	}
	return "", false
}

func copilotPostureAxis(flag string) string { return copilotPostureMovingFlags[flag] }

// copilotPostureMovingFlagNames returns the audited set, sorted, so the refusal
// message lists it deterministically.
func copilotPostureMovingFlagNames() []string {
	names := make([]string, 0, len(copilotPostureMovingFlags))
	for name := range copilotPostureMovingFlags {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
