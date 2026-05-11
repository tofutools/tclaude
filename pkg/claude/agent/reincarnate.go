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
// with a fresh CC instance that inherits its identity (groups,
// per-conv permission grants, group ownerships) and, optionally, picks
// up a new task via a queued message or direct prompt injection.
//
// The daemon does the heavy lifting; this CLI is a thin wrapper over
// /v1/whoami/reincarnate. See reincarnate.go in the agentd package
// (and agents_todo.md → "Agent reincarnate") for the orchestration
// design.

type reincarnateParams struct {
	FollowUp string `pos:"true" help:"First-turn prompt for the new agent (REQUIRED). Quote multi-word strings. If you have no concrete next directive, summarise your previous 'life' (what you were doing, where the relevant files are) so the successor has something to start from."`
	Target   string `long:"target" optional:"true" help:"Reincarnate ANOTHER agent instead of self. Selector: alias, full conv-id, or 8+-char prefix. Requires the agent.reincarnate permission, or being an owner of a group containing the target."`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s'). Capped at 300s. Timeout = deny. Self-target only."`
}

func reincarnateCmd() *cobra.Command {
	return boa.CmdT[reincarnateParams]{
		Use:   "reincarnate",
		Short: "Replace this agent (or another, with --target) with a fresh successor that inherits its identity",
		Long: "Spawns a fresh CC instance and migrates the target agent's identity " +
			"(group memberships, per-conv permission grants, group ownerships) onto " +
			"the new conv-id. The old conversation is then soft-stopped. The new " +
			"agent comes up with a clean context window but the same identity. " +
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
			"something to start from. The follow-up is delivered via the existing " +
			"message-flush nudge pipeline (when the agent is in at least one group) " +
			"or, for solo agents, by direct keystroke injection into the freshly- " +
			"spawned pane. For cross-agent reincarnations the FromConv on the " +
			"handoff message is the caller, so the new agent sees who asked it to " +
			"pick up the work.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *reincarnateParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Target).SetAlternativesFunc(completeConvSelectors)
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *reincarnateParams, _ *cobra.Command, _ []string) {
			os.Exit(runReincarnate(p.FollowUp, p.Target, p.AskHuman, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runReincarnate(followUp, target, askHuman string, stdout, stderr io.Writer) int {
	followUp = strings.TrimSpace(followUp)
	target = strings.TrimSpace(target)
	if followUp == "" {
		fmt.Fprintln(stderr, "Error: a follow-up prompt is required. The new agent comes up with a clean")
		fmt.Fprintln(stderr, "context window and would otherwise sit idle. If you have no concrete next")
		fmt.Fprintln(stderr, "directive, summarise your previous 'life' (what you were doing, where the")
		fmt.Fprintln(stderr, "relevant files are, what's next) so the successor has something to start from.")
		return rcInvalidArg
	}
	if !isValidFollowUp(followUp) {
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
	if ask > 0 && target != "" {
		// Cross-agent path doesn't honour X-Tclaude-Ask-Human (see
		// requireCrossAgentPermission). Surface that here so the caller
		// doesn't think they have an escape hatch.
		fmt.Fprintln(stderr, "Error: --ask-human is only supported when reincarnating self; cross-agent calls require an explicit slug grant or group ownership.")
		return rcInvalidArg
	}
	if ask > 0 {
		fmt.Fprintf(stdout, "Waiting up to %s for human approval...\n", ask)
	}
	body := map[string]string{"follow_up": followUp}
	path := "/v1/whoami/reincarnate"
	if target != "" {
		path = "/v1/agent/" + url.PathEscape(target) + "/reincarnate"
	}
	var resp struct {
		OldConv        string   `json:"old_conv"`
		NewConv        string   `json:"new_conv"`
		CallerConv     string   `json:"caller_conv,omitempty"`
		Label          string   `json:"label"`
		TmuxSession    string   `json:"tmux_session"`
		AttachCmd      string   `json:"attach_cmd"`
		Migrated       []string `json:"migrated"`
		FollowUp       string   `json:"follow_up,omitempty"`
		MessageID      int64    `json:"message_id,omitempty"`
		Note           string   `json:"note,omitempty"`
	}
	if err := DaemonRequest(http.MethodPost, path, body, &resp, DaemonOpts{AskHuman: ask}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if resp.CallerConv != "" {
		fmt.Fprintf(stdout, "Reincarnated %s -> %s (called by %s)\n",
			short(resp.OldConv), short(resp.NewConv), short(resp.CallerConv))
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
