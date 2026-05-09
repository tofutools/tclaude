package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/common/table"
	"github.com/tofutools/tclaude/pkg/common"
)

// groupsCmd is `tclaude agent groups …`. The daemon enforces "the human is
// the only mutator" by inspecting peer credentials on each call: callers
// from inside a Claude Code session (with a `claude`/`node` ancestor in
// the pid tree) get 403 on POST/DELETE endpoints; callers without one
// (the human running tclaude from a plain shell) succeed.
func groupsCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:         "groups",
		Short:       "Manage agent groups (allow-listed who can talk to whom)",
		Long:        "Group membership is human-controlled: agents can `ls` and `members`; create/rm/add/remove are gated server-side on the absence of a Claude Code ancestor in the caller's process tree.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			groupsLsCmd(),
			groupsCreateCmd(),
			groupsRmCmd(),
			groupsMembersCmd(),
			groupsAddCmd(),
			groupsRemoveCmd(),
		},
	}.ToCobra()
}

// --- groups ls ---

type groupsLsParams struct {
	JSON bool `long:"json" help:"Output JSON"`
}

func groupsLsCmd() *cobra.Command {
	return boa.CmdT[groupsLsParams]{
		Use:         "ls",
		Short:       "List all groups",
		ParamEnrich: common.DefaultParamEnricher(),
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
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var groups []groupSummary
	if err := DaemonGet("/v1/groups", &groups); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
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
	Name  string `pos:"true" help:"Group name"`
	Descr string `long:"descr" short:"d" optional:"true" help:"Optional description"`
}

func groupsCreateCmd() *cobra.Command {
	return boa.CmdT[groupsCreateParams]{
		Use:         "create",
		Short:       "Create a new group",
		ParamEnrich: common.DefaultParamEnricher(),
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
	var resp struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	if err := DaemonPost("/v1/groups", map[string]string{
		"name":  p.Name,
		"descr": p.Descr,
	}, &resp); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	fmt.Fprintf(stdout, "Created group %q (id=%d)\n", resp.Name, resp.ID)
	return rcOK
}

// --- groups rm ---

type groupsRmParams struct {
	Name string `pos:"true" help:"Group name"`
}

func groupsRmCmd() *cobra.Command {
	return boa.CmdT[groupsRmParams]{
		Use:         "rm",
		Short:       "Delete a group (fails if any messages still reference it)",
		ParamEnrich: common.DefaultParamEnricher(),
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
	if err := DaemonDelete("/v1/groups/"+url.PathEscape(p.Name), nil); err != nil {
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
		tbl.AddRow(table.Row{Cells: []string{
			short(m.ConvID),
			alias,
			m.Role,
			m.Descr,
		}})
	}
	fmt.Fprintln(stdout, tbl.Render())
	return rcOK
}

// --- groups add ---

type groupsAddParams struct {
	Group string `pos:"true" help:"Group name"`
	Conv  string `pos:"true" help:"Conversation: UUID, prefix, or current title"`
	Alias string `long:"alias" short:"a" optional:"true" help:"Alias to use for this conv inside the group"`
	Role  string `long:"role" short:"r" optional:"true" help:"Role label, e.g. 'lead', 'reviewer'"`
	Descr string `long:"descr" short:"d" optional:"true" help:"Short description of this member"`
}

func groupsAddCmd() *cobra.Command {
	return boa.CmdT[groupsAddParams]{
		Use:         "add",
		Short:       "Add a conversation to a group",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *groupsAddParams, _ *cobra.Command, _ []string) {
			os.Exit(runGroupsAdd(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runGroupsAdd(p *groupsAddParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var resp struct {
		ConvID string `json:"conv_id"`
	}
	if err := DaemonPost("/v1/groups/"+url.PathEscape(p.Group)+"/members", map[string]string{
		"conv":  p.Conv,
		"alias": p.Alias,
		"role":  p.Role,
		"descr": p.Descr,
	}, &resp); err != nil {
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
	Group string `pos:"true" help:"Group name"`
	Conv  string `pos:"true" help:"Conversation: UUID, prefix, or current title"`
}

func groupsRemoveCmd() *cobra.Command {
	return boa.CmdT[groupsRemoveParams]{
		Use:         "remove",
		Short:       "Remove a conversation from a group",
		Aliases:     []string{"rm-member"},
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *groupsRemoveParams, _ *cobra.Command, _ []string) {
			os.Exit(runGroupsRemove(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runGroupsRemove(p *groupsRemoveParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	if err := DaemonDelete("/v1/groups/"+url.PathEscape(p.Group)+"/members/"+url.PathEscape(p.Conv), nil); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	fmt.Fprintf(stdout, "Removed %s from group %q\n", p.Conv, p.Group)
	return rcOK
}
