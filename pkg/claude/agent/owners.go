package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/common/table"
	"github.com/tofutools/tclaude/pkg/common"
)

// ownerEntry mirrors the daemon's ownerJSON payload for /v1/groups/<g>/owners.
type ownerEntry struct {
	AgentID   string `json:"agent_id,omitempty"`
	ConvID    string `json:"conv_id"`
	Title     string `json:"title"`
	Online    bool   `json:"online"`
	GrantedAt string `json:"granted_at,omitempty"`
	GrantedBy string `json:"granted_by,omitempty"`
}

// --- groups owners ---

type groupsOwnersParams struct {
	Name string `pos:"true" help:"Group name"`
	JSON bool   `long:"json" help:"Output JSON"`
}

func groupsOwnersCmd() *cobra.Command {
	return boa.CmdT[groupsOwnersParams]{
		Use:   "owners",
		Short: "List owners of a group (can message members without being one)",
		Long: "Owners can message a group's members and multicast to the group " +
			"without being members themselves. Useful for coordinator agents that " +
			"orchestrate teams. An owner who is also a member appears in both " +
			"`groups members` (with an `(owner)` tag on its role) and here.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *groupsOwnersParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Name).SetAlternativesFunc(completeGroupNames)
			return nil
		},
		RunFunc: func(p *groupsOwnersParams, _ *cobra.Command, _ []string) {
			os.Exit(runGroupsOwners(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runGroupsOwners(p *groupsOwnersParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var owners []ownerEntry
	path := "/v1/groups/" + url.PathEscape(p.Name) + "/owners"
	if err := DaemonGet(path, &owners); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if p.JSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(owners); err != nil {
			return rcIOFailure
		}
		return rcOK
	}
	if len(owners) == 0 {
		fmt.Fprintln(stdout, "(no owners)")
		return rcOK
	}
	tbl := table.New(
		table.Column{Header: "", Width: 1},
		table.Column{Header: "ID", Width: 12},
		table.Column{Header: "TITLE", MinWidth: 8, Weight: 1, Truncate: true},
		table.Column{Header: "GRANTED", MinWidth: 10, Weight: 0.6, Truncate: true},
	)
	tbl.SetTerminalWidth(table.GetTerminalWidth())
	for _, o := range owners {
		tbl.AddRow(table.Row{Cells: []string{
			onlineMark(o.Online),
			shortAgentID(o.AgentID, o.ConvID),
			o.Title,
			o.GrantedAt,
		}})
	}
	fmt.Fprintln(stdout, tbl.Render())
	return rcOK
}

// --- groups grant-owner ---

type groupsGrantOwnerParams struct {
	Group    string `pos:"true" help:"Group name"`
	Conv     string `pos:"true" help:"Conversation: UUID, prefix, or current title"`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup. Capped at 300s. Timeout = deny."`
}

func groupsGrantOwnerCmd() *cobra.Command {
	return boa.CmdT[groupsGrantOwnerParams]{
		Use:   "grant-owner",
		Short: "Grant ownership of a group to a conversation",
		Long: "Owners can message the group's members and multicast to the group " +
			"without being members themselves. Permission slug `groups.own` (default: " +
			"human-only).",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *groupsGrantOwnerParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Group).SetAlternativesFunc(completeGroupNames)
			boa.GetParamT(ctx, &p.Conv).SetAlternativesFunc(completeConvSelectors)
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *groupsGrantOwnerParams, _ *cobra.Command, _ []string) {
			os.Exit(runGroupsGrantOwner(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runGroupsGrantOwner(p *groupsGrantOwnerParams, stdout, stderr io.Writer) int {
	if p.Group == "" || p.Conv == "" {
		fmt.Fprintln(stderr, "Error: <group> and <conv> are both required")
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
	body := map[string]string{"conv": p.Conv}
	var resp struct {
		Group   string `json:"group"`
		AgentID string `json:"agent_id"`
		ConvID  string `json:"conv_id"`
	}
	path := "/v1/groups/" + url.PathEscape(p.Group) + "/owners"
	if err := DaemonRequest(http.MethodPost, path, body, &resp, DaemonOpts{AskHuman: ask}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	fmt.Fprintf(stdout, "Granted ownership of %q to %s\n", resp.Group, lookupID(resp.AgentID, resp.ConvID))
	return rcOK
}

// --- groups revoke-owner ---

type groupsRevokeOwnerParams struct {
	Group    string `pos:"true" help:"Group name"`
	Conv     string `pos:"true" help:"Conversation: UUID, prefix, or current title"`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup. Capped at 300s. Timeout = deny."`
}

func groupsRevokeOwnerCmd() *cobra.Command {
	return boa.CmdT[groupsRevokeOwnerParams]{
		Use:         "revoke-owner",
		Short:       "Revoke ownership of a group from a conversation",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *groupsRevokeOwnerParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Group).SetAlternativesFunc(completeGroupNames)
			boa.GetParamT(ctx, &p.Conv).SetAlternativesFunc(completeConvSelectors)
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *groupsRevokeOwnerParams, _ *cobra.Command, _ []string) {
			os.Exit(runGroupsRevokeOwner(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runGroupsRevokeOwner(p *groupsRevokeOwnerParams, stdout, stderr io.Writer) int {
	if p.Group == "" || p.Conv == "" {
		fmt.Fprintln(stderr, "Error: <group> and <conv> are both required")
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
	path := "/v1/groups/" + url.PathEscape(p.Group) + "/owners/" + url.PathEscape(p.Conv)
	if err := DaemonRequest(http.MethodDelete, path, nil, nil, DaemonOpts{AskHuman: ask}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	fmt.Fprintf(stdout, "Revoked ownership of %q from %s\n", p.Group, p.Conv)
	return rcOK
}
