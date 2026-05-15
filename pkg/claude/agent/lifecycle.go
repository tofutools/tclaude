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
	Target   string `long:"target" optional:"true" help:"Compact ANOTHER agent's pane instead of self. Selector: alias, full conv-id, or 8+-char prefix. Requires the agent.compact permission, or being an owner of a group containing the target."`
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
	JSON bool `long:"json" help:"Output JSON"`
}

func contextInfoCmd() *cobra.Command {
	return boa.CmdT[contextInfoParams]{
		Use:         "context-info",
		Short:       "Show this conversation's context-window state",
		Long:        "Reads the latest context_pct (populated by tclaude's statusline hook) and any pending /compact claim. Read-only and self-targeted; no permission slug required.",
		ParamEnrich: common.DefaultParamEnricher(),
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

// isValidInitialMessage mirrors the daemon-side isValidInitialMessage
// check (agentd/handlers.go). Unlike a follow-up — which is typed into
// a tmux pane — a spawn's initial message is delivered to the new
// agent's inbox, so newlines and tabs are allowed; only NUL / escape /
// other non-text control characters are rejected. An empty string is
// valid (no initial message). Length is capped at 4096 bytes.
func isValidInitialMessage(s string) bool {
	if len(s) > 4096 {
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

func runContextInfo(p *contextInfoParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var resp struct {
		ConvID            string  `json:"conv_id"`
		SessionID         string  `json:"session_id,omitempty"`
		ContextPct        float64 `json:"context_pct"`
		TokensInput       int64   `json:"tokens_input"`
		TokensOutput      int64   `json:"tokens_output"`
		ContextWindowSize int64   `json:"context_window_size"`
		CompactPending    float64 `json:"compact_pending"`
	}
	if err := DaemonGet("/v1/whoami/context", &resp); err != nil {
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
	if resp.CompactPending > 0 {
		fmt.Fprintf(stdout, "compact: pending (claimed at unix %.0f)\n", resp.CompactPending)
	} else {
		fmt.Fprintln(stdout, "compact: idle")
	}
	return rcOK
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
