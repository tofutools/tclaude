package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/common/table"
	"github.com/tofutools/tclaude/pkg/common"
)

// groupsCmd is `tclaude agent groups …`. Mutating subcommands (create,
// rm, add, remove, update-member) are gated by the daemon on a per-action
// permission slug — humans (no CC ancestor) always pass; agents must
// hold the matching slug in `agent.default_permissions` or
// `agent.permission_overrides[<conv>]` in `~/.tclaude/config.json`.
//
// Slugs: groups.create, groups.rm, member.add, member.remove,
// member.redesignate. Read-only subcommands (`ls`, `members`) are open
// to any caller.
func groupsCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:         "groups",
		Short:       "Manage agent groups (allow-listed who can talk to whom)",
		Long:        "`ls` and `members` are open. Mutating subcommands (create, rm, add, remove, update-member) are gated server-side on a permission slug: humans always pass; agents need the slug granted in agent.default_permissions or agent.permission_overrides in ~/.tclaude/config.json. Slugs: groups.create, groups.rm, member.add, member.remove, member.redesignate.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			groupsLsCmd(),
			groupsCreateCmd(),
			groupsRmCmd(),
			groupsMembersCmd(),
			groupsAddCmd(),
			groupsRemoveCmd(),
			groupsUpdateMemberCmd(),
			groupsStopCmd(),
			groupsResumeCmd(),
			groupsOwnersCmd(),
			groupsGrantOwnerCmd(),
			groupsRevokeOwnerCmd(),
		},
	}.ToCobra()
}

// --- groups ls ---

type groupsLsParams struct {
	State string `long:"state" optional:"true" help:"Filter: online (any member online) | offline (no member online)"`
	JSON  bool   `long:"json" help:"Output JSON"`
}

