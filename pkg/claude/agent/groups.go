package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
			groupsArchiveCmd(),
			groupsUnarchiveCmd(),
			groupsMembersCmd(),
			groupsAddCmd(),
			groupsRemoveCmd(),
			groupsUpdateMemberCmd(),
			groupsStopCmd(),
			groupsResumeCmd(),
			groupsRetireCmd(),
			groupsOwnersCmd(),
			groupsGrantOwnerCmd(),
			groupsRevokeOwnerCmd(),
			groupsRenameCmd(),
			groupsSetDescrCmd(),
			groupsSetDefaultDirCmd(),
			groupsSetDefaultProfileCmd(),
			groupsSetRemoteControlCmd(),
			groupsSetContextCmd(),
			groupsSetMaxMembersCmd(),
			groupsSetNotificationsCmd(),
			groupsCloneCmd(),
			groupsLinkCmd(),
			groupsLinksAllCmd(),
			groupsWhyCanMessageCmd(),
			groupsExportCmd(),
			groupsImportCmd(),
			groupsTransfersCmd(),
		},
	}.ToCobra()
}

// --- groups ls ---

type groupsLsParams struct {
	State    string `long:"state" optional:"true" help:"Filter: online (any member online) | offline (no member online)"`
	Archived bool   `long:"archived" help:"Include archived (soft-deleted) groups in the listing"`
	JSON     bool   `long:"json" help:"Output JSON"`
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
	Name           string `json:"name"`
	Descr          string `json:"descr,omitempty"`
	Members        int    `json:"members"`
	Online         int    `json:"online"`
	MaxMembers     int    `json:"max_members,omitempty"`     // hard member cap; 0 = unlimited
	DefaultProfile string `json:"default_profile,omitempty"` // spawn profile whose launch fields fill blank spawn fields; "" = none
	Archived       bool   `json:"archived,omitempty"`
	NotifyMuted    bool   `json:"notify_muted,omitempty"`          // OS notifications switched off for this group's agents
	RemoteControl  string `json:"remote_control_policy,omitempty"` // group remote-control policy: inherit | optin | deny (JOH-262)
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
	path := "/v1/groups"
	if p.Archived {
		path += "?archived=1"
	}
	var groups []groupSummary
	if err := DaemonGet(path, &groups); err != nil {
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
		table.Column{Header: "MEMBERS", Width: 9, Align: table.AlignRight},
		table.Column{Header: "ONLINE", Width: 6, Align: table.AlignRight},
		table.Column{Header: "PROFILE", MinWidth: 7, Weight: 0.4, Truncate: true},
		table.Column{Header: "DESCR", MinWidth: 10, Weight: 1.4, Truncate: true},
	)
	tbl.SetTerminalWidth(table.GetTerminalWidth())
	for _, g := range groups {
		name := g.Name
		if g.Archived {
			// Visually mark archived rows so the listing distinguishes
			// them from live groups when --archived is on.
			name += " (archived)"
		}
		if g.NotifyMuted {
			name += " 🔕"
		}
		// Show the member count against the cap (e.g. "3/10") when the
		// group has one; a bare count when it's unlimited.
		members := fmt.Sprintf("%d", g.Members)
		if g.MaxMembers > 0 {
			members = fmt.Sprintf("%d/%d", g.Members, g.MaxMembers)
		}
		tbl.AddRow(table.Row{Cells: []string{
			name,
			members,
			fmt.Sprintf("%d", g.Online),
			g.DefaultProfile,
			g.Descr,
		}})
	}
	fmt.Fprintln(stdout, tbl.Render())
	return rcOK
}

// --- groups create ---

// GroupsCreateParams drives `tclaude agent groups create`. The optional
// repeatable `--member` flag bootstraps a team in one shot: the CLI
// creates the group first, then spawns one fresh CC session per member
// (via the existing `groups.spawn` daemon endpoint).
type GroupsCreateParams struct {
	Name        string `pos:"true" help:"Group name"`
	Descr       string `long:"descr" short:"d" optional:"true" help:"Optional description"`
	Context     string `long:"context" optional:"true" help:"Shared startup context delivered to the inbox of agents spawned into this group. For multi-line context use --context-file."`
	ContextFile string `long:"context-file" optional:"true" help:"Read the group startup context from this file (alternative to --context)."`
	// Members is registered manually as a non-splitting StringArray in
	// groupsCreateCmd's InitFuncCtx — see there for the boa/StringSlice why.
	Members    []string `long:"member" optional:"true"`
	MaxMembers int      `long:"max-members" optional:"true" help:"Hard cap on the group's member count (0 = unlimited, the default). A spawn that would exceed it is refused. Change later with 'groups set-max-members'."`
	AskHuman   string   `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s' or '60'). Capped at 300s. Timeout = deny."`
}

// memberFlagHelp documents the repeatable `--member` flag. It lives here (not
// in a struct tag) because the flag is registered manually as a StringArray —
// see GroupsCreateParams.Members and groupsCreateCmd's InitFuncCtx.
const memberFlagHelp = "Bootstrap a team member: comma-separated key=value pairs (name=NAME,role=TAG,descr=TEXT,cwd=PATH). Repeatable. 'name' is required (it becomes the new agent's conversation title); 'cwd' defaults to caller's cwd. Values cannot contain commas (a value may contain '='); for richer descriptions use 'groups update-member' afterwards."

