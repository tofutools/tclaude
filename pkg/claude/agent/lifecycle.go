package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/common/table"
	"github.com/tofutools/tclaude/pkg/common"
)

// Self-lifecycle commands: compact, reincarnate, context-info.
//
// `compact` mirrors `tclaude agent rename` — the daemon types `/compact`
// into the caller's own CC pane via tmux send-keys, gated on
// `self.compact`. Compact preserves the conv ID, so identity (groups,
// permissions, name) survives.
//
// `reincarnate` is the heavier replacement for what was once `clear`.
// `/clear` rotates CC's conv ID and orphans agent identity, so we don't
// inject it. Instead the daemon snapshots the old conv's identity,
// spawns a fresh CC session, migrates identity to the new conv, and
// soft-stops the old one. Implementation: see reincarnate.go.

// --- agent compact ---

type compactParams struct {
	FollowUp string `pos:"true" optional:"true" help:"Optional follow-up prompt to submit after /compact lands (quote multi-word strings)"`
	Target   string `long:"target" optional:"true" help:"Compact ANOTHER agent's pane instead of self. Selector: title, full conv-id, or 8+-char prefix. Requires the agent.compact permission, or being an owner of a group containing the target."`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s'). Capped at 300s. Timeout = deny. Self-target only."`
}

