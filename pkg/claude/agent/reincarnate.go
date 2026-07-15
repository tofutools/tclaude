package agent

import (
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

// `tclaude agent reincarnate [follow-up]` — replace the calling agent
// with a fresh harness instance that inherits its identity (groups,
// per-conv permission grants, group ownerships) and, optionally, picks
// up a new task via a queued message or direct prompt injection.
//
// The daemon does the heavy lifting; this CLI is a thin wrapper over
// /v1/whoami/reincarnate. See reincarnate.go in the agentd package
// for the orchestration design.

type reincarnateParams struct {
	FollowUp string `pos:"true" optional:"true" help:"First-turn prompt for the new agent (REQUIRED — give it inline here or via --file). Quote multi-word strings. If you have no concrete next directive, summarise your previous 'life' (what you were doing, where the relevant files are) so the successor has something to start from."`
	File     string `long:"file" short:"f" optional:"true" help:"Read the follow-up from this file instead of the positional argument ('-' reads stdin). Sidesteps shell quoting — best for long, multi-line, or backtick-containing follow-ups. Mutually exclusive with the positional argument."`
	Target   string `long:"target" optional:"true" help:"Reincarnate ANOTHER agent instead of self. Selector: title, full conv-id, or 8+-char prefix. Requires the agent.reincarnate permission, or being an owner of a group containing the target."`
	AskHuman string `long:"ask-human" optional:"true" help:"Deprecated compatibility no-op for self-reincarnation, which needs no permission; cross-agent calls still require an explicit grant or group ownership."`
}

func reincarnateCmd() *cobra.Command {
	return boa.CmdT[reincarnateParams]{
		Use:   "reincarnate",
		Short: "Replace this agent (or another, with --target) with a fresh successor that inherits its identity",
		Long: "Spawns a fresh harness instance and migrates the target agent's identity " +
			"(group memberships, per-conv permission grants, group ownerships) onto " +
			"the new conv-id. The old conversation is then soft-stopped. The new " +
			"agent comes up with a clean context window but the same identity. " +
			"\n\n" +
			"Reincarnation is primarily a Claude Code context-management tool: its " +
			"compaction is comparatively slow and lossy. Codex CLI has effective, " +
			"efficient automatic compaction; normally let a Codex agent run to full " +
			"context and auto-compact. Do not reincarnate a Codex agent merely to " +
			"free context space. An explicit human request or another deliberate reason " +
			"to replace the agent can still justify reincarnating either harness. " +
			"\n\n" +
			"By default the target is the calling agent itself (self-reincarnate). " +
			"Use --target <selector> to reincarnate ANOTHER agent — the manager " +
			"pattern. Cross-agent calls require the agent.reincarnate permission, " +
			"OR the caller being an owner of a group containing the target. " +
			"\n\n" +
			"Persist any task state to disk *before* calling — the daemon migrates " +
			"identity, not work. The skill (agent-lifecycle) explains the disk-handoff " +
			"convention. " +
			"\n\n" +
			"A follow-up prompt is REQUIRED — the new agent comes up with a clean " +
			"context window and would otherwise sit idle. If you have no concrete " +
			"next directive, summarise your previous 'life' (what you were doing, " +
			"where the relevant files are, what's next) so the successor has " +
			"something to start from. For a long or multi-line follow-up, prefer " +
			"--file <path> (or --file - to read stdin) — it reads the follow-up from " +
			"a file and so sidesteps shell quoting, including backticks the shell " +
			"would otherwise eat from an inline string. The follow-up is delivered " +
			"via the existing message-flush nudge pipeline (when the agent is in at " +
			"least one group) or, for solo agents, by direct keystroke injection " +
			"into the freshly-spawned pane. For cross-agent reincarnations the " +
			"FromConv on the handoff message is the caller, so the new agent sees " +
			"who asked it to pick up the work.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *reincarnateParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Target).SetAlternativesFunc(completeConvSelectors)
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *reincarnateParams, _ *cobra.Command, _ []string) {
			os.Exit(runReincarnate(p, os.Stdin, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runReincarnate(p *reincarnateParams, stdin io.Reader, stdout, stderr io.Writer) int {
	rawFollowUp, rc := resolveBodyInput(p.FollowUp, p.File, "the follow-up argument", stdin, stderr)
	if rc != rcOK {
		return rc
	}
	followUp := strings.TrimSpace(rawFollowUp)
	target := strings.TrimSpace(p.Target)
	askHuman := p.AskHuman
	if followUp == "" {
		fmt.Fprintln(stderr, "Error: a follow-up prompt is required. The new agent comes up with a clean")
		fmt.Fprintln(stderr, "context window and would otherwise sit idle. If you have no concrete next")
		fmt.Fprintln(stderr, "directive, summarise your previous 'life' (what you were doing, where the")
		fmt.Fprintln(stderr, "relevant files are, what's next) so the successor has something to start from.")
		return rcInvalidArg
	}
	// Validate against the lenient inbox rule — a grouped successor
	// receives the handoff in its inbox, like a spawn brief. The client
	// can't know group membership, so the strict solo-pane limit is the
	// daemon's call; this is just a fast local error for the always-bad
	// cases (oversize / NUL / escape).
	if !isValidInitialMessage(followUp) {
		fmt.Fprintf(stderr, "Error: REJECTED. Follow-up must be at most %d characters; newlines and\n", MaxInitialMessageBytes)
		fmt.Fprintln(stderr, "tabs are allowed (a grouped agent's handoff rides its inbox, like a spawn")
		fmt.Fprintln(stderr, "brief), but NUL / escape / other control characters are not. A solo agent")
		fmt.Fprintln(stderr, "(in no group) keeps a stricter 4096-char, single-line limit — the daemon")
		fmt.Fprintln(stderr, "enforces that once it knows the agent's group membership.")
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	_, err := ParseAskHuman(askHuman)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcInvalidArg
	}
	if askHuman != "" && target != "" {
		// Cross-agent path doesn't honour X-Tclaude-Ask-Human (see
		// requireCrossAgentPermission). Surface that here so the caller
		// doesn't think they have an escape hatch.
		fmt.Fprintln(stderr, "Error: --ask-human is only supported when reincarnating self; cross-agent calls require an explicit slug grant or group ownership.")
		return rcInvalidArg
	}
	body := map[string]any{"follow_up": followUp}
	path := "/v1/whoami/reincarnate"
	if target != "" {
		path = "/v1/agent/" + url.PathEscape(target) + "/reincarnate"
	}
	var resp struct {
		OldConv       string   `json:"old_conv"`
		NewConv       string   `json:"new_conv"`
		CallerConv    string   `json:"caller_conv,omitempty"`
		CallerAgentID string   `json:"caller_agent_id,omitempty"`
		Label         string   `json:"label"`
		TmuxSession   string   `json:"tmux_session"`
		AttachCmd     string   `json:"attach_cmd"`
		Migrated      []string `json:"migrated"`
		FollowUp      string   `json:"follow_up,omitempty"`
		MessageID     int64    `json:"message_id,omitempty"`
		Note          string   `json:"note,omitempty"`
	}
	if err := DaemonRequest(http.MethodPost, path, body, &resp, DaemonOpts{}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if resp.CallerConv != "" {
		fmt.Fprintf(stdout, "Reincarnated %s -> %s (called by %s)\n",
			short(resp.OldConv), short(resp.NewConv), shortAgentID(resp.CallerAgentID, resp.CallerConv))
	} else {
		fmt.Fprintf(stdout, "Reincarnated %s -> %s\n", short(resp.OldConv), short(resp.NewConv))
	}
	if resp.AttachCmd != "" {
		fmt.Fprintf(stdout, "  attach: %s\n", resp.AttachCmd)
	}
	if len(resp.Migrated) > 0 {
		fmt.Fprintf(stdout, "  migrated: %s\n", strings.Join(resp.Migrated, ", "))
	}
	if resp.FollowUp != "" {
		if resp.MessageID > 0 {
			fmt.Fprintf(stdout, "  follow-up queued as message #%d\n", resp.MessageID)
		} else {
			fmt.Fprintln(stdout, "  follow-up injected into new pane")
		}
	}
	if resp.Note != "" {
		fmt.Fprintf(stdout, "Note: %s\n", resp.Note)
	}
	return rcOK
}
