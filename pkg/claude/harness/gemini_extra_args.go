package harness

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
)

// geminiOwnedFlag describes one Gemini CLI option tclaude renders, records, or
// depends on not being moved behind its back.
type geminiOwnedFlag struct {
	axis   string
	remedy string
}

// geminiOwnedFlags is the audited pass-through set for Gemini CLI, read from
// the yargs definition in packages/cli/src/config/config.ts at
// GeminiPinnedVersion. The reasoning is Copilot's (see ValidateLaunchExtraArgs):
// a pass-through arg that names an option tclaude itself renders or records
// produces a pane that disagrees with the record, and every later decision
// made from the record is then made about a launch that did not happen.
//
// Keys are canonical long names WITHOUT the leading dashes; geminiOwnedArg
// normalizes every spelling yargs accepts (short alias, `--x=v`, camelCase,
// `--no-x`) onto them.
var geminiOwnedFlags = map[string]geminiOwnedFlag{
	// Identity: which conversation the pane is.
	"resume":         {"which conversation the pane attaches to", geminiUseResume},
	"session-id":     {"the conversation id", geminiUseResume},
	"session-file":   {"which conversation the pane attaches to", geminiUseResume},
	"list-sessions":  {"the pane's runtime (it lists sessions and exits)", geminiNoRuntime},
	"delete-session": {"the pane's runtime (it deletes a session and exits)", geminiNoRuntime},

	// Identity: the first turn, and headless mode.
	"prompt-interactive": {"the submitted first turn", geminiUsePrompt},
	"prompt":             {"headless mode", geminiNoHeadless},
	"output-format":      {"headless mode", geminiNoHeadless},

	// Identity: the working directory. `--worktree` creates a git worktree
	// and moves the process into it, so the recorded cwd (and the conversation
	// store's project scoping, which follows the cwd) would describe a
	// directory the pane is not in.
	"worktree": {"the working tree, and with it the working directory", geminiUseWorktree},

	// Metadata tclaude validates and records.
	"model": {"the model", geminiUseModel},

	// Permission: approval and trust. tclaude does not model Gemini's
	// approval axis yet, so a pane launched with one of these would run under
	// a posture nothing recorded — refused rather than silently unaccounted.
	"approval-mode": {"the approval mode", geminiNoApproval},
	"yolo":          {"the approval mode", geminiNoApproval},
	"skip-trust":    {"the folder-trust decision", geminiNoTrust},
	"policy":        {"the approval policy", geminiNoApproval},
	"admin-policy":  {"the approval policy", geminiNoApproval},
	"allowed-tools": {"the approval policy", geminiNoApproval},

	// Sandbox: Gemini's own container/Seatbelt sandbox. tclaude's sandbox
	// reasoning describes the pane it launched; re-executing the CLI inside a
	// container moves its filesystem, home and process tree out from under it.
	"sandbox": {"Gemini's own sandbox", geminiNoSandbox},

	// Runtime: what the pane actually is.
	"acp":              {"the pane's protocol", geminiNoRuntime},
	"experimental-acp": {"the pane's protocol", geminiNoRuntime},
}

// geminiShortFlags maps the single-letter aliases yargs registers for the
// options above.
var geminiShortFlags = map[rune]string{
	'r': "resume",
	'i': "prompt-interactive",
	'p': "prompt",
	'o': "output-format",
	'w': "worktree",
	'm': "model",
	'y': "yolo",
	's': "sandbox",
}

