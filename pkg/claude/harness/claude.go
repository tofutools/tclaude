package harness

import (
	"fmt"
	"slices"
	"strings"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
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
		Convs:       claudeConvStore{},
		// Claude Code accepts a preset conv-id (--session-id), a launch-time
		// display name (--name, written as a custom-title turn just like
		// /rename), and a positional first-turn prompt — so the daemon can
		// spawn it fully enrolled and skip the post-connect tmux injection.
		LaunchEnrollment: true,
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
	// --session-id pins the conversation id for a FRESH launch only (a
	// resume continues an existing id). It lets the daemon know the conv-id
	// before the pane starts, so the rename + welcome ride in as launch args
	// instead of post-launch tmux injections. Quoted defensively even though
	// it is a validated UUID.
	if spec.SessionID != "" && spec.ResumeID == "" {
		cmd += " --session-id " + clcommon.ShellQuoteArg(spec.SessionID)
	}
	// --name sets the display name at launch; Claude Code records it as a
	// `custom-title` turn the same way /rename does. Quoted because the name
	// is free-ish text handed to `sh -c`.
	if spec.Name != "" {
		cmd += " --name " + clcommon.ShellQuoteArg(spec.Name)
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
	// `claude [options] [prompt]` — a trailing positional prompt the
	// interactive session submits itself at launch (verified against the
	// `claude --help` "[prompt] Your prompt" arg). The daemon spawn path
	// uses it to deliver the agent's welcome turn without a tmux send-keys
	// injection. Only on a FRESH launch: a --resume continues an existing
	// conversation and takes no launch prompt. Quoted as a single arg so the
	// whole prompt is one positional, never split into stray flags/words.
	if spec.InitialPrompt != "" && spec.ResumeID == "" {
		cmd += " " + clcommon.ShellQuoteArg(spec.InitialPrompt)
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

func (claudeLifecycle) RenameCommand() string   { return "/rename" }
func (claudeLifecycle) CompactCommand() string  { return "/compact" }
func (claudeLifecycle) SoftExitCommand() string { return "/exit" }

// claudeConvStore assembles conversations from Claude Code's storage
// model — one cwd-indexed `.jsonl` per conv under ~/.claude/projects — by
// delegating to the existing convops read paths. It's the reference impl
// the Codex ConvStore (JOH-152) plugs in beside; the production callers
// (conv ls / search / dashboard) are rewired to enumerate every
// registered harness in the multi-harness enumeration slice (JOH-153).
type claudeConvStore struct{}

// ListConvs returns the conversations under cwd's Claude project dir, or —
// when cwd is the empty sentinel — every indexed conversation across all
// projects (served from the conv_index cache, the same fast path watch
// mode uses).
func (claudeConvStore) ListConvs(cwd string) ([]convops.SessionEntry, error) {
	if cwd == "" {
		return convops.LoadEntriesFromDB("")
	}
	idx, err := convops.LoadSessionsIndex(convops.GetClaudeProjectPath(cwd))
	if err != nil {
		return nil, err
	}
	return idx.Entries, nil
}

// Resolve maps an id prefix to a conv via the conv_index cache. It
// distinguishes the three outcomes the contract requires: no match
// (nil, nil), an unreadable store (nil, err), and an ambiguous prefix
// (nil, err) — never collapsing the latter two into "not found". An exact
// id match always wins over prefix matches.
func (claudeConvStore) Resolve(idPrefix, cwd string, global bool) (*ConvRef, error) {
	if idPrefix == "" {
		return nil, nil
	}

	var rows []*db.ConvIndexRow
	var err error
	if global {
		rows, err = db.ListAllConvIndex()
	} else {
		rows, err = db.ListConvIndex(convops.GetClaudeProjectPath(cwd))
	}
	if err != nil {
		return nil, fmt.Errorf("resolve conversation %q: %w", idPrefix, err)
	}

	// An exact id match is unambiguous regardless of how many other ids
	// share it as a prefix.
	for _, r := range rows {
		if r.ConvID == idPrefix {
			return claudeConvRef(r), nil
		}
	}

	var matches []*db.ConvIndexRow
	for _, r := range rows {
		if strings.HasPrefix(r.ConvID, idPrefix) {
			matches = append(matches, r)
		}
	}
	switch len(matches) {
	case 0:
		return nil, nil
	case 1:
		return claudeConvRef(matches[0]), nil
	default:
		return nil, fmt.Errorf("ambiguous conversation id %q: matches %d conversations", idPrefix, len(matches))
	}
}

// Title returns the conv's display title, refreshing from the `.jsonl` if
// it changed on disk. Priority mirrors SessionEntry.DisplayTitle:
// customTitle > summary > first prompt. An unknown conv is ("", nil).
func (claudeConvStore) Title(convID string) (string, error) {
	row := convops.RefreshConvIndexEntry(convID)
	if row == nil {
		return "", nil
	}
	switch {
	case row.CustomTitle != "":
		return row.CustomTitle, nil
	case row.Summary != "":
		return row.Summary, nil
	default:
		return row.FirstPrompt, nil
	}
}

// SetTitle is a guard for Claude Code: its title store is the `.jsonl`
// customTitle turn, which only a live CC pane writes (via the `/rename`
// slash command). There is no direct-write path, so agentd renames a CC
// conv by injecting `/rename` (gated on Lifecycle.RenameCommand) and never
// calls this. Returning an error rather than silently writing
// conv_index.custom_title is deliberate: a direct conv_index write would
// be clobbered by the next .jsonl rescan (which finds no customTitle turn).
func (claudeConvStore) SetTitle(convID, title string) error {
	return fmt.Errorf("claude renames via the %q slash injection, not a direct title write", "/rename")
}

func claudeConvRef(r *db.ConvIndexRow) *ConvRef {
	return &ConvRef{
		ConvID:      r.ConvID,
		ProjectPath: r.ProjectPath,
		Harness:     r.Harness,
	}
}
