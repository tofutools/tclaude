package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/common"
)

// groupsCmd is `tclaude agent groups …`. The mutating subcommands refuse
// when invoked from inside an agent (heuristic: $TCLAUDE_SESSION_ID is set
// AND the resolved current conv-id is already a group member).
func groupsCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:         "groups",
		Short:       "Manage agent groups (allow-listed who can talk to whom)",
		Long:        "Group membership is human-controlled: agents can `ls` and `members`, but cannot create/rm/add/remove unless --allow-from-agent is passed.",
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

// refuseIfAgent gates the mutating `groups …` subcommands. Returns nil if
// allowed, or an error explaining why not.
//
// Detection: walk the process tree looking for a `claude` (or `node`)
// ancestor. If one exists, this invocation is coming from inside an agent
// session, which by default cannot mutate group membership.
//
// Override precedence:
//
//  1. Per-command --allow-from-agent flag (testing escape hatch)
//  2. ~/.tclaude/config.json: `agent.allow_agent_mutate_groups: true`
//  3. Default: refuse
func refuseIfAgent(allowFromAgent bool) error {
	if allowFromAgent {
		return nil
	}
	if !calledFromAgent() {
		return nil
	}
	cfg, _ := config.Load()
	if cfg != nil && cfg.Agent != nil && cfg.Agent.AllowAgentMutateGroups {
		return nil
	}
	return errors.New("refusing to mutate group membership from inside a Claude Code session; " +
		"set agent.allow_agent_mutate_groups=true in ~/.tclaude/config.json or pass --allow-from-agent to override")
}

// calledFromAgent returns true if any ancestor process is `claude` (or
// `node`, since Claude Code runs as node). Uses the same walker as
// session.FindClaudePID.
func calledFromAgent() bool {
	return session.FindClaudePID() != 0
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

func runGroupsLs(p *groupsLsParams, stdout, stderr io.Writer) int {
	groups, err := db.ListAgentGroups()
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcIOFailure
	}
	if p.JSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(groups)
		return rcOK
	}
	if len(groups) == 0 {
		fmt.Fprintln(stdout, "(no groups)")
		return rcOK
	}
	for _, g := range groups {
		members, _ := db.ListAgentGroupMembers(g.ID)
		fmt.Fprintf(stdout, "%-20s  %d members  %s\n", g.Name, len(members), g.Descr)
	}
	return rcOK
}

// --- groups create ---

type groupsCreateParams struct {
	Name            string `pos:"true" help:"Group name"`
	Descr           string `long:"descr" short:"d" optional:"true" help:"Optional description"`
	AllowFromAgent  bool   `long:"allow-from-agent" help:"Allow this command to run from inside an agent session (testing escape hatch)"`
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
	if err := refuseIfAgent(p.AllowFromAgent); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcAuth
	}
	if p.Name == "" {
		fmt.Fprintf(stderr, "Error: group name is required\n")
		return rcInvalidArg
	}
	if existing, _ := db.GetAgentGroupByName(p.Name); existing != nil {
		fmt.Fprintf(stderr, "Error: group %q already exists\n", p.Name)
		return rcInvalidArg
	}
	id, err := db.CreateAgentGroup(p.Name, p.Descr)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcIOFailure
	}
	fmt.Fprintf(stdout, "Created group %q (id=%d)\n", p.Name, id)
	return rcOK
}

// --- groups rm ---