const (
	geminiUseResume = "let tclaude own the conversation identity — it pins the id before the " +
		"pane starts and enrolls the agent against it; use `tclaude conv resume <id>` to attach " +
		"to a different conversation"
	geminiUsePrompt = "pass the first turn as the launch's initial message (`--initial-prompt` " +
		"on `session new`, `--initial-message` on `agent spawn`), which tclaude renders as the " +
		"single `-i` and records"
	geminiNoHeadless  = "a tclaude pane is interactive by construction, so drop the flag"
	geminiUseWorktree = "use tclaude's own worktree option, which creates the tree, launches the " +
		"pane inside it and records the directory it used"
	geminiUseModel   = "use tclaude's own `--model` option, which it validates and records"
	geminiNoApproval = "tclaude does not yet model Gemini CLI's approval modes or policy engine, " +
		"so there is no recorded launch a pass-through approval option could agree with; " +
		"configure the default in Gemini's own settings.json instead"
	geminiNoTrust = "tclaude does not yet model Gemini CLI's folder trust; trust the directory " +
		"once in an interactive Gemini session (or in Gemini's trustedFolders.json) instead"
	geminiNoSandbox = "tclaude does not model Gemini CLI's container/Seatbelt sandbox, so there " +
		"is no recorded launch a pass-through sandbox flag could agree with"
	geminiNoRuntime = "tclaude manages a local interactive Gemini TUI in a tmux pane; it has no " +
		"contract for an ACP peer or a one-shot listing command"
)

// validateGeminiExtraArgs is ValidateLaunchExtraArgs' Gemini arm.
func validateGeminiExtraArgs(args []string) error {
	for _, arg := range args {
		flag, owned, ok := geminiOwnedArg(arg)
		if !ok {
			continue
		}
		return fmt.Errorf(
			"pass-through argument %s names %s, which tclaude renders, records or depends on for "+
				"this launch, so the pane would run differently from the launch that was written "+
				"down; %s (audited Gemini CLI options: %s)",
			flag, owned.axis, owned.remedy, strings.Join(geminiOwnedFlagNames(), " "))
	}
	return nil
}

// geminiOwnedArg reports whether one pass-through argument names an audited
// option, in any spelling yargs accepts:
//
//   - `--name` and `--name=value`
//   - camelCase (`--sessionId`): yargs' camel-case-expansion is on by default
//   - boolean negation (`--no-yolo`): still names the option, and a negation
//     of a posture tclaude does not record is just as unrecorded
//   - `-x`, `-xVALUE`, and grouped short flags (`-yi`): every letter of a
//     single-dash token is checked, which may over-refuse a value glued to a
//     short flag but can never miss an audited alias
//
// Positional tokens (no leading dash) are never options and pass through.
func geminiOwnedArg(arg string) (string, geminiOwnedFlag, bool) {
	token := strings.TrimSpace(arg)
	switch {
	case token == "--" || !strings.HasPrefix(token, "-"):
		return "", geminiOwnedFlag{}, false
	case strings.HasPrefix(token, "--"):
		name := strings.TrimPrefix(token, "--")
		if before, _, ok := strings.Cut(name, "="); ok {
			name = before
		}
		name = geminiKebab(name)
		if owned, found := geminiOwnedFlags[name]; found {
			return "--" + name, owned, true
		}
		if negated, ok := strings.CutPrefix(name, "no-"); ok {
			if owned, found := geminiOwnedFlags[negated]; found {
				return "--" + name, owned, true
			}
		}
		return "", geminiOwnedFlag{}, false
	default:
		letters := strings.TrimPrefix(token, "-")
		if before, _, ok := strings.Cut(letters, "="); ok {
			letters = before
		}
		for _, r := range letters {
			if long, found := geminiShortFlags[r]; found {
				return "-" + string(r), geminiOwnedFlags[long], true
			}
		}
		return "", geminiOwnedFlag{}, false
	}
}

// geminiKebab converts a camelCase option name to the kebab-case spelling the
// audited set is keyed by. Already-kebab names are returned unchanged.
func geminiKebab(name string) string {
	var b strings.Builder
	for i, r := range name {
		if unicode.IsUpper(r) {
			if i > 0 {
				b.WriteByte('-')
			}
			b.WriteRune(unicode.ToLower(r))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func geminiOwnedFlagNames() []string {
	names := make([]string, 0, len(geminiOwnedFlags)+len(geminiShortFlags))
	for name := range geminiOwnedFlags {
		names = append(names, "--"+name)
	}
	for r := range geminiShortFlags {
		names = append(names, "-"+string(r))
	}
	sort.Strings(names)
	return names
}
