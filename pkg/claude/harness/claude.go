package harness

import (
	"slices"
	"strings"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
)

// init registers the Claude Code harness as the default. Claude Code is
// the harness tclaude was built around; every contract below preserves
// its existing behavior exactly — this package is a refactor seam, not a
// behavior change.
func init() {
	Register(&Harness{
		Name:        DefaultName,
		DisplayName: "Claude Code",
		Spawn:       claudeSpawner{},
		Models:      claudeModels{},
		Life:        claudeLifecycle{},
	})
}

// claudeSpawner builds the `claude` invocation that runs inside the tmux
// pane. The logic moved here verbatim from session.buildClaudeCmd — the
// "unset omits the flag" guarantee and the shell-quoting are unchanged.
type claudeSpawner struct{}

func (claudeSpawner) Binary() string { return "claude" }

// BuildCommand assembles the `claude` invocation: env exports + the
// binary, an optional --resume, an optional --effort and --model (each
// appended only when an explicit value was chosen — empty leaves claude
// on its own default), then any post-`--` passthrough args. effort and
// model are validated single tokens, but everything is shell-quoted
// anyway; the passthrough args are shell-quoted individually. Kept pure
// so the "unset omits the flag" guarantee is unit-testable without tmux.
func (claudeSpawner) BuildCommand(spec SpawnSpec) string {
	cmd := spec.EnvExports + "claude"
	if spec.ResumeID != "" {
		cmd += " --resume " + spec.ResumeID
	}
	if spec.Effort != "" {
		// Quote defensively even though effort is a validated single
		// token: this string is handed to `sh -c`, so quoting keeps the
		// safety local here rather than trusting every caller to have
		// validated first. For a clean level it is a no-op.
		cmd += " --effort " + clcommon.ShellQuoteArg(spec.Effort)
	}
	if spec.Model != "" {
		// Quoting is load-bearing here, not just defensive: the `[1m]`
		// aliases contain brackets, which sh would otherwise treat as a
		// glob pattern.
		cmd += " --model " + clcommon.ShellQuoteArg(spec.Model)
	}
	if len(spec.ExtraArgs) > 0 {
		quoted := make([]string, len(spec.ExtraArgs))
		for i, a := range spec.ExtraArgs {
			quoted[i] = clcommon.ShellQuoteArg(a)
		}
		cmd += " " + strings.Join(quoted, " ")
	}
	return cmd
}

// claudeModels delegates to the curated clcommon validators so the model
// and effort knowledge stays in one place; the catalog is the
// harness-agnostic dispatch point that a future codex catalog parallels.
type claudeModels struct{}

func (claudeModels) ValidateModel(s string) (string, error) {
	return clcommon.ValidateModel(s)
}

func (claudeModels) ValidateEffort(s string) (string, error) {
	return clcommon.ValidateEffort(s)
}

// Models returns a copy so callers can't mutate the shared source list.
func (claudeModels) Models() []string {
	return slices.Clone(clcommon.ValidModels)
}

// EffortLevels returns a copy so callers can't mutate the shared source list.
func (claudeModels) EffortLevels() []string {
	return slices.Clone(clcommon.ValidEffortLevels)
}

// claudeLifecycle names Claude Code's in-pane control slash commands. All
// three are supported, so CC behavior is unchanged when call sites gate
// injections on these tokens.
type claudeLifecycle struct{}

func (claudeLifecycle) RenameCommand() string  { return "/rename" }
func (claudeLifecycle) CompactCommand() string { return "/compact" }
func (claudeLifecycle) SoftExitCommand() string { return "/exit" }
