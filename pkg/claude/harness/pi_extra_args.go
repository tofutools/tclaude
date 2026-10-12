package harness

import (
	"fmt"
	"sort"
	"strings"
)

// piOwnedFlags is the audited pass-through set for pi, taken from
// `pi --help` and docs/cli.md at PiMinimumVersion. The reasoning is Copilot's
// and Gemini's (see ValidateLaunchExtraArgs): a pass-through arg naming an
// option tclaude renders or records produces a pane that disagrees with the
// record. Keys are long names without dashes; short aliases map onto them.
var piOwnedFlags = map[string]string{
	// Identity: which conversation the pane is, and whether it is one at all.
	"continue":    "which conversation the pane attaches to; use tclaude's own resume",
	"resume":      "which conversation the pane attaches to; use tclaude's own resume",
	"session":     "which conversation the pane attaches to; use tclaude's own resume",
	"session-id":  "the conversation id; use --session-id on tclaude itself",
	"fork":        "which conversation the pane attaches to; use tclaude's clone",
	"session-dir": "where the conversation is stored, which tclaude reads",
	"no-session":  "whether the conversation is stored at all",
	// Headless and one-shot modes, which leave no interactive pane.
	"print":       "headless mode; use tclaude ask",
	"mode":        "headless mode; use tclaude ask",
	"export":      "the pane's runtime (it exports and exits)",
	"list-models": "the pane's runtime (it lists models and exits)",
	// Metadata tclaude validates and records.
	"model":    "the model; use --model",
	"provider": "the model; use --model provider/id",
	"models":   "the model; use --model",
	"thinking": "the thinking level; use --effort",
	"name":     "the conversation name; use --name",
	// Credentials never belong on a recorded command line.
	"api-key": "a credential, which must not be recorded in the launch command; use pi's /login or the provider's environment variable",
	// Permission: the tool set is the approval axis, and project trust is --trust-dir.
	"tools":            "the approval posture; use --ask-for-approval",
	"exclude-tools":    "the approval posture; use --ask-for-approval",
	"no-tools":         "the approval posture; use --ask-for-approval",
	"no-builtin-tools": "the approval posture; use --ask-for-approval",
	"approve":          "the project-trust decision; use --trust-dir",
	"no-approve":       "the project-trust decision",
}

// piShortFlags maps pi's short aliases onto their long names.
var piShortFlags = map[string]string{
	"c": "continue", "r": "resume", "p": "print", "n": "name",
	"t": "tools", "xt": "exclude-tools", "nt": "no-tools", "nbt": "no-builtin-tools",
	"a": "approve", "na": "no-approve",
}

// validatePiExtraArgs refuses the first pass-through arg that names a
// tclaude-owned pi option. Everything after a bare `--` is message text, not
// options, and is left alone.
func validatePiExtraArgs(args []string) error {
	for _, arg := range args {
		if arg == "--" {
			return nil
		}
		name := ""
		switch {
		case strings.HasPrefix(arg, "--"):
			name, _, _ = strings.Cut(strings.TrimPrefix(arg, "--"), "=")
		case strings.HasPrefix(arg, "-") && len(arg) > 1:
			name = piShortFlags[strings.TrimPrefix(arg, "-")]
		}
		if remedy, owned := piOwnedFlags[name]; owned {
			return fmt.Errorf("pass-through argument %s names an option tclaude renders and records for this "+
				"pi launch (%s); tclaude-owned pi flags: %s", arg, remedy, strings.Join(piOwnedFlagNames(), " "))
		}
	}
	return nil
}

func piOwnedFlagNames() []string {
	names := make([]string, 0, len(piOwnedFlags))
	for name := range piOwnedFlags {
		names = append(names, "--"+name)
	}
	sort.Strings(names)
	return names
}
