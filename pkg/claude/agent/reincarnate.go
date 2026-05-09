package agent

import (
	"fmt"
	"io"
	"net/http"
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
	FollowUp string `pos:"true" optional:"true" help:"Optional first-turn prompt for the new agent. Quote multi-word strings."`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s'). Capped at 300s. Timeout = deny."`
}

func reincarnateCmd() *cobra.Command {
	return boa.CmdT[reincarnateParams]{
		Use:   "reincarnate",
		Short: "Replace this agent with a fresh successor that inherits its identity",
		Long: "Spawns a fresh CC instance and migrates the calling agent's identity " +
			"(group memberships, per-conv permission grants, group ownerships) onto " +
			"the new conv-id. The old conversation is then soft-stopped. The new " +
			"agent comes up with a clean context window but the same identity. " +
			"\n\n" +
			"Persist any task state to disk *before* calling — the daemon migrates " +
			"identity, not work. The skill (agent-lifecycle) explains the disk-handoff " +
			"convention. " +
			"\n\n" +
			"An optional follow-up prompt is delivered to the new agent via the " +
			"existing message-flush nudge pipeline (when the agent is in at least " +
			"one group) or, for solo agents, by direct keystroke injection into the " +
			"freshly-spawned pane. " +
			"\n\n" +
			"Requires the `self.reincarnate` permission (default-granted alongside " +
			"self.rename and self.compact).",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *reincarnateParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *reincarnateParams, _ *cobra.Command, _ []string) {
			os.Exit(runReincarnate(p.FollowUp, p.AskHuman, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runReincarnate(followUp, askHuman string, stdout, stderr io.Writer) int {
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
		OldConv     string   `json:"old_conv"`
		NewConv     string   `json:"new_conv"`
		Label       string   `json:"label"`
		TmuxSession string   `json:"tmux_session"`
		AttachCmd   string   `json:"attach_cmd"`
		Migrated    []string `json:"migrated"`
		FollowUp    string   `json:"follow_up,omitempty"`
		MessageID   int64    `json:"message_id,omitempty"`
		Note        string   `json:"note,omitempty"`
	}
	if err := DaemonRequest(http.MethodPost, "/v1/whoami/reincarnate", body, &resp, DaemonOpts{AskHuman: ask}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	fmt.Fprintf(stdout, "Reincarnated %s -> %s\n", short(resp.OldConv), short(resp.NewConv))
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