type groupsRmParams struct {
	Name           string `pos:"true" help:"Group name"`
	AllowFromAgent bool   `long:"allow-from-agent" help:"Allow this command to run from inside an agent session"`
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
	if err := refuseIfAgent(p.AllowFromAgent); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcAuth
	}
	if p.Name == "" {
		fmt.Fprintf(stderr, "Error: group name is required\n")
		return rcInvalidArg
	}
	g, err := db.GetAgentGroupByName(p.Name)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcIOFailure
	}
	if g == nil {
		fmt.Fprintf(stderr, "Error: no such group %q\n", p.Name)
		return rcNotFound
	}
	if err := db.DeleteAgentGroup(p.Name); err != nil {
		fmt.Fprintf(stderr, "Error: %v (likely there are messages referencing this group; prune them first)\n", err)
		return rcIOFailure
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
	g, err := db.GetAgentGroupByName(p.Name)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcIOFailure
	}
	if g == nil {
		fmt.Fprintf(stderr, "Error: no such group %q\n", p.Name)
		return rcNotFound
	}
	members, err := db.ListAgentGroupMembers(g.ID)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcIOFailure
	}
	out := make([]memberEntry, 0, len(members))
	for _, m := range members {
		row, _ := db.GetConvIndex(m.ConvID)
		title := "(unknown)"
		if row != nil {
			if t := displayTitle(row); t != "" {
				title = t
			}
		}
		out = append(out, memberEntry{
			ConvID: m.ConvID,
			Title:  title,
			Alias:  m.Alias,
			Role:   m.Role,
			Descr:  m.Descr,
		})
	}
	if p.JSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
		return rcOK
	}
	if len(out) == 0 {
		fmt.Fprintln(stdout, "(no members)")
		return rcOK
	}
	for _, m := range out {
		short := m.ConvID
		if len(short) >= 8 {
			short = short[:8]
		}
		alias := m.Alias
		if alias == "" {
			alias = m.Title
		}
		fmt.Fprintf(stdout, "%s  %-20s  %-15s  %s\n", short, alias, m.Role, m.Descr)
	}
	return rcOK
}

// --- groups add ---

type groupsAddParams struct {
	Group          string `pos:"true" help:"Group name"`
	Conv           string `pos:"true" help:"Conversation: UUID, prefix, or current title"`
	Alias          string `long:"alias" short:"a" optional:"true" help:"Alias to use for this conv inside the group"`
	Role           string `long:"role" short:"r" optional:"true" help:"Role label, e.g. 'lead', 'reviewer'"`
	Descr          string `long:"descr" short:"d" optional:"true" help:"Short description of this member"`
	AllowFromAgent bool   `long:"allow-from-agent" help:"Allow this command to run from inside an agent session"`
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
	if err := refuseIfAgent(p.AllowFromAgent); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcAuth
	}
	g, err := db.GetAgentGroupByName(p.Group)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcIOFailure
	}
	if g == nil {
		fmt.Fprintf(stderr, "Error: no such group %q\n", p.Group)
		return rcNotFound
	}
	r, matches, err := resolveSelector(p.Conv)
	if errors.Is(err, errAmbiguous) {
		printAmbiguous(stderr, p.Conv, matches)
		return rcAmbiguous
	}
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcNotFound
	}
	if err := db.AddAgentGroupMember(&db.AgentGroupMember{
		GroupID: g.ID,
		ConvID:  r.ConvID,
		Alias:   p.Alias,
		Role:    p.Role,
		Descr:   p.Descr,
	}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcIOFailure
	}
	short := r.ConvID
	if len(short) >= 8 {
		short = short[:8]
	}
	fmt.Fprintf(stdout, "Added %s to group %q (alias=%q role=%q)\n", short, p.Group, p.Alias, p.Role)
	return rcOK
}

// --- groups remove ---

type groupsRemoveParams struct {
	Group          string `pos:"true" help:"Group name"`
	Conv           string `pos:"true" help:"Conversation: UUID, prefix, or current title"`
	AllowFromAgent bool   `long:"allow-from-agent" help:"Allow this command to run from inside an agent session"`
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
	if err := refuseIfAgent(p.AllowFromAgent); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcAuth
	}
	g, err := db.GetAgentGroupByName(p.Group)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcIOFailure
	}
	if g == nil {
		fmt.Fprintf(stderr, "Error: no such group %q\n", p.Group)
		return rcNotFound
	}
	r, matches, err := resolveSelector(p.Conv)
	if errors.Is(err, errAmbiguous) {
		printAmbiguous(stderr, p.Conv, matches)
		return rcAmbiguous
	}
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcNotFound
	}
	if err := db.RemoveAgentGroupMember(g.ID, r.ConvID); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcIOFailure
	}
	short := r.ConvID
	if len(short) >= 8 {
		short = short[:8]
	}
	fmt.Fprintf(stdout, "Removed %s from group %q\n", short, p.Group)
	return rcOK
}