func groupsCreateCmd() *cobra.Command {
	return boa.CmdT[GroupsCreateParams]{
		Use:   "create",
		Short: "Create a new group, optionally bootstrapping members in one call",
		Long: "Create a new group. With one or more `--member` flags, immediately " +
			"spawn fresh CC sessions for each member and add them to the group. Each " +
			"`--member` value is a comma-separated list of key=value pairs: " +
			"`name=lead,role=tech-lead,descr=Owns the diff,cwd=.`. The `name` becomes " +
			"the new agent's conversation title. Member spawn requires `groups.spawn` " +
			"(default human-only).",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *GroupsCreateParams, cmd *cobra.Command) error {
			// `Name` is brand-new on create; no value-completion to offer.
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			// Register `--member` as a non-splitting StringArray. boa maps a
			// []string field to pflag's StringSlice, which CSV-splits each
			// flag value on commas at parse time; parseMemberSpec ALSO splits
			// on commas, so the two collide and it only ever sees the first
			// "name=..." fragment (every later pair arrives as its own
			// nameless member). SetNoFlag suppresses boa's own registration,
			// then StringArrayVar binds --member directly into the shared
			// params struct so the whole spec reaches parseMemberSpec intact.
			boa.GetParamT(ctx, &p.Members).SetNoFlag(true)
			cmd.Flags().StringArrayVar(&p.Members, "member", nil, memberFlagHelp)
			return nil
		},
		RunFunc: func(p *GroupsCreateParams, _ *cobra.Command, _ []string) {
			os.Exit(RunGroupsCreate(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

// memberSpec is the parsed shape of one `--member` flag value.
type memberSpec struct {
	Name  string
	Role  string
	Descr string
	Cwd   string
}

// parseMemberSpec turns "name=lead,role=tech-lead,descr=Owns the diff,cwd=."
// into a memberSpec. Values can't contain commas or '=' (for v1) — the
// helper documents this trade-off in the user-facing error.
func parseMemberSpec(s string) (*memberSpec, error) {
	spec := &memberSpec{}
	parts := strings.Split(s, ",")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			return nil, fmt.Errorf("--member %q: expected key=value pairs separated by commas, got %q", s, part)
		}
		key := strings.ToLower(strings.TrimSpace(kv[0]))
		val := strings.TrimSpace(kv[1])
		switch key {
		case "name":
			spec.Name = val
		case "role":
			spec.Role = val
		case "descr":
			spec.Descr = val
		case "cwd":
			spec.Cwd = val
		default:
			return nil, fmt.Errorf("--member %q: unknown key %q (allowed: name, role, descr, cwd)", s, key)
		}
	}
	if spec.Name == "" {
		return nil, fmt.Errorf("--member %q: 'name' is required", s)
	}
	return spec, nil
}

// RunGroupsCreate dispatches the create + (optional) bootstrap.
// Exported so flow tests can drive it directly through the agent
// client bridge.
func RunGroupsCreate(p *GroupsCreateParams, stdout, stderr io.Writer) int {
	if p.Name == "" {
		fmt.Fprintf(stderr, "Error: group name is required\n")
		return rcInvalidArg
	}

	// Resolve the optional startup context: --context inline or
	// --context-file from disk, not both.
	if p.Context != "" && p.ContextFile != "" {
		fmt.Fprintf(stderr, "Error: pass --context OR --context-file, not both\n")
		return rcInvalidArg
	}
	groupContext := p.Context
	if p.ContextFile != "" {
		data, err := os.ReadFile(p.ContextFile)
		if err != nil {
			fmt.Fprintf(stderr, "Error: reading %q: %v\n", p.ContextFile, err)
			return rcInvalidArg
		}
		groupContext = string(data)
	}

	if p.MaxMembers < 0 {
		fmt.Fprintf(stderr, "Error: --max-members must be >= 0 (0 = unlimited)\n")
		return rcInvalidArg
	}

	// Parse member specs up-front so a typo doesn't leave an empty
	// group sitting around. Fails the whole command before any DB work.
	specs := make([]*memberSpec, 0, len(p.Members))
	for _, m := range p.Members {
		spec, err := parseMemberSpec(m)
		if err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return rcInvalidArg
		}
		specs = append(specs, spec)
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
	if err := DaemonRequest(http.MethodPost, "/v1/groups", map[string]any{
		"name":            p.Name,
		"descr":           p.Descr,
		"default_context": groupContext,
		"max_members":     p.MaxMembers,
	}, &resp, DaemonOpts{AskHuman: ask}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	fmt.Fprintf(stdout, "Created group %q (id=%d)\n", resp.Name, resp.ID)

	// Bootstrap members. Each spawn is independent — partial failure is
	// reported but doesn't abort the rest. The group already exists at
	// this point, so a half-bootstrapped team is a recoverable state
	// (the human can retry just the failures via `agent spawn`).
	if len(specs) > 0 {
		spawned, failed := bootstrapMembers(p.Name, specs, stdout, stderr)
		fmt.Fprintf(stdout, "Bootstrapped %d/%d members\n", spawned, len(specs))
		if failed > 0 && spawned == 0 {
			return rcIOFailure
		}
	}
	return rcOK
}

// bootstrapMembers iterates parsed member specs, calling the
// `groups.spawn` daemon endpoint for each. Returns (spawned, failed).
// Cwd defaults to the caller's cwd when the spec doesn't pin one,
// matching the pattern `agent spawn` and `--join-group` use.
func bootstrapMembers(groupName string, specs []*memberSpec, stdout, stderr io.Writer) (spawned, failed int) {
	callerCwd := ""
	if wd, err := os.Getwd(); err == nil {
		callerCwd = wd
	}
	path := "/v1/groups/" + groupName + "/spawn"
	for _, spec := range specs {
		cwd := spec.Cwd
		if cwd == "" {
			cwd = callerCwd
		}
		body := map[string]any{
			"name":            spec.Name,
			"role":            spec.Role,
			"descr":           spec.Descr,
			"cwd":             cwd,
			"timeout_seconds": 30,
		}
		var sresp SpawnResponse
		if err := DaemonRequest(http.MethodPost, path, body, &sresp, DaemonOpts{}); err != nil {
			fmt.Fprintf(stderr, "  Failed to spawn member name=%q: %v\n", spec.Name, err)
			failed++
			continue
		}
		fmt.Fprintf(stdout, "  Spawned member name=%q conv=%s tmux=%s\n",
			spec.Name, short(sresp.ConvID), sresp.TmuxSession)
		spawned++
	}
	return spawned, failed
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
	// AgentID is the stable actor key — the canonical ID shown for a member.
	AgentID string `json:"agent_id,omitempty"`
	ConvID  string `json:"conv_id"`
	Title   string `json:"title"`
	// CreatedAt is the conversation's creation timestamp (RFC3339); the
	// listing defaults to newest-first on it, surfaced as the AGE column.
	CreatedAt string `json:"created_at,omitempty"`
	Role      string `json:"role,omitempty"`
	Descr     string `json:"descr,omitempty"`
	// Branch is the git branch / worktree the member is working on,
	// from its conv_index row. Empty when not indexed / not in a repo.
	Branch string `json:"branch,omitempty"`
	Online bool   `json:"online"`
	Owner  bool   `json:"owner,omitempty"`
}

// relTimeAgo renders an RFC3339 timestamp as a coarse "N{s,m,h,d} ago"
// string, or "" when empty/unparseable. It is the Go port of the
// dashboard's relTime (helpers.js) — same buckets, same wording — so the
// AGE column reads identically in the CLI and the browser.
func relTimeAgo(iso string) string {
	if iso == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, iso)
	if err != nil {
		return ""
	}
	sec := max(int(time.Since(t).Seconds()), 0)
	switch {
	case sec < 60:
		return fmt.Sprintf("%ds ago", sec)
	case sec < 3600:
		return fmt.Sprintf("%dm ago", sec/60)
	case sec < 86400:
		return fmt.Sprintf("%dh ago", sec/3600)
	default:
		return fmt.Sprintf("%dd ago", sec/86400)
	}
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
		table.Column{Header: "ID", Width: 12},
		table.Column{Header: "NAME", MinWidth: 8, Weight: 0.8, Truncate: true},
		table.Column{Header: "AGE", Width: 7},
		table.Column{Header: "ROLE", MinWidth: 6, Weight: 0.4, Truncate: true},
		table.Column{Header: "BRANCH", MinWidth: 8, Weight: 0.6, Truncate: true},
		table.Column{Header: "DESCR", MinWidth: 10, Weight: 1.2, Truncate: true},
	)
	tbl.SetTerminalWidth(table.GetTerminalWidth())
	for _, m := range members {
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
		// ID is the stable agent_id (short form); NAME is the display title.
		tbl.AddRow(table.Row{Cells: []string{
			onlineMark(m.Online),
			shortAgentID(m.AgentID, m.ConvID),
			m.Title,
			relTimeAgo(m.CreatedAt),
			role,
			m.Branch,
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
	fmt.Fprintf(stdout, "Added %s to group %q (role=%q)\n", shortID, p.Group, p.Role)
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

// --- groups retire ---

type groupsRetireParams struct {
	Name       string `pos:"true" help:"Group name"`
	Status     string `long:"status" optional:"true" help:"Only retire members of this live status (comma-separated). One or more of: idle, offline, working, awaiting, error. Default (or 'all') = every member." alts:"all,idle,offline,working,awaiting,error"`
	NoShutdown bool   `long:"no-shutdown" help:"Leave each retired member's running session alive. By default retire also soft-exits the running tmux pane (sends /exit); pass this to keep the processes running."`
	Reason     string `long:"reason" short:"r" optional:"true" help:"Why the members are being retired (recorded in the audit trail)"`
	AskHuman   string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s' or '60'). Capped at 300s. Timeout = deny."`
}

func groupsRetireCmd() *cobra.Command {
	return boa.CmdT[groupsRetireParams]{
		Use:   "retire",
		Short: "Retire every other member of a group (bulk soft-delete)",
		Long: "Retires every OTHER active-agent member of the group in one shot — the bulk parallel of `tclaude agent retire`. " +
			"Each member is demoted to a plain conversation: its group memberships are dropped and its permission/sudo grants " +
			"revoked, but the conversation itself (.jsonl, history) is left intact and reinstatable. This is the non-destructive " +
			"bulk cleanup, not `agent delete`. " +
			"\n\n" +
			"By default each retired member's running tmux pane is also soft-exited (sends /exit); pass --no-shutdown to leave " +
			"the processes running. The CALLER's own conversation is always skipped — an agent never retires itself. Members that " +
			"aren't active agents (placeholders, already-retired convs) are skipped; retire is idempotent. " +
			"\n\n" +
			"Gated on the `groups.retire` permission (default human-only). Note retire leaves ALL of a member's groups, not just this one.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *groupsRetireParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Name).SetAlternativesFunc(completeGroupNames)
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *groupsRetireParams, _ *cobra.Command, _ []string) {
			os.Exit(runGroupsRetire(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runGroupsRetire(p *groupsRetireParams, stdout, stderr io.Writer) int {
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
	// Spell shutdown out explicitly so the CLI default is independent of
	// the server-side default (which also defaults shutdown ON).
	q := url.Values{}
	if p.NoShutdown {
		q.Set("shutdown", "0")
	} else {
		q.Set("shutdown", "1")
	}
	if reason := strings.TrimSpace(p.Reason); reason != "" {
		q.Set("reason", reason)
	}
	if status := strings.TrimSpace(p.Status); status != "" {
		q.Set("status", status)
	}
	path := "/v1/groups/" + url.PathEscape(p.Name) + "/retire?" + q.Encode()

	var resp struct {
		Group   string `json:"group"`
		Action  string `json:"action"`
		Members []struct {
			AgentID string `json:"agent_id,omitempty"`
			ConvID  string `json:"conv_id"`
			Title   string `json:"title,omitempty"`
			Action  string `json:"action"`
			Detail  string `json:"detail,omitempty"`
			TmuxSes string `json:"tmux_session,omitempty"`
		} `json:"members"`
		Warnings []string `json:"warnings,omitempty"`
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
		table.Column{Header: "ID", Width: 12},
		table.Column{Header: "NAME", MinWidth: 8, Weight: 0.6, Truncate: true},
		table.Column{Header: "ACTION", MinWidth: 10, Weight: 0.6, Truncate: true},
		table.Column{Header: "DETAIL", MinWidth: 10, Weight: 1.4, Truncate: true},
	)
	tbl.SetTerminalWidth(table.GetTerminalWidth())
	for _, m := range resp.Members {
		name := m.Title
		if name == "" {
			name = "(unnamed)"
		}
		tbl.AddRow(table.Row{Cells: []string{
			shortAgentID(m.AgentID, m.ConvID), name, m.Action, m.Detail,
		}})
	}
	fmt.Fprintf(stdout, "Group %q — %s:\n", resp.Group, resp.Action)
	fmt.Fprintln(stdout, tbl.Render())
	for _, warn := range resp.Warnings {
		fmt.Fprintf(stdout, "⚠ %s\n", warn)
	}
	return rcOK
}

// runGroupsLifecycle is shared between stop/resume — both endpoints
// return the same per-member result shape, only the action label
// changes.
func runGroupsLifecycle(path string, ask time.Duration, stdout, stderr io.Writer) int {
	var resp struct {
		Group   string `json:"group"`
		Action  string `json:"action"`
		Members []struct {
			AgentID string `json:"agent_id,omitempty"`
			ConvID  string `json:"conv_id"`
			Title   string `json:"title,omitempty"`
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
		table.Column{Header: "ID", Width: 12},
		table.Column{Header: "NAME", MinWidth: 8, Weight: 0.6, Truncate: true},
		table.Column{Header: "ACTION", MinWidth: 10, Weight: 0.6, Truncate: true},
		table.Column{Header: "DETAIL", MinWidth: 10, Weight: 1.4, Truncate: true},
	)
	tbl.SetTerminalWidth(table.GetTerminalWidth())
	for _, m := range resp.Members {
		name := m.Title
		if name == "" {
			name = "(unnamed)"
		}
		tbl.AddRow(table.Row{Cells: []string{
			shortAgentID(m.AgentID, m.ConvID), name, m.Action, m.Detail,
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
	Role     string `long:"role" short:"r" optional:"true" help:"New role label (pass empty string to clear)"`
	Descr    string `long:"descr" short:"d" optional:"true" help:"New description (pass empty string to clear)"`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s' or '60'). Capped at 300s. Timeout = deny."`
}

func groupsUpdateMemberCmd() *cobra.Command {
	return boa.CmdT[groupsUpdateMemberParams]{
		Use:         "update-member",
		Short:       "Edit role/descr on an existing group member",
		Long:        "Patch the role or descr of a member already in a group. Only the flags you pass are touched; pass an empty string (e.g. --role='') to clear a field. To rename an agent use `tclaude agent rename` — an agent's single name is its conversation title, not a per-group field. Same human-only gate as `add`/`remove`.",
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
	if cmd.Flags().Changed("role") {
		body["role"] = p.Role
	}
	if cmd.Flags().Changed("descr") {
		body["descr"] = p.Descr
	}
	if len(body) == 0 {
		fmt.Fprintf(stderr, "Error: at least one of --role / --descr is required\n")
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

// --- groups archive / unarchive ---

type groupsArchiveParams struct {
	Name     string `pos:"true" help:"Group name"`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s'). Capped at 300s. Timeout = deny."`
}

func groupsArchiveCmd() *cobra.Command {
	return boa.CmdT[groupsArchiveParams]{
		Use:   "archive",
		Short: "Archive (soft-delete) a group",
		Long: "Soft-deletes the group: freezes membership + ownership, refuses " +
			"future mutating operations (add/remove members, grant/revoke owners, " +
			"messages), and hides the group from default listings. Message " +
			"history is preserved. Distinct from `groups rm` (which destroys " +
			"the group + history outright). Reverse with `groups unarchive`.\n\n" +
			"Note: archive does NOT auto-stop the group's running members. " +
			"If you also want to end the running tmux panes, run `groups stop` " +
			"first — the two-step keeps the destructive part visible.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *groupsArchiveParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Name).SetAlternativesFunc(completeGroupNames)
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *groupsArchiveParams, _ *cobra.Command, _ []string) {
			os.Exit(runGroupsArchiveOrUnarchive(p.Name, "archive", p.AskHuman, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func groupsUnarchiveCmd() *cobra.Command {
	return boa.CmdT[groupsArchiveParams]{
		Use:         "unarchive",
		Short:       "Reverse `groups archive` — re-activate a soft-deleted group",
		Long:        "Clears the group's archived flag so mutating operations are accepted again and the group reappears in default listings. Idempotent on already-active groups.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *groupsArchiveParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Name).SetAlternativesFunc(completeArchivedGroupNames)
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *groupsArchiveParams, _ *cobra.Command, _ []string) {
			os.Exit(runGroupsArchiveOrUnarchive(p.Name, "unarchive", p.AskHuman, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

// --- groups clone ---

type groupsCloneParams struct {
	Source   string `pos:"true" help:"Source group to clone"`
	NewName  string `pos:"true" optional:"true" help:"Optional new group name (defaults to <source>-c-<N>)"`
	NoAgents bool   `long:"no-agents" help:"Clone only the group's settings + owners — do NOT clone the member agents. The new group comes up with no members."`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s'). Capped at 300s. Timeout = deny."`
}

func groupsCloneCmd() *cobra.Command {
	return boa.CmdT[groupsCloneParams]{
		Use:   "clone",
		Short: "Clone an entire group: snapshot members + owners, fork each into a new group",
		Long: "Clones every member of <source> via the same `agent clone` machinery, " +
			"attaches the clones to a new group, and copies <source>'s owners (same conv-ids) " +
			"onto the new group. The source group is left untouched.\n\n" +
			"Default new group name is <source>-c-<N> (smallest free N globally). " +
			"Clone-of-a-clone strips the existing -c-<N> suffix before computing the next " +
			"so names don't nest.\n\n" +
			"Each member clone uses the copy-jsonl path so the clone starts with the " +
			"source's conversation history. Owners stay as the same conv-id (no clone). " +
			"Per-conv permissions on each member are copied to the clone (best-effort).\n\n" +
			"The new group always carries every source setting — default directory, " +
			"description, startup context, default profile, max-members cap and the notify " +
			"switch.\n\n" +
			"Pass --no-agents to clone only the group's settings + owners and skip the " +
			"member agents entirely (the new group comes up with no members).",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *groupsCloneParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Source).SetAlternativesFunc(completeGroupNames)
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *groupsCloneParams, _ *cobra.Command, _ []string) {
			os.Exit(runGroupsClone(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runGroupsClone(p *groupsCloneParams, stdout, stderr io.Writer) int {
	if p.Source == "" {
		fmt.Fprintf(stderr, "Error: source group is required\n")
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
	if p.NewName != "" {
		body["new_name"] = p.NewName
	}
	if p.NoAgents {
		body["no_clone_members"] = true
	}
	var resp struct {
		Group        string `json:"group"`
		SrcGroup     string `json:"src_group"`
		OwnersCopied int    `json:"owners_copied"`
		Members      []struct {
			SrcConv string `json:"src_conv"`
			NewConv string `json:"new_conv,omitempty"`
			Title   string `json:"title,omitempty"`
			Label   string `json:"label,omitempty"`
			Error   string `json:"error,omitempty"`
		} `json:"members"`
	}
	path := "/v1/groups/" + url.PathEscape(p.Source) + "/clone"
	if err := DaemonRequest(http.MethodPost, path, body, &resp, DaemonOpts{AskHuman: ask}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	fmt.Fprintf(stdout, "%s -> %s (%d owner(s) copied)\n",
		resp.SrcGroup, resp.Group, resp.OwnersCopied)
	if p.NoAgents {
		fmt.Fprintf(stdout, "  (settings + owners only — no member agents cloned)\n")
	}
	failed := 0
	for _, m := range resp.Members {
		if m.Error != "" {
			fmt.Fprintf(stdout, "  ! %s -> FAILED: %s\n", m.SrcConv, m.Error)
			failed++
			continue
		}
		fmt.Fprintf(stdout, "  + %s -> %s (title %s)\n",
			m.SrcConv, m.NewConv, m.Title)
	}
	if failed > 0 {
		fmt.Fprintf(stderr, "%d member(s) failed; retry with `tclaude agent clone <src-conv> --target %s`\n",
			failed, resp.Group)
		return rcIOFailure
	}
	return rcOK
}

// --- groups rename ---

type groupsRenameParams struct {
	Old      string `pos:"true" help:"Existing group name"`
	New      string `pos:"true" help:"New group name"`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s'). Capped at 300s. Timeout = deny."`
}

func groupsRenameCmd() *cobra.Command {
	return boa.CmdT[groupsRenameParams]{
		Use:   "rename",
		Short: "Rename a group",
		Long: "Rename a group's canonical name. Membership, ownership, " +
			"messages, and cron jobs all stay attached (the schema uses " +
			"integer foreign keys, so the rename is a single-row update). " +
			"Same-name rename is a no-op. The previous name is recorded in " +
			"agent_group_audit so the history is debuggable.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *groupsRenameParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Old).SetAlternativesFunc(completeGroupNames)
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *groupsRenameParams, _ *cobra.Command, _ []string) {
			os.Exit(runGroupsRename(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runGroupsRename(p *groupsRenameParams, stdout, stderr io.Writer) int {
	if p.Old == "" || p.New == "" {
		fmt.Fprintf(stderr, "Error: both <old> and <new> names are required\n")
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
		Group   string `json:"group"`
		OldName string `json:"old_name"`
		Action  string `json:"action"`
	}
	body := map[string]string{"new_name": p.New}
	path := "/v1/groups/" + url.PathEscape(p.Old) + "/rename"
	if err := DaemonRequest(http.MethodPost, path, body, &resp, DaemonOpts{AskHuman: ask}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if resp.OldName == resp.Group {
		fmt.Fprintf(stdout, "%s: no-op (same name)\n", resp.Group)
	} else {
		fmt.Fprintf(stdout, "%s -> %s\n", resp.OldName, resp.Group)
	}
	return rcOK
}

// --- groups set-descr ---

type groupsSetDescrParams struct {
	Group    string `pos:"true" help:"Group to configure"`
	Descr    string `pos:"true" optional:"true" help:"New one-line description for the group. Omit to clear it."`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s'). Capped at 300s. Timeout = deny."`
}

func groupsSetDescrCmd() *cobra.Command {
	return boa.CmdT[groupsSetDescrParams]{
		Use:   "set-descr",
		Short: "Set (or clear) a group's description",
		Long: "Set the group's own one-line description — the text shown next " +
			"to the group name on the dashboard. This is the group entity's " +
			"description, distinct from the per-member descr edited with " +
			"`groups update-member`. Pass the new description as the second " +
			"argument; omit it to clear the description. Gated on the " +
			"`groups.rename` permission (default human-only).",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *groupsSetDescrParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Group).SetAlternativesFunc(completeGroupNames)
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *groupsSetDescrParams, _ *cobra.Command, _ []string) {
			os.Exit(runGroupsSetDescr(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runGroupsSetDescr(p *groupsSetDescrParams, stdout, stderr io.Writer) int {
	if p.Group == "" {
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
		Group string `json:"group"`
		Descr string `json:"descr"`
	}
	// descr is *string server-side, so an omitted positional sends ""
	// — distinct from omitting the field — which clears the description.
	body := map[string]any{"descr": p.Descr}
	path := "/v1/groups/" + url.PathEscape(p.Group)
	if err := DaemonRequest(http.MethodPatch, path, body, &resp, DaemonOpts{AskHuman: ask}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if resp.Descr == "" {
		fmt.Fprintf(stdout, "%s: description cleared\n", resp.Group)
	} else {
		fmt.Fprintf(stdout, "%s: description set to %q\n", resp.Group, resp.Descr)
	}
	return rcOK
}

// --- groups set-default-dir ---

type groupsSetDefaultDirParams struct {
	Group    string `pos:"true" help:"Group to configure"`
	Dir      string `pos:"true" optional:"true" help:"Default working directory for agents spawned into this group. Relative paths resolve against the current directory. Omit to clear the default."`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s'). Capped at 300s. Timeout = deny."`
}

func groupsSetDefaultDirCmd() *cobra.Command {
	return boa.CmdT[groupsSetDefaultDirParams]{
		Use:   "set-default-dir",
		Short: "Set (or clear) a group's default spawn directory",
		Long: "Set the working directory pre-filled into the spawn form for " +
			"agents created directly into this group. The daemon also " +
			"substitutes it server-side when a spawn request leaves cwd " +
			"blank, so `tclaude agent spawn <group>` and the dashboard's " +
			"'+ spawn agent' button both inherit it. Omit <dir> to clear " +
			"the default (spawns then fall back to the daemon's own cwd). " +
			"Gated on the `groups.rename` permission (default human-only).",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *groupsSetDefaultDirParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Group).SetAlternativesFunc(completeGroupNames)
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *groupsSetDefaultDirParams, _ *cobra.Command, _ []string) {
			os.Exit(runGroupsSetDefaultDir(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runGroupsSetDefaultDir(p *groupsSetDefaultDirParams, stdout, stderr io.Writer) int {
	if p.Group == "" {
		fmt.Fprintf(stderr, "Error: group name is required\n")
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	// Resolve a non-empty path to absolute so the stored value is
	// unambiguous regardless of where the spawn later runs from.
	dir := strings.TrimSpace(p.Dir)
	if dir != "" {
		abs, err := filepath.Abs(dir)
		if err != nil {
			fmt.Fprintf(stderr, "Error: resolving %q: %v\n", dir, err)
			return rcInvalidArg
		}
		dir = abs
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
		Group      string `json:"group"`
		DefaultCwd string `json:"default_cwd"`
	}
	body := map[string]string{"default_cwd": dir}
	path := "/v1/groups/" + url.PathEscape(p.Group)
	if err := DaemonRequest(http.MethodPatch, path, body, &resp, DaemonOpts{AskHuman: ask}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if resp.DefaultCwd == "" {
		fmt.Fprintf(stdout, "%s: default spawn dir cleared\n", resp.Group)
	} else {
		fmt.Fprintf(stdout, "%s: default spawn dir set to %s\n", resp.Group, resp.DefaultCwd)
	}
	return rcOK
}

// --- groups set-default-profile ---

type groupsSetDefaultProfileParams struct {
	Group    string `pos:"true" help:"Group to configure"`
	Profile  string `pos:"true" optional:"true" help:"Name of the spawn profile whose launch fields fill blank spawn fields for this group's agents. Omit to clear the default."`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s'). Capped at 300s. Timeout = deny."`
}

func groupsSetDefaultProfileCmd() *cobra.Command {
	return boa.CmdT[groupsSetDefaultProfileParams]{
		Use:   "set-default-profile",
		Short: "Set (or clear) a group's default spawn profile",
		Long: "Set the spawn profile (JOH-210) whose launch fields " +
			"(harness/model/effort/sandbox/…) fill blank spawn fields server-side, " +
			"so `tclaude agent spawn <group>`, the dashboard's '+ spawn agent' " +
			"button and group-template instantiation all inherit it. The profile " +
			"carries its own harness, so a group can default its team onto a Codex " +
			"profile — the harness-correct replacement for the retired " +
			"`set-default-model`. Omit <profile> to clear the default. Gated on " +
			"the `groups.rename` permission (default human-only).",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *groupsSetDefaultProfileParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Group).SetAlternativesFunc(completeGroupNames)
			boa.GetParamT(ctx, &p.Profile).SetAlternativesFunc(completeSpawnProfileNames)
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *groupsSetDefaultProfileParams, _ *cobra.Command, _ []string) {
			os.Exit(runGroupsSetDefaultProfile(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runGroupsSetDefaultProfile(p *groupsSetDefaultProfileParams, stdout, stderr io.Writer) int {
	if p.Group == "" {
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
		Group          string `json:"group"`
		DefaultProfile string `json:"default_profile"`
	}
	// The daemon validates the profile exists ("" clears the default).
	body := map[string]string{"default_profile": strings.TrimSpace(p.Profile)}
	path := "/v1/groups/" + url.PathEscape(p.Group)
	if err := DaemonRequest(http.MethodPatch, path, body, &resp, DaemonOpts{AskHuman: ask}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if resp.DefaultProfile == "" {
		fmt.Fprintf(stdout, "%s: default profile cleared\n", resp.Group)
	} else {
		fmt.Fprintf(stdout, "%s: default profile set to %s\n", resp.Group, resp.DefaultProfile)
	}
	return rcOK
}

// --- groups set-remote-control ---

type groupsSetRemoteControlParams struct {
	Group    string `pos:"true" help:"Group to configure"`
	Policy   string `pos:"true" optional:"true" help:"Remote-control policy: 'optin' (force Claude Code Remote Access on for this group's agents), 'deny' (force it off, overriding the profile), or 'inherit' (defer to the spawn profile's default). Omit to clear the override (inherit)."`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s'). Capped at 300s. Timeout = deny."`
}

func groupsSetRemoteControlCmd() *cobra.Command {
	return boa.CmdT[groupsSetRemoteControlParams]{
		Use:   "set-remote-control",
		Short: "Set (or clear) a group's remote-control policy",
		Long: "Set the group's remote-control DEFAULT, which overrides a spawn profile's " +
			"remote-control default at spawn (JOH-262): 'optin' defaults Claude Code's built-in " +
			"Remote Access on for agents spawned into the group, 'deny' defaults it off " +
			"(even when the profile defaults it on), and 'inherit' (the default) defers to the " +
			"profile. This is a default, not a lock — an explicit per-spawn value (the dashboard " +
			"checkbox / `agent spawn --remote-control`) wins over it. Omit <policy> to clear back " +
			"to inherit. Codex agents have no Remote Access, so a default-on is silently a no-op " +
			"for them. Gated on the `groups.rename` permission (default human-only).",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *groupsSetRemoteControlParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Group).SetAlternativesFunc(completeGroupNames)
			boa.GetParamT(ctx, &p.Policy).SetAlternatives([]string{"inherit", "optin", "deny"})
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *groupsSetRemoteControlParams, _ *cobra.Command, _ []string) {
			os.Exit(runGroupsSetRemoteControl(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runGroupsSetRemoteControl(p *groupsSetRemoteControlParams, stdout, stderr io.Writer) int {
	if p.Group == "" {
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
	// Omitting the policy clears the override (inherit), mirroring the
	// set-default-profile "omit to clear" convention. The daemon validates the
	// token (inherit|optin|deny).
	policy := strings.TrimSpace(p.Policy)
	if policy == "" {
		policy = "inherit"
	}
	var resp struct {
		Group               string `json:"group"`
		RemoteControlPolicy string `json:"remote_control_policy"`
	}
	body := map[string]string{"remote_control_policy": policy}
	path := "/v1/groups/" + url.PathEscape(p.Group)
	if err := DaemonRequest(http.MethodPatch, path, body, &resp, DaemonOpts{AskHuman: ask}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	fmt.Fprintf(stdout, "%s: remote-control policy set to %s\n", resp.Group, resp.RemoteControlPolicy)
	return rcOK
}

// --- groups set-context ---

type groupsSetContextParams struct {
	Group    string `pos:"true" help:"Group to configure"`
	Context  string `pos:"true" optional:"true" help:"Startup context delivered to the inbox of agents spawned into this group. Omit (and omit --file) to clear it."`
	File     string `long:"file" short:"f" optional:"true" help:"Read the startup context from this file instead of the positional argument ('-' reads stdin). Sidesteps shell quoting — best for long, multi-line, or backtick-containing context. Mutually exclusive with the positional argument."`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s'). Capped at 300s. Timeout = deny."`
}

func groupsSetContextCmd() *cobra.Command {
	return boa.CmdT[groupsSetContextParams]{
		Use:   "set-context",
		Short: "Set (or clear) a group's shared startup context",
		Long: "Set a block of guidance that the daemon delivers to the inbox of every " +
			"agent spawned into this group, as part of its startup briefing. Pass " +
			"the context as the second argument, or with --file to load it from " +
			"a file (--file - reads stdin). The file form is better for long or " +
			"multi-line context and sidesteps shell quoting, including backticks " +
			"the shell would otherwise eat from an inline string. Omit both to " +
			"clear it. Each spawn can still opt out individually (the dashboard's " +
			"'include group default context' checkbox). Gated on the " +
			"`groups.rename` permission (default human-only).",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *groupsSetContextParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Group).SetAlternativesFunc(completeGroupNames)
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *groupsSetContextParams, _ *cobra.Command, _ []string) {
			os.Exit(runGroupsSetContext(p, os.Stdin, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runGroupsSetContext(p *groupsSetContextParams, stdin io.Reader, stdout, stderr io.Writer) int {
	if p.Group == "" {
		fmt.Fprintf(stderr, "Error: group name is required\n")
		return rcInvalidArg
	}
	context, rc := resolveBodyInput(p.Context, p.File, "the context argument", stdin, stderr)
	if rc != rcOK {
		return rc
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
		Group          string `json:"group"`
		DefaultContext string `json:"default_context"`
	}
	body := map[string]string{"default_context": context}
	path := "/v1/groups/" + url.PathEscape(p.Group)
	if err := DaemonRequest(http.MethodPatch, path, body, &resp, DaemonOpts{AskHuman: ask}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if resp.DefaultContext == "" {
		fmt.Fprintf(stdout, "%s: startup context cleared\n", resp.Group)
	} else {
		fmt.Fprintf(stdout, "%s: startup context set (%d chars)\n", resp.Group, len(resp.DefaultContext))
	}
	return rcOK
}

// --- groups set-max-members ---

type groupsSetMaxMembersParams struct {
	Group    string `pos:"true" help:"Group to configure"`
	Max      string `pos:"true" optional:"true" help:"Hard cap on the group's member count. A spawn that would exceed it is refused. Omit (or pass 0) to clear the cap (unlimited)."`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s'). Capped at 300s. Timeout = deny."`
}

func groupsSetMaxMembersCmd() *cobra.Command {
	return boa.CmdT[groupsSetMaxMembersParams]{
		Use:   "set-max-members",
		Short: "Set (or clear) a group's hard member cap",
		Long: "Cap how many members a group may hold. A `tclaude agent spawn` " +
			"that would push the group over the cap is refused with 409 — the " +
			"spawn-guardrail layer that keeps a spawn-capable agent from growing " +
			"a team without bound. The cap is a hard property of the group: it " +
			"applies to every caller, the human included; a human raises it to " +
			"add more. Omit <max> or pass 0 to clear it (unlimited, the " +
			"default). Gated on the `groups.rename` permission (default " +
			"human-only).",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *groupsSetMaxMembersParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Group).SetAlternativesFunc(completeGroupNames)
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *groupsSetMaxMembersParams, _ *cobra.Command, _ []string) {
			os.Exit(runGroupsSetMaxMembers(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runGroupsSetMaxMembers(p *groupsSetMaxMembersParams, stdout, stderr io.Writer) int {
	if p.Group == "" {
		fmt.Fprintf(stderr, "Error: group name is required\n")
		return rcInvalidArg
	}
	// An omitted <max> clears the cap; otherwise parse it. A negative
	// value is rejected here rather than silently clamped, so a typo
	// surfaces at the CLI instead of becoming a confusing "unlimited".
	maxMembers := 0
	if s := strings.TrimSpace(p.Max); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil {
			fmt.Fprintf(stderr, "Error: invalid <max> %q: expected a non-negative integer\n", p.Max)
			return rcInvalidArg
		}
		if n < 0 {
			fmt.Fprintf(stderr, "Error: <max> must be >= 0 (0 clears the cap)\n")
			return rcInvalidArg
		}
		maxMembers = n
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
		Group      string `json:"group"`
		MaxMembers int    `json:"max_members"`
	}
	body := map[string]any{"max_members": maxMembers}
	path := "/v1/groups/" + url.PathEscape(p.Group)
	if err := DaemonRequest(http.MethodPatch, path, body, &resp, DaemonOpts{AskHuman: ask}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if resp.MaxMembers == 0 {
		fmt.Fprintf(stdout, "%s: member cap cleared (unlimited)\n", resp.Group)
	} else {
		fmt.Fprintf(stdout, "%s: member cap set to %d\n", resp.Group, resp.MaxMembers)
	}
	return rcOK
}

type groupsSetNotificationsParams struct {
	Group    string `pos:"true" help:"Group to configure"`
	Mode     string `pos:"true" help:"on = OS notifications for member agents (the default); off = mute the whole group (a per-agent 'on' override still notifies)"`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s'). Capped at 300s. Timeout = deny."`
}

func groupsSetNotificationsCmd() *cobra.Command {
	return boa.CmdT[groupsSetNotificationsParams]{
		Use:   "set-notifications",
		Short: "Mute or unmute OS notifications for a group's agents",
		Long: "Flip the group's OS-notification switch. `off` mutes " +
			"state-transition desktop notifications for every member agent; " +
			"`on` restores the default. A per-agent override (set from the " +
			"dashboard) still wins either way, and the global " +
			"notifications.enabled config toggle sits above both. Gated on " +
			"the `groups.rename` permission (default human-only).",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *groupsSetNotificationsParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Group).SetAlternativesFunc(completeGroupNames)
			boa.GetParamT(ctx, &p.Mode).SetAlternatives([]string{"on", "off"})
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *groupsSetNotificationsParams, _ *cobra.Command, _ []string) {
			os.Exit(runGroupsSetNotifications(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runGroupsSetNotifications(p *groupsSetNotificationsParams, stdout, stderr io.Writer) int {
	if p.Group == "" {
		fmt.Fprintf(stderr, "Error: group name is required\n")
		return rcInvalidArg
	}
	var enabled bool
	switch strings.ToLower(strings.TrimSpace(p.Mode)) {
	case "on":
		enabled = true
	case "off":
		enabled = false
	default:
		fmt.Fprintf(stderr, "Error: invalid mode %q: expected on or off\n", p.Mode)
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
		Group         string `json:"group"`
		NotifyEnabled bool   `json:"notify_enabled"`
	}
	body := map[string]any{"notify_enabled": enabled}
	path := "/v1/groups/" + url.PathEscape(p.Group)
	if err := DaemonRequest(http.MethodPatch, path, body, &resp, DaemonOpts{AskHuman: ask}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if resp.NotifyEnabled {
		fmt.Fprintf(stdout, "%s: OS notifications on\n", resp.Group)
	} else {
		fmt.Fprintf(stdout, "%s: OS notifications muted\n", resp.Group)
	}
	return rcOK
}

func runGroupsArchiveOrUnarchive(name, verb, askHuman string, stdout, stderr io.Writer) int {
	if name == "" {
		fmt.Fprintf(stderr, "Error: group name is required\n")
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
	var resp struct {
		Group  string `json:"group"`
		Action string `json:"action"`
		Note   string `json:"note,omitempty"`
	}
	path := "/v1/groups/" + url.PathEscape(name) + "/" + verb
	if err := DaemonRequest(http.MethodPost, path, nil, &resp, DaemonOpts{AskHuman: ask}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	fmt.Fprintf(stdout, "%s: %s\n", resp.Group, resp.Action)
	if resp.Note != "" {
		fmt.Fprintf(stdout, "  %s\n", resp.Note)
	}
	return rcOK
}
