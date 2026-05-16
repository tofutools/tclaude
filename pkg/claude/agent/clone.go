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

// `tclaude agent clone [follow-up] [--no-copy-conv] [--target <peer>]`
// — fork an agent into a sibling that inherits its identity. The
// original keeps running; the clone spawns alongside it.
//
// By default the original's conv jsonl is copied so the clone starts
// with the same context. `--no-copy-conv` skips the copy and gives
// the clone a fresh context window with identity only.
//
// Like the other lifecycle verbs, `--target <selector>` swaps the
// action onto a peer (manager pattern).

type cloneParams struct {
	FollowUp   string `pos:"true" optional:"true" help:"Optional first-turn prompt for the clone (quote multi-word strings)."`
	File       string `long:"file" short:"f" optional:"true" help:"Read the follow-up from this file instead of the positional argument ('-' reads stdin). Sidesteps shell quoting — best for long, multi-line, or backtick-containing follow-ups. Mutually exclusive with the positional argument."`
	Target     string `long:"target" optional:"true" help:"Clone ANOTHER agent instead of self. Selector: title, full conv-id, or 8+-char prefix. Requires the agent.clone permission, or being an owner of a group containing the target."`
	NoCopyConv bool   `long:"no-copy-conv" help:"Spawn the clone with a fresh context (default: copy the original's jsonl so the clone starts with the same conversation history)."`
	AskHuman   string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s'). Capped at 300s. Timeout = deny. Self-target only."`
}

func cloneCmd() *cobra.Command {
	return boa.CmdT[cloneParams]{
		Use:   "clone",
		Short: "Fork this agent (or another, with --target) into a sibling that inherits its identity",
		Long: "Spawns a fresh CC instance that inherits the target agent's identity " +
			"(group memberships, per-conv permission grants, group ownerships). The " +
			"clone is renamed to `<original-title>-c-<N>`. The ORIGINAL keeps running " +
			"— that's the difference vs reincarnate. " +
			"\n\n" +
			"By default the original's conv jsonl is copied onto the clone's fresh " +
			"conv-id, so the clone starts with the same conversation history. Use " +
			"--no-copy-conv to skip the copy and give the clone a blank context. " +
			"\n\n" +
			"For a long or multi-line follow-up, prefer --file <path> (or --file - " +
			"to read stdin) — it reads the follow-up from a file and so sidesteps " +
			"shell quoting, including backticks the shell would otherwise eat from " +
			"an inline string. " +
			"\n\n" +
			"Cross-agent: --target <selector> clones ANOTHER agent (requires the " +
			"agent.clone permission, or being an owner of a group containing the target). " +
			"Otherwise self-targeted, requires the self.clone permission " +
			"(default-granted alongside self.compact and self.reincarnate).",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *cloneParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Target).SetAlternativesFunc(completeConvSelectors)
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *cloneParams, _ *cobra.Command, _ []string) {
			os.Exit(runClone(p, os.Stdin, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runClone(p *cloneParams, stdin io.Reader, stdout, stderr io.Writer) int {
	rawFollowUp, rc := resolveBodyInput(p.FollowUp, p.File, "the follow-up argument", stdin, stderr)
	if rc != rcOK {
		return rc
	}
	followUp := strings.TrimSpace(rawFollowUp)
	target := strings.TrimSpace(p.Target)
	// Validate against the lenient inbox rule — a grouped clone receives
	// the handoff in its inbox, like a spawn brief. The client can't
	// know group membership, so the strict solo-pane limit is the
	// daemon's call; this is just a fast local error for the always-bad
	// cases (oversize / NUL / escape).
	if followUp != "" && !isValidInitialMessage(followUp) {
		fmt.Fprintf(stderr, "Error: REJECTED. Follow-up must be at most %d characters; newlines and\n", MaxInitialMessageBytes)
		fmt.Fprintln(stderr, "tabs are allowed (a grouped agent's handoff rides its inbox, like a spawn")
		fmt.Fprintln(stderr, "brief), but NUL / escape / other control characters are not. A solo agent")
		fmt.Fprintln(stderr, "(in no group) keeps a stricter 4096-char, single-line limit — the daemon")
		fmt.Fprintln(stderr, "enforces that once it knows the agent's group membership.")
		return rcInvalidArg
	}
	if target != "" && p.AskHuman != "" {
		fmt.Fprintln(stderr, "Error: --ask-human is only supported when targeting self; cross-agent calls require an explicit slug grant or group ownership.")
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	ask, err := ParseAskHuman(p.AskHuman)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcInvalidArg
	}
	if ask > 0 {
		fmt.Fprintf(stdout, "Waiting up to %s for human approval...\n", ask)
	}
	body := map[string]any{}
	if followUp != "" {
		body["follow_up"] = followUp
	}
	if p.NoCopyConv {
		body["no_copy_conv"] = true
	}
	path := "/v1/whoami/clone"
	if target != "" {
		path = "/v1/agent/" + url.PathEscape(target) + "/clone"
	}
	var resp struct {
		OldConv     string   `json:"old_conv"`
		NewConv     string   `json:"new_conv"`
		CallerConv  string   `json:"caller_conv,omitempty"`
		Label       string   `json:"label"`
		TmuxSession string   `json:"tmux_session"`
		AttachCmd   string   `json:"attach_cmd"`
		Copied      []string `json:"copied"`
		CopyConv    bool     `json:"copy_conv"`
		FollowUp    string   `json:"follow_up,omitempty"`
		MessageID   int64    `json:"message_id,omitempty"`
		Note        string   `json:"note,omitempty"`
	}
	if err := DaemonRequest(http.MethodPost, path, body, &resp, DaemonOpts{AskHuman: ask}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if resp.CallerConv != "" {
		fmt.Fprintf(stdout, "Cloned %s -> %s (called by %s)\n",
			short(resp.OldConv), short(resp.NewConv), short(resp.CallerConv))
	} else {
		fmt.Fprintf(stdout, "Cloned %s -> %s\n", short(resp.OldConv), short(resp.NewConv))
	}
	if !resp.CopyConv {
		fmt.Fprintln(stdout, "  (clone has fresh context — --no-copy-conv was set)")
	}
	if resp.AttachCmd != "" {
		fmt.Fprintf(stdout, "  attach: %s\n", resp.AttachCmd)
	}
	if len(resp.Copied) > 0 {
		fmt.Fprintf(stdout, "  copied identity: %s\n", strings.Join(resp.Copied, ", "))
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