func groupsLsCmd() *cobra.Command {
	return boa.CmdT[groupsLsParams]{
		Use:         "ls",
		Short:       "List all groups",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *groupsLsParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.State).SetAlternativesFunc(completeStateFilterValues)
			return nil
		},
		RunFunc: func(p *groupsLsParams, _ *cobra.Command, _ []string) {
			os.Exit(runGroupsLs(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

type groupSummary struct {
	Name    string `json:"name"`
	Descr   string `json:"descr,omitempty"`
	Members int    `json:"members"`
	Online  int    `json:"online"`
}

func runGroupsLs(p *groupsLsParams, stdout, stderr io.Writer) int {
	wantOnline, applyState, err := parseStateFilter(p.State)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var groups []groupSummary
	if err := DaemonGet("/v1/groups", &groups); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if applyState {
		filtered := make([]groupSummary, 0, len(groups))
		for _, g := range groups {
			if (g.Online > 0) == wantOnline {
				filtered = append(filtered, g)
			}
		}
		groups = filtered
	}
	if p.JSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(groups); err != nil {
			return rcIOFailure
		}
		return rcOK
	}
	if len(groups) == 0 {
		fmt.Fprintln(stdout, "(no groups)")
		return rcOK
	}
	tbl := table.New(
		table.Column{Header: "NAME", MinWidth: 8, Weight: 0.6, Truncate: true},
		table.Column{Header: "MEMBERS", Width: 7, Align: table.AlignRight},
		table.Column{Header: "ONLINE", Width: 6, Align: table.AlignRight},
		table.Column{Header: "DESCR", MinWidth: 10, Weight: 1.4, Truncate: true},
	)
	tbl.SetTerminalWidth(table.GetTerminalWidth())
	for _, g := range groups {
		tbl.AddRow(table.Row{Cells: []string{
			g.Name,
			fmt.Sprintf("%d", g.Members),
			fmt.Sprintf("%d", g.Online),
			g.Descr,
		}})
	}
	fmt.Fprintln(stdout, tbl.Render())
	return rcOK
}

// --- groups create ---

type groupsCreateParams struct {
	Name     string `pos:"true" help:"Group name"`
	Descr    string `long:"descr" short:"d" optional:"true" help:"Optional description"`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s' or '60'). Capped at 300s. Timeout = deny."`
}

func groupsCreateCmd() *cobra.Command {
	return boa.CmdT[groupsCreateParams]{
		Use:         "create",
		Short:       "Create a new group",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *groupsCreateParams, _ *cobra.Command) error {
			// `Name` is brand-new on create; no value-completion to offer.
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *groupsCreateParams, _ *cobra.Command, _ []string) {
			os.Exit(runGroupsCreate(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runGroupsCreate(p *groupsCreateParams, stdout, stderr io.Writer) int {
	if p.Name == "" {
		fmt.Fprintf(stderr, "Error: group name is required\n")
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
	var resp struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	if err := DaemonRequest(http.MethodPost, "/v1/groups", map[string]string{
		"name":  p.Name,
		"descr": p.Descr,
	}, &resp, DaemonOpts{AskHuman: ask}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	fmt.Fprintf(stdout, "Created group %q (id=%d)\n", resp.Name, resp.ID)
	return rcOK
}

// --- groups rm ---

type groupsRmParams struct {
	Name     string `pos:"true" help:"Group name"`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s' or '60'). Capped at 300s. Timeout = deny."`
}

func groupsRmCmd() *cobra.Command {
	return boa.CmdT[groupsRmParams]{
		Use:         "rm",
		Short:       "Delete a group (fails if any messages still reference it)",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *groupsRmParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Name).SetAlternativesFunc(completeGroupNames)
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *groupsRmParams, _ *cobra.Command, _ []string) {
			os.Exit(runGroupsRm(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runGroupsRm(p *groupsRmParams, stdout, stderr io.Writer) int {
	if p.Name == "" {
		fmt.Fprintf(stderr, "Error: group name is required\n")
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
	if err := DaemonRequest(http.MethodDelete, "/v1/groups/"+url.PathEscape(p.Name), nil, nil, DaemonOpts{AskHuman: ask}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	fmt.Fprintf(stdout, "Deleted group %q\n", p.Name)
	return rcOK
}

// --- groups members ---

type groupsMembersParams struct {
	Name string `pos:"true" help:"Group name"`
	JSON bool   `long:"json" help:"Output JSON"`
}

func groupsMembersCmd() *cobra.Command {
	return boa.CmdT[groupsMembersParams]{
		Use:         "members",
		Short:       "List members of a group",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *groupsMembersParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Name).SetAlternativesFunc(completeGroupNames)
			return nil
		},
		RunFunc: func(p *groupsMembersParams, _ *cobra.Command, _ []string) {
			os.Exit(runGroupsMembers(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

type memberEntry struct {
	ConvID string `json:"conv_id"`
	Title  string `json:"title"`
	Alias  string `json:"alias,omitempty"`
	Role   string `json:"role,omitempty"`
	Descr  string `json:"descr,omitempty"`
	Online bool   `json:"online"`
	Owner  bool   `json:"owner,omitempty"`
}

func runGroupsMembers(p *groupsMembersParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var members []memberEntry
	if err := DaemonGet("/v1/groups/"+url.PathEscape(p.Name)+"/members", &members); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if p.JSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(members); err != nil {
			return rcIOFailure
		}
		return rcOK
	}
	if len(members) == 0 {
		fmt.Fprintln(stdout, "(no members)")
		return rcOK
	}
	tbl := table.New(
		table.Column{Header: "", Width: 1},
		table.Column{Header: "ID", Width: 8},
		table.Column{Header: "ALIAS", MinWidth: 8, Weight: 0.8, Truncate: true},
		table.Column{Header: "ROLE", MinWidth: 6, Weight: 0.4, Truncate: true},
		table.Column{Header: "DESCR", MinWidth: 10, Weight: 1.2, Truncate: true},
	)
	tbl.SetTerminalWidth(table.GetTerminalWidth())
	for _, m := range members {
		alias := m.Alias
		if alias == "" {
			alias = m.Title
		}
		// Tag owners inline so the human can see at a glance who's
		// privileged. A pure-owner (not a member) is surfaced by the
		// daemon with role=="owner" already, so we only need to
		// decorate the member case.
		role := m.Role
		if m.Owner && role != "" && role != "owner" {
			role = role + " (owner)"
		} else if m.Owner && role == "" {
			role = "owner"
		}
		tbl.AddRow(table.Row{Cells: []string{
			onlineMark(m.Online),
			short(m.ConvID),
			alias,
			role,
			m.Descr,
		}})
	}
	fmt.Fprintln(stdout, tbl.Render())
	return rcOK
}

// --- groups add ---

type groupsAddParams struct {
	Group    string `pos:"true" help:"Group name"`
	Conv     string `pos:"true" help:"Conversation: UUID, prefix, or current title"`
	Alias    string `long:"alias" short:"a" optional:"true" help:"Alias to use for this conv inside the group"`
	Role     string `long:"role" short:"r" optional:"true" help:"Role label, e.g. 'lead', 'reviewer'"`
	Descr    string `long:"descr" short:"d" optional:"true" help:"Short description of this member"`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s' or '60'). Capped at 300s. Timeout = deny."`
}

func groupsAddCmd() *cobra.Command {
	return boa.CmdT[groupsAddParams]{
		Use:         "add",
		Short:       "Add a conversation to a group",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *groupsAddParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Group).SetAlternativesFunc(completeGroupNames)
			boa.GetParamT(ctx, &p.Conv).SetAlternativesFunc(completeConvSelectors)
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *groupsAddParams, _ *cobra.Command, _ []string) {
			os.Exit(runGroupsAdd(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runGroupsAdd(p *groupsAddParams, stdout, stderr io.Writer) int {
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
	var resp struct {
		ConvID string `json:"conv_id"`
	}
	if err := DaemonRequest(http.MethodPost, "/v1/groups/"+url.PathEscape(p.Group)+"/members", map[string]string{
		"conv":  p.Conv,
		"alias": p.Alias,
		"role":  p.Role,
		"descr": p.Descr,
	}, &resp, DaemonOpts{AskHuman: ask}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	shortID := resp.ConvID
	if len(shortID) >= 8 {
		shortID = shortID[:8]
	}
	fmt.Fprintf(stdout, "Added %s to group %q (alias=%q role=%q)\n", shortID, p.Group, p.Alias, p.Role)
	return rcOK
}

// --- groups remove ---

type groupsRemoveParams struct {
	Group    string `pos:"true" help:"Group name"`
	Conv     string `pos:"true" help:"Conversation: UUID, prefix, or current title"`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s' or '60'). Capped at 300s. Timeout = deny."`
}

func groupsRemoveCmd() *cobra.Command {
	return boa.CmdT[groupsRemoveParams]{
		Use:         "remove",
		Short:       "Remove a conversation from a group",
		Aliases:     []string{"rm-member"},
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *groupsRemoveParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Group).SetAlternativesFunc(completeGroupNames)
			boa.GetParamT(ctx, &p.Conv).SetAlternativesFunc(completeConvSelectors)
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *groupsRemoveParams, _ *cobra.Command, _ []string) {
			os.Exit(runGroupsRemove(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runGroupsRemove(p *groupsRemoveParams, stdout, stderr io.Writer) int {
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
	if err := DaemonRequest(http.MethodDelete, "/v1/groups/"+url.PathEscape(p.Group)+"/members/"+url.PathEscape(p.Conv), nil, nil, DaemonOpts{AskHuman: ask}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	fmt.Fprintf(stdout, "Removed %s from group %q\n", p.Conv, p.Group)
	return rcOK
}

// --- groups stop ---

type groupsStopParams struct {
	Name     string `pos:"true" help:"Group name"`
	Force    bool   `long:"force" help:"Use tmux kill-session instead of soft /exit"`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s' or '60'). Capped at 300s. Timeout = deny."`
}

func groupsStopCmd() *cobra.Command {
	return boa.CmdT[groupsStopParams]{
		Use:         "stop",
		Short:       "End every member's running tmux session in a group",
		Long:        "Soft-stops by default: injects `/exit` into each online member's CC pane via tmux send-keys. With --force, uses `tmux kill-session` (drops any unsubmitted input). Members already offline are skipped — stop is idempotent.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *groupsStopParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Name).SetAlternativesFunc(completeGroupNames)
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *groupsStopParams, _ *cobra.Command, _ []string) {
			os.Exit(runGroupsStop(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runGroupsStop(p *groupsStopParams, stdout, stderr io.Writer) int {
	if p.Name == "" {
		fmt.Fprintf(stderr, "Error: group name is required\n")
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
	path := "/v1/groups/" + url.PathEscape(p.Name) + "/stop"
	if p.Force {
		path += "?force=1"
	}
	return runGroupsLifecycle(path, ask, stdout, stderr)
}

// --- groups resume ---

type groupsResumeParams struct {
	Name     string `pos:"true" help:"Group name"`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s' or '60'). Capped at 300s. Timeout = deny."`
}

func groupsResumeCmd() *cobra.Command {
	return boa.CmdT[groupsResumeParams]{
		Use:         "resume",
		Short:       "Start a tclaude session for every offline member of a group",
		Long:        "For each member with a known conv-id and no live tmux session, spawns `tclaude session new -r <conv> -d --global`. Members already online are skipped — resume is idempotent. Useful as a 'wake the team' reconciliation.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *groupsResumeParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Name).SetAlternativesFunc(completeGroupNames)
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *groupsResumeParams, _ *cobra.Command, _ []string) {
			os.Exit(runGroupsResume(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runGroupsResume(p *groupsResumeParams, stdout, stderr io.Writer) int {
	if p.Name == "" {
		fmt.Fprintf(stderr, "Error: group name is required\n")
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
	path := "/v1/groups/" + url.PathEscape(p.Name) + "/resume"
	return runGroupsLifecycle(path, ask, stdout, stderr)
}

// runGroupsLifecycle is shared between stop/resume — both endpoints
// return the same per-member result shape, only the action label
// changes.
func runGroupsLifecycle(path string, ask time.Duration, stdout, stderr io.Writer) int {
	var resp struct {
		Group   string `json:"group"`
		Action  string `json:"action"`
		Members []struct {
			ConvID  string `json:"conv_id"`
			Alias   string `json:"alias,omitempty"`
			Action  string `json:"action"`
			Detail  string `json:"detail,omitempty"`
			TmuxSes string `json:"tmux_session,omitempty"`
		} `json:"members"`
	}
	opts := DaemonOpts{AskHuman: ask}
	if err := DaemonRequest(http.MethodPost, path, nil, &resp, opts); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if len(resp.Members) == 0 {
		fmt.Fprintf(stdout, "Group %q has no members.\n", resp.Group)
		return rcOK
	}
	tbl := table.New(
		table.Column{Header: "ID", Width: 8},
		table.Column{Header: "ALIAS", MinWidth: 8, Weight: 0.6, Truncate: true},
		table.Column{Header: "ACTION", MinWidth: 10, Weight: 0.6, Truncate: true},
		table.Column{Header: "DETAIL", MinWidth: 10, Weight: 1.4, Truncate: true},
	)
	tbl.SetTerminalWidth(table.GetTerminalWidth())
	for _, m := range resp.Members {
		alias := m.Alias
		if alias == "" {
			alias = "(unnamed)"
		}
		tbl.AddRow(table.Row{Cells: []string{
			short(m.ConvID), alias, m.Action, m.Detail,
		}})
	}
	fmt.Fprintf(stdout, "Group %q — %s:\n", resp.Group, resp.Action)
	fmt.Fprintln(stdout, tbl.Render())
	return rcOK
}

// --- groups update-member ---

type groupsUpdateMemberParams struct {
	Group    string `pos:"true" help:"Group name"`
	Conv     string `pos:"true" help:"Conversation: UUID, prefix, or current title"`
	Alias    string `long:"alias" short:"a" optional:"true" help:"New alias (pass empty string to clear)"`
	Role     string `long:"role" short:"r" optional:"true" help:"New role label (pass empty string to clear)"`
	Descr    string `long:"descr" short:"d" optional:"true" help:"New description (pass empty string to clear)"`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s' or '60'). Capped at 300s. Timeout = deny."`
}

func groupsUpdateMemberCmd() *cobra.Command {
	return boa.CmdT[groupsUpdateMemberParams]{
		Use:         "update-member",
		Short:       "Edit alias/role/descr on an existing group member",
		Long:        "Patch the alias, role, or descr of a member already in a group. Only the flags you pass are touched; pass an empty string (e.g. --alias='') to clear a field. Same human-only gate as `add`/`remove`.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *groupsUpdateMemberParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Group).SetAlternativesFunc(completeGroupNames)
			boa.GetParamT(ctx, &p.Conv).SetAlternativesFunc(completeConvSelectors)
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *groupsUpdateMemberParams, cmd *cobra.Command, _ []string) {
			os.Exit(runGroupsUpdateMember(p, cmd, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runGroupsUpdateMember(p *groupsUpdateMemberParams, cmd *cobra.Command, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	ask, err := ParseAskHuman(p.AskHuman)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcInvalidArg
	}
	body := map[string]any{}
	if cmd.Flags().Changed("alias") {
		body["alias"] = p.Alias
	}
	if cmd.Flags().Changed("role") {
		body["role"] = p.Role
	}
	if cmd.Flags().Changed("descr") {
		body["descr"] = p.Descr
	}
	if len(body) == 0 {
		fmt.Fprintf(stderr, "Error: at least one of --alias / --role / --descr is required\n")
		return rcInvalidArg
	}
	if ask > 0 {
		fmt.Fprintf(stdout, "Waiting up to %s for human approval...\n", ask)
	}
	var resp struct {
		ConvID string `json:"conv_id"`
	}
	path := "/v1/groups/" + url.PathEscape(p.Group) + "/members/" + url.PathEscape(p.Conv)
	if err := DaemonRequest(http.MethodPatch, path, body, &resp, DaemonOpts{AskHuman: ask}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	fmt.Fprintf(stdout, "Updated %s in group %q\n", short(resp.ConvID), p.Group)
	return rcOK
}