func compactCmd() *cobra.Command {
	return boa.CmdT[compactParams]{
		Use:   "compact",
		Short: "Compact a conversation (self by default, or another with --target)",
		Long: "Asks tclaude agentd to inject `/compact` into a CC pane. " +
			"By default the target is the calling agent itself (requires the " +
			"`self.compact` permission). Use --target <selector> to compact ANOTHER " +
			"agent — the manager pattern (requires `agent.compact`, or being an " +
			"owner of a group containing the target). " +
			"\n\n" +
			"An optional follow-up prompt is queued as the next turn after compact " +
			"settles — the bytes wait in the pty until CC resumes reading. Note: " +
			"there's no clean way for the daemon to know exactly when /compact " +
			"completes, so the follow-up may land in a still-busy textarea on " +
			"unlucky timing.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *compactParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Target).SetAlternativesFunc(completeConvSelectors)
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *compactParams, _ *cobra.Command, _ []string) {
			os.Exit(runSlashWithOptionalTarget(p.FollowUp, p.Target, p.AskHuman, "compact", os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

// --- agent context-info ---

type contextInfoParams struct {
	Target string `long:"target" optional:"true" help:"Show ANOTHER agent's context-window state instead of self. Selector: title, full conv-id, or 8+-char prefix. Requires the agent.context-info permission, or being an owner of a group containing the target."`
	Group  string `long:"group" optional:"true" help:"Show the context-window state of EVERY member of a group at a glance (name or id). Read-only; requires the agent.context-info permission, or being an owner of the group."`
	JSON   bool   `long:"json" help:"Output JSON"`
}

func contextInfoCmd() *cobra.Command {
	return boa.CmdT[contextInfoParams]{
		Use:   "context-info",
		Short: "Show context-window state (self, --target an agent, or --group a whole team)",
		Long: "Reads the latest context_pct (populated by tclaude's statusline hook) and any pending /compact claim. " +
			"By default reports the calling agent's own state (read-only, no permission slug). " +
			"\n\n" +
			"--target <selector> reports ANOTHER agent's state — the manager pattern, for a lead watching a worker " +
			"approach its context limit (requires `agent.context-info`, or being an owner of a group containing the " +
			"target). --group <name|id> lists every member of a group in one table so a lead can spot anyone running " +
			"hot (requires `agent.context-info`, or owning the group). --target and --group are mutually exclusive.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *contextInfoParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Target).SetAlternativesFunc(completeConvSelectors)
			boa.GetParamT(ctx, &p.Group).SetAlternativesFunc(completeGroupNames)
			return nil
		},
		RunFunc: func(p *contextInfoParams, _ *cobra.Command, _ []string) {
			os.Exit(runContextInfo(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

// --- shared run helpers ---

// isValidFollowUp mirrors the daemon-side check; the daemon is the
// security boundary, but a fast local error keeps a doomed prompt from
// hitting the wire.
func isValidFollowUp(s string) bool {
	if s == "" || len(s) > 4096 {
		return false
	}
	for _, r := range s {
		if r == ' ' {
			continue
		}
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// MaxInitialMessageBytes caps a spawn task-brief (`--initial-message`).
// The brief is delivered to the new agent's inbox as a SQLite
// agent_messages row — not typed into a tmux pane — so it carries none
// of the follow-up's tmux-invocation constraints (see isValidFollowUp,
// capped far lower for that reason) and can comfortably hold a detailed
// multi-paragraph brief. Both the client-side validator here and the
// daemon-side one (agentd/handlers.go) share this constant so their
// caps stay identical.
const MaxInitialMessageBytes = 16384

// isValidInitialMessage mirrors the daemon-side isValidInitialMessage
// check (agentd/handlers.go). Unlike a follow-up — which is typed into
// a tmux pane — a spawn's initial message is delivered to the new
// agent's inbox, so newlines and tabs are allowed; only NUL / escape /
// other non-text control characters are rejected. An empty string is
// valid (no initial message). Length is capped at MaxInitialMessageBytes.
func isValidInitialMessage(s string) bool {
	if len(s) > MaxInitialMessageBytes {
		return false
	}
	for _, r := range s {
		if r == '\n' || r == '\t' {
			continue
		}
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// MaxSpawnNameLen caps an agent's spawn name. It matches the rename
// title's 64-char display cap (a spawn name becomes the conversation
// title via the post-spawn /rename injection), so a name that clears
// this also clears the daemon's downstream isValidRenameTitle gate.
// Both the client-side validator here and the daemon-side one
// (agentd/handlers.go) share this constant so their caps stay identical.
const MaxSpawnNameLen = 64

// isValidSpawnName mirrors the daemon-side isValidSpawnName check
// (agentd/handlers.go). It enforces the agent-name charset at the spawn
// boundary: ASCII letters, digits, '_' and '-' only — no spaces,
// punctuation, or unicode. An empty name is valid (the name is optional;
// the agent gets an auto-generated label). This is intentionally
// stricter than isValidRenameTitle: a spawn name doubles as a git
// worktree branch name (the dashboard's name→branch sync), so it must be
// a safe branch token, and the strict set is a clean subset of the
// rename charset so a non-empty validated name always clears the
// downstream /rename gate.
func isValidSpawnName(name string) bool {
	if len(name) > MaxSpawnNameLen {
		return false
	}
	for _, r := range name {
		if !isSpawnNameRune(r) {
			return false
		}
	}
	return true
}

// isSpawnNameRune reports whether r is in the spawn-name charset: ASCII
// letters, digits, '-' and '_'. Factored out so isValidSpawnName (the
// accept set) and NormalizeSpawnName (the coerce set) read off ONE
// definition and can't drift.
func isSpawnNameRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z':
	case r >= 'A' && r <= 'Z':
	case r >= '0' && r <= '9':
	case r == '-' || r == '_':
	default:
		return false
	}
	return true
}

// NormalizeSpawnName coerces an arbitrary string into a valid spawn name —
// the safe [A-Za-z0-9_-] branch-token charset isValidSpawnName accepts —
// so any name a human types "just works" instead of being rejected. It is
// the opt-out-able auto-normalization the spawn surfaces (the dashboard
// modal, `tclaude agent spawn`, the daemon's spawn boundary) apply when
// config's agent.spawn_name_normalize is on (the default).
//
// Every maximal run of disallowed characters (spaces, punctuation, unicode,
// control chars) collapses to a single '-', and leading/trailing '-'
// introduced this way are trimmed — so "code reviewer!" → "code-reviewer"
// and "[café]" → "caf". The result is then capped at MaxSpawnNameLen (it is
// all ASCII, so bytes == runes), re-trimming a trailing '-' a mid-run cut
// may leave.
//
// It is idempotent: normalizing its own output returns it unchanged, and
// the output always satisfies isValidSpawnName, so a caller can
// normalize-then-spawn with no second rejection. An all-disallowed input
// (e.g. "🎉") normalizes to "" — still valid, since an empty spawn name is
// the "just give me an auto-labelled agent" case.
func NormalizeSpawnName(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	prevSep := false
	for _, r := range name {
		if isSpawnNameRune(r) {
			b.WriteRune(r)
			prevSep = false
		} else if !prevSep {
			// Collapse a run of disallowed chars into one separator.
			b.WriteByte('-')
			prevSep = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > MaxSpawnNameLen {
		out = strings.TrimRight(out[:MaxSpawnNameLen], "-")
	}
	return out
}

// runSlashWithOptionalTarget dispatches to either the self endpoint
// (/v1/whoami/{verb}) or the cross-agent endpoint (/v1/agent/{target}/{verb})
// based on whether target is set. The cross-agent path doesn't honour
// --ask-human (manager pattern is opt-in via explicit slug grants).
func runSlashWithOptionalTarget(followUp, target, askHuman, label string, stdout, stderr io.Writer) int {
	target = strings.TrimSpace(target)
	if target == "" {
		return runSelfSlash(followUp, askHuman, "/v1/whoami/"+label, label, stdout, stderr)
	}
	if askHuman != "" {
		fmt.Fprintln(stderr, "Error: --ask-human is only supported when targeting self; cross-agent calls require an explicit slug grant or group ownership.")
		return rcInvalidArg
	}
	followUp = strings.TrimSpace(followUp)
	if followUp != "" && !isValidFollowUp(followUp) {
		fmt.Fprintln(stderr, "Error: REJECTED. Follow-up must be 1-4096 printable characters; control")
		fmt.Fprintln(stderr, "characters (newlines, tabs, etc.) are not allowed because each newline")
		fmt.Fprintln(stderr, "would be treated as a separate prompt-submit by tmux send-keys.")
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	body := map[string]string{}
	if followUp != "" {
		body["follow_up"] = followUp
	}
	var resp struct {
		ConvID     string `json:"conv_id"`
		CallerConv string `json:"caller_conv,omitempty"`
		Action     string `json:"action"`
		FollowUp   string `json:"follow_up,omitempty"`
		Note       string `json:"note,omitempty"`
	}
	path := "/v1/agent/" + url.PathEscape(target) + "/" + label
	if err := DaemonRequest(http.MethodPost, path, body, &resp, DaemonOpts{}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if resp.FollowUp != "" {
		fmt.Fprintf(stdout, "Submitted /%s + follow-up to %s (caller %s)\n", label, short(resp.ConvID), short(resp.CallerConv))
	} else {
		fmt.Fprintf(stdout, "Submitted /%s to %s (caller %s)\n", label, short(resp.ConvID), short(resp.CallerConv))
	}
	if resp.Note != "" {
		fmt.Fprintf(stdout, "Note: %s\n", resp.Note)
	}
	return rcOK
}

func runSelfSlash(followUp, askHuman, path, label string, stdout, stderr io.Writer) int {
	followUp = strings.TrimSpace(followUp)
	if followUp != "" && !isValidFollowUp(followUp) {
		fmt.Fprintln(stderr, "Error: REJECTED. Follow-up must be 1-4096 printable characters; control")
		fmt.Fprintln(stderr, "characters (newlines, tabs, etc.) are not allowed because each newline")
		fmt.Fprintln(stderr, "would be treated as a separate prompt-submit by tmux send-keys.")
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	ask, err := ParseAskHuman(askHuman)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcInvalidArg
	}
	if ask > 0 {
		fmt.Fprintf(stdout, "Waiting up to %s for human approval...\n", ask)
	}
	body := map[string]string{}
	if followUp != "" {
		body["follow_up"] = followUp
	}
	var resp struct {
		ConvID   string `json:"conv_id"`
		Action   string `json:"action"`
		FollowUp string `json:"follow_up,omitempty"`
		Note     string `json:"note,omitempty"`
	}
	if err := DaemonRequest(http.MethodPost, path, body, &resp, DaemonOpts{AskHuman: ask}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if resp.FollowUp != "" {
		fmt.Fprintf(stdout, "Submitted /%s + follow-up to %s\n", label, short(resp.ConvID))
	} else {
		fmt.Fprintf(stdout, "Submitted /%s to %s\n", label, short(resp.ConvID))
	}
	if resp.Note != "" {
		fmt.Fprintf(stdout, "Note: %s\n", resp.Note)
	}
	return rcOK
}

// contextInfoResp is the wire shape for a single agent's context read
// (self via /v1/whoami/context, or another agent via
// /v1/agent/{selector}/context). CallerConv is only set on the
// cross-agent path.
type contextInfoResp struct {
	ConvID            string  `json:"conv_id"`
	SessionID         string  `json:"session_id,omitempty"`
	ContextPct        float64 `json:"context_pct"`
	TokensInput       int64   `json:"tokens_input"`
	TokensOutput      int64   `json:"tokens_output"`
	ContextWindowSize int64   `json:"context_window_size"`
	Model             string  `json:"model,omitempty"`
	CallerConv        string  `json:"caller_conv,omitempty"`
}

// groupContextEntry mirrors the daemon's per-member wire shape for
// GET /v1/groups/{name}/context.
type groupContextEntry struct {
	ConvID            string  `json:"conv_id"`
	Title             string  `json:"title"`
	Role              string  `json:"role,omitempty"`
	Online            bool    `json:"online"`
	HasSnapshot       bool    `json:"has_snapshot"`
	ContextPct        float64 `json:"context_pct"`
	TokensInput       int64   `json:"tokens_input"`
	TokensOutput      int64   `json:"tokens_output"`
	ContextWindowSize int64   `json:"context_window_size"`
	Model             string  `json:"model,omitempty"`
}

func runContextInfo(p *contextInfoParams, stdout, stderr io.Writer) int {
	target := strings.TrimSpace(p.Target)
	group := strings.TrimSpace(p.Group)
	if target != "" && group != "" {
		fmt.Fprintln(stderr, "Error: --target and --group are mutually exclusive (one agent vs a whole group).")
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	if group != "" {
		return runGroupContextInfo(p, group, stdout, stderr)
	}

	path := "/v1/whoami/context"
	if target != "" {
		path = "/v1/agent/" + url.PathEscape(target) + "/context"
	}
	var resp contextInfoResp
	if err := DaemonGet(path, &resp); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if p.JSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(resp); err != nil {
			return rcIOFailure
		}
		return rcOK
	}
	fmt.Fprintf(stdout, "conv:    %s\n", short(resp.ConvID))
	if resp.CallerConv != "" {
		fmt.Fprintf(stdout, "caller:  %s\n", short(resp.CallerConv))
	}
	tokensTotal := resp.TokensInput + resp.TokensOutput
	if resp.ContextWindowSize > 0 && tokensTotal > 0 {
		// Authoritative shape: real abs counts + real window size from CC.
		fmt.Fprintf(stdout, "context: %.0f%% (%s in + %s out = %s of %s tokens)\n",
			resp.ContextPct,
			formatTokens(resp.TokensInput),
			formatTokens(resp.TokensOutput),
			formatTokens(tokensTotal),
			formatTokens(resp.ContextWindowSize))
	} else {
		// Statusline hook hasn't fired with the new fields yet (or
		// running pre-v2.1.132 Claude Code). Fall back to percentage-only.
		fmt.Fprintf(stdout, "context: %.0f%% (abs token data not available yet — wait for next CC turn)\n", resp.ContextPct)
	}
	if resp.Model != "" {
		fmt.Fprintf(stdout, "model:   %s\n", resp.Model)
	}
	return rcOK
}

// runGroupContextInfo fetches and renders the context-window state of
// every member of a group — the lead-watching-workers view. Read-only.
func runGroupContextInfo(p *contextInfoParams, group string, stdout, stderr io.Writer) int {
	var entries []groupContextEntry
	if err := DaemonGet("/v1/groups/"+url.PathEscape(group)+"/context", &entries); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if p.JSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(entries); err != nil {
			return rcIOFailure
		}
		return rcOK
	}
	if len(entries) == 0 {
		fmt.Fprintf(stdout, "(group %q has no members)\n", group)
		return rcOK
	}
	tbl := table.New(
		table.Column{Header: "", Width: 1},
		table.Column{Header: "ID", Width: 8},
		table.Column{Header: "NAME", MinWidth: 8, Weight: 0.8, Truncate: true},
		table.Column{Header: "ROLE", MinWidth: 6, Weight: 0.4, Truncate: true},
		table.Column{Header: "CTX%", Width: 5, Align: table.AlignRight},
		table.Column{Header: "TOKENS", MinWidth: 13, Weight: 0.5, Truncate: true},
		table.Column{Header: "MODEL", MinWidth: 6, Weight: 0.5, Truncate: true},
	)
	tbl.SetTerminalWidth(table.GetTerminalWidth())
	for _, e := range entries {
		tbl.AddRow(table.Row{Cells: []string{
			onlineMark(e.Online),
			short(e.ConvID),
			e.Title,
			e.Role,
			formatContextPct(e),
			formatContextTokens(e),
			e.Model,
		}})
	}
	fmt.Fprintln(stdout, tbl.Render())
	return rcOK
}

// formatContextPct renders the CTX% cell. A member whose statusline hook
// has never fired (HasSnapshot false) shows "—" rather than a misleading
// "0%" — the daemon distinguishes "not reported yet" from a genuine 0%.
func formatContextPct(e groupContextEntry) string {
	if !e.HasSnapshot {
		return "—"
	}
	return fmt.Sprintf("%.0f%%", e.ContextPct)
}

// formatContextTokens renders the TOKENS cell as "<used> / <window>"
// (e.g. "120k / 200k"). Empty when the absolute counts aren't available
// yet — the percentage in CTX% still carries the headline figure.
func formatContextTokens(e groupContextEntry) string {
	total := e.TokensInput + e.TokensOutput
	if !e.HasSnapshot || e.ContextWindowSize <= 0 || total <= 0 {
		return ""
	}
	return formatTokens(total) + " / " + formatTokens(e.ContextWindowSize)
}

// formatTokens renders an int64 as a short human-readable token count
// (e.g. 1_300_000 → "1.3M", 130_000 → "130k", 47 → "47"). Keeps the
// terminal output narrow without dropping useful precision.
func formatTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%dk", n/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}
