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

// `tclaude agent promote / retire / reinstate <selector>` — the
// explicit verbs that move a conversation across the agent boundary.
//
//   - promote   — turn a plain conversation into an agent so it shows
//                 on the roster (`agent ls`, the dashboard Agents tab).
//   - retire    — demote an agent back to a plain conversation: drops
//                 its group memberships and permission/sudo grants,
//                 but leaves the conversation itself intact and
//                 reinstatable. The non-destructive alternative to
//                 `agent delete`.
//   - reinstate — return a retired agent to active status.
//
// Auth: promote/reinstate need the agent.promote slug, retire needs
// agent.retire — OR being an owner of a group containing the target.
// Humans always pass. Same shape as the other cross-agent verbs.

type promoteParams struct {
	Selector string `pos:"true" help:"Target conv: title, full conv-id, or 8+-char prefix"`
}

func promoteCmd() *cobra.Command {
	return boa.CmdT[promoteParams]{
		Use:   "promote",
		Short: "Promote a plain conversation into an agent",
		Long: "Enrolls the target conversation as an agent so it appears on " +
			"the roster (`tclaude agent ls`, the dashboard Agents tab) and " +
			"can be messaged, grouped and granted permissions. " +
			"\n\n" +
			"If the target is a retired agent, promote reinstates it. " +
			"\n\n" +
			"Auth: requires the agent.promote permission OR being an owner " +
			"of a group containing the target.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *promoteParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Selector).SetAlternativesFunc(completeConvSelectors)
			return nil
		},
		RunFunc: func(p *promoteParams, _ *cobra.Command, _ []string) {
			os.Exit(runPromote(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runPromote(p *promoteParams, stdout, stderr io.Writer) int {
	selector := strings.TrimSpace(p.Selector)
	if selector == "" {
		fmt.Fprintln(stderr, "Error: a target selector is required")
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	path := "/v1/agent/" + url.PathEscape(selector) + "/promote"
	var resp struct {
		ConvID     string `json:"conv_id"`
		PriorState string `json:"prior_state"`
		State      string `json:"state"`
	}
	if err := DaemonRequest(http.MethodPost, path, nil, &resp, DaemonOpts{}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	switch resp.PriorState {
	case "active":
		fmt.Fprintf(stdout, "%s: already an active agent — no change\n", short(resp.ConvID))
	case "retired":
		fmt.Fprintf(stdout, "%s: reinstated (was retired) → active agent\n", short(resp.ConvID))
	default:
		fmt.Fprintf(stdout, "%s: promoted → active agent\n", short(resp.ConvID))
	}
	return rcOK
}

type retireParams struct {
	Selector         string `pos:"true" help:"Target conv: title, full conv-id, or 8+-char prefix"`
	Reason           string `long:"reason" short:"r" optional:"true" help:"Why the agent is being retired (recorded in the audit trail)"`
	NoShutdown       bool   `long:"no-shutdown" help:"Leave the agent's running session alive. By default retire also soft-exits the running tmux session (sends /exit); pass this to keep the process running."`
	NoDeleteWorktree bool   `long:"no-delete-worktree" help:"Keep the agent's git worktree + branch. By default retire also removes a removable linked worktree (never the main repo or one shared with a live agent) and force-deletes its branch; pass this to leave the worktree untouched."`
}

func retireCmd() *cobra.Command {
	return boa.CmdT[retireParams]{
		Use:   "retire",
		Short: "Retire an agent — demote it to a plain conversation",
		Long: "Soft-deletes the target agent: drops every group membership and " +
			"revokes every permission and sudo grant, so it stops being an " +
			"agent — but the conversation itself (.jsonl, history) is left " +
			"completely intact and the agent can be reinstated later. " +
			"\n\n" +
			"By default retire also shuts the agent's running session down — " +
			"it soft-exits the tmux pane (sends /exit), since a retired " +
			"agent's idle process is almost never wanted. The conversation " +
			"is untouched and still reinstatable; only the live process " +
			"ends. Pass --no-shutdown to leave the session running. A " +
			"retired agent with no live session is a no-op either way. " +
			"\n\n" +
			"By default retire ALSO cleans up the agent's git worktree: it " +
			"removes the linked worktree and force-deletes its branch, so a " +
			"retired feature agent leaves no git footprint behind. This is " +
			"safe — the repo's main worktree, and any worktree a surviving " +
			"agent is still working in, are always kept. Because the agent's " +
			"cwd IS the worktree, removal waits until its pane exits; with " +
			"--no-shutdown a still-running agent keeps its worktree. Pass " +
			"--no-delete-worktree to leave the worktree and branch untouched. " +
			"\n\n" +
			"This is the non-destructive alternative to `tclaude agent " +
			"delete`, which permanently wipes the conversation. " +
			"\n\n" +
			"Auth: requires the agent.retire permission OR being an owner " +
			"of a group containing the target.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *retireParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Selector).SetAlternativesFunc(completeConvSelectors)
			return nil
		},
		RunFunc: func(p *retireParams, _ *cobra.Command, _ []string) {
			os.Exit(runRetire(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runRetire(p *retireParams, stdout, stderr io.Writer) int {
	selector := strings.TrimSpace(p.Selector)
	if selector == "" {
		fmt.Fprintln(stderr, "Error: a target selector is required")
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	// Always send shutdown and delete_worktree explicitly so the request
	// is unambiguous — the daemon defaults shutdown ON and delete_worktree
	// OFF for absent params, but spelling both out keeps the CLI behaviour
	// independent of those server-side defaults. The CLI's own defaults
	// (set by the --no-* flags) do both, matching the dashboard modal.
	q := url.Values{}
	if reason := strings.TrimSpace(p.Reason); reason != "" {
		q.Set("reason", reason)
	}
	if p.NoShutdown {
		q.Set("shutdown", "0")
	} else {
		q.Set("shutdown", "1")
	}
	if p.NoDeleteWorktree {
		q.Set("delete_worktree", "0")
	} else {
		q.Set("delete_worktree", "1")
	}
	path := "/v1/agent/" + url.PathEscape(selector) + "/retire?" + q.Encode()
	var resp struct {
		ConvID  string `json:"conv_id"`
		Outcome struct {
			GroupsLeft   []string `json:"groups_left"`
			PermsRevoked int64    `json:"perms_revoked"`
			SudoRevoked  int64    `json:"sudo_revoked"`
			Retired      bool     `json:"retired"`
		} `json:"outcome"`
		// Shutdown is present only when shutdown was requested. Action
		// mirrors stopOneConv: soft_stopped | skipped:already_offline |
		// error.
		Shutdown *struct {
			Action string `json:"action"`
			Detail string `json:"detail"`
		} `json:"shutdown"`
		// Worktree is present only when delete_worktree was requested.
		// Action is one of: none | kept | removed | scheduled (see the
		// daemon's retireWorktreePlan); Detail is already human-readable.
		Worktree *struct {
			Action string `json:"action"`
			Detail string `json:"detail"`
		} `json:"worktree"`
	}
	if err := DaemonRequest(http.MethodPost, path, nil, &resp, DaemonOpts{}); err != nil {
		// A dangling agent entry — an enrollment whose conversation data
		// is gone — can't be retired (there's no conversation to demote).
		// Point the human at the only meaningful cleanup instead of the
		// raw resolver error.
		if de, ok := err.(*DaemonError); ok && de.IsDangling() {
			fmt.Fprintf(stderr, "Error: %s has no conversation data — it's a dangling agent entry.\n", short(selector))
			fmt.Fprintln(stderr, "Retire can't demote a missing conversation. To remove the dangling entry, run:")
			fmt.Fprintf(stderr, "  tclaude agent delete %s\n", selector)
			return rcNotFound
		}
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	fmt.Fprintf(stdout, "%s: retired → plain conversation (data kept, reinstatable)\n", short(resp.ConvID))
	if len(resp.Outcome.GroupsLeft) > 0 {
		fmt.Fprintf(stdout, "  left groups: %s\n", strings.Join(resp.Outcome.GroupsLeft, ", "))
	}
	if resp.Outcome.PermsRevoked > 0 || resp.Outcome.SudoRevoked > 0 {
		fmt.Fprintf(stdout, "  revoked: %d permission grant(s), %d sudo grant(s)\n",
			resp.Outcome.PermsRevoked, resp.Outcome.SudoRevoked)
	}
	if resp.Shutdown != nil {
		switch {
		case resp.Shutdown.Action == "soft_stopped":
			fmt.Fprintln(stdout, "  session: sent /exit — the running session is shutting down")
		case strings.HasPrefix(resp.Shutdown.Action, "skipped"):
			fmt.Fprintln(stdout, "  session: no running session to stop")
		case resp.Shutdown.Action == "error":
			fmt.Fprintf(stdout, "  session: shutdown failed: %s\n", resp.Shutdown.Detail)
		}
	}
	// The detail strings are already human-readable (e.g. "worktree +
	// branch will be removed after the agent exits", "worktree kept (main
	// repo)"). Action "none" means the agent had no worktree — stay silent
	// rather than printing a "no worktree" line on every default retire.
	if resp.Worktree != nil && resp.Worktree.Action != "none" && resp.Worktree.Detail != "" {
		fmt.Fprintf(stdout, "  worktree: %s\n", resp.Worktree.Detail)
	}
	return rcOK
}

type reinstateParams struct {
	Selector string `pos:"true" help:"Target conv: title, full conv-id, or 8+-char prefix"`
}

func reinstateCmd() *cobra.Command {
	return boa.CmdT[reinstateParams]{
		Use:   "reinstate",
		Short: "Reinstate a retired agent back to active status",
		Long: "Returns a retired agent to active status so it shows on the " +
			"roster again. Its old group memberships and grants do NOT come " +
			"back — retire stripped those — so the reinstated agent starts " +
			"fresh. " +
			"\n\n" +
			"Auth: requires the agent.promote permission OR being an owner " +
			"of a group containing the target.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *reinstateParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Selector).SetAlternativesFunc(completeConvSelectors)
			return nil
		},
		RunFunc: func(p *reinstateParams, _ *cobra.Command, _ []string) {
			os.Exit(runReinstate(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runReinstate(p *reinstateParams, stdout, stderr io.Writer) int {
	selector := strings.TrimSpace(p.Selector)
	if selector == "" {
		fmt.Fprintln(stderr, "Error: a target selector is required")
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	path := "/v1/agent/" + url.PathEscape(selector) + "/reinstate"
	var resp struct {
		ConvID string `json:"conv_id"`
		State  string `json:"state"`
	}
	if err := DaemonRequest(http.MethodPost, path, nil, &resp, DaemonOpts{}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	fmt.Fprintf(stdout, "%s: reinstated → active agent\n", short(resp.ConvID))
	return rcOK
}
