package agentd

import (
	"regexp"
	"sort"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// auto_permit_conditions.go is the vocabulary of the auto-permit feature: the
// NAMED permission-prompt conditions an operator may pre-consent to per agent.
//
// The registry is compile-time and deliberately narrow. This is not a blanket
// "accept everything" mode — `--dangerously-skip-permissions` already covers
// that case and is the honest way to ask for it. What this exists for is the
// prompt an allow-rule cannot reach: Claude Code's EnterWorktree safety check
// is a hardcoded gate that ignores allow-rules, the auto-mode classifier and
// PreToolUse hook approvals alike, so an operator who is perfectly happy for
// their agent to enter a worktree unattended has, today, no configuration
// anywhere that expresses that consent. One named condition per such prompt is
// the whole of the design.
//
// Each condition carries two independent matchers, and BOTH must hold before a
// key is injected:
//
//   - DetailMatch runs against the session row's status_detail — the tool name
//     (PermissionRequest hook) or notification message the harness reported
//     when it went into awaiting_permission. This is the cheap DB-side filter
//     that decides whether a pane is worth reading at all.
//   - PaneRequire runs against the pane's captured text. This is the real
//     gate: it proves the specific dialog this condition names is on screen
//     right now, so an Enter cannot land on some other prompt that happened to
//     arrive between the status read and the injection.
//
// Both are matched against text the HARNESS renders, so the failure direction
// is the safe one: if Claude Code rewords the dialog, nothing matches, nothing
// is auto-answered, and the prompt simply waits for the human as it does today.
type autoPermitCondition struct {
	// Name is the stable, operator-facing identifier — lower-case kebab-case,
	// matching db.NormalizeAutoPermitCondition, and what is stored in the
	// opt-in table and printed by the CLI.
	Name string
	// Summary is the one-line description shown by `tclaude agent auto-permit
	// ls` and in the CLI help, so an operator can see what they are consenting
	// to without reading this file.
	Summary string
	// Harness names the harness whose dialog this condition describes. The
	// prompt text is harness-specific, so a condition never fires against a
	// session running a different harness even if its detail happened to match.
	Harness string
	// DetailMatch gates on the session row's status_detail.
	DetailMatch *regexp.Regexp
	// PaneRequire are the patterns the captured pane must ALL contain. Written
	// as several narrow patterns rather than one big one so each states a
	// separate fact: which dialog is up, and that it is a live choice prompt
	// rather than a transcript echo of a past one.
	PaneRequire []*regexp.Regexp
	// AcceptKeys are the tmux key names injected to accept, in order. They are
	// compile-time constants — nothing operator- or agent-controlled ever
	// reaches the send-keys argv from this path.
	AcceptKeys []string
}

// autoPermitConditions is the registry. Adding an entry here is the only way to
// widen what auto-permit can answer; see the file comment for the bar a new
// entry has to clear.
var autoPermitConditions = []autoPermitCondition{
	{
		Name: "enter-worktree",
		Summary: "Claude Code's EnterWorktree safety check — the hardcoded " +
			"\"Enter the worktree at …?\" confirmation shown when a session moves its " +
			"working directory and write access into a worktree. Answers Yes.",
		Harness: harness.DefaultName,
		// The hook reports the tool name ("EnterWorktree"); the legacy
		// notification path reports the dialog's question text instead. Accept
		// either, since which one a given Claude Code build emits is not
		// something this side controls.
		DetailMatch: regexp.MustCompile(`(?i)enterworktree|enter the worktree at`),
		PaneRequire: []*regexp.Regexp{
			// The question this specific safety check asks.
			regexp.MustCompile(`(?i)enter the worktree at`),
			// The reject option of Claude Code's choice dialog. Its presence is
			// what distinguishes a LIVE prompt awaiting a keystroke from the
			// transcript of one that was already answered.
			regexp.MustCompile(`(?i)no, and tell claude what to do differently`),
		},
		// The dialog opens with "Yes" highlighted, so a single Enter accepts it
		// — the same keystroke the human presses. Deliberately not a digit
		// ("1"): if the dialog were somehow not up, a stray Enter submits an
		// empty prompt (harmless), while a stray digit types a character into
		// the composer that the agent's next message would carry.
		AcceptKeys: []string{"Enter"},
	},
}

// autoPermitConditionNames returns every registered condition name, sorted. Used
// by the CLI listing and by the boundary validator that refuses an unknown name
// at write time.
func autoPermitConditionNames() []string {
	out := make([]string, 0, len(autoPermitConditions))
	for _, c := range autoPermitConditions {
		out = append(out, c.Name)
	}
	sort.Strings(out)
	return out
}

// lookupAutoPermitCondition returns the registered condition by name, or nil.
func lookupAutoPermitCondition(name string) *autoPermitCondition {
	name = strings.TrimSpace(name)
	for i := range autoPermitConditions {
		if autoPermitConditions[i].Name == name {
			return &autoPermitConditions[i]
		}
	}
	return nil
}

// matchesDetail reports whether a session's harness + status_detail are the ones
// this condition describes. It is the cheap pre-filter: a true here only earns
// the pane read, never the keystroke.
func (c *autoPermitCondition) matchesDetail(harnessName, detail string) bool {
	if c.DetailMatch == nil {
		return false
	}
	// Session rows coalesce an empty harness to the default on write; a legacy
	// row that predates that is read the same way rather than skipped.
	if harnessName == "" {
		harnessName = harness.DefaultName
	}
	if harnessName != c.Harness {
		return false
	}
	return c.DetailMatch.MatchString(detail)
}

// matchesPane reports whether the captured pane shows this condition's dialog,
// live and awaiting a keystroke. Every PaneRequire pattern must hit; a condition
// with no patterns never matches, so a malformed registry entry fails closed.
func (c *autoPermitCondition) matchesPane(pane string) bool {
	if len(c.PaneRequire) == 0 || strings.TrimSpace(pane) == "" {
		return false
	}
	for _, re := range c.PaneRequire {
		if !re.MatchString(pane) {
			return false
		}
	}
	return true
}
