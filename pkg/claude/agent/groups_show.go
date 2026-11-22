package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"slices"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"

	"github.com/tofutools/tclaude/pkg/common"
)

type groupsShowParams struct {
	Name string `pos:"true" help:"Group name"`
	JSON bool   `long:"json" help:"Output JSON"`
}

func groupsShowCmd() *cobra.Command {
	return boa.CmdT[groupsShowParams]{
		Use:   "show",
		Short: "Show a group: its summary, startup context and settings",
		Long: "Shows what the groups set-* commands write. The summary (description, parent, attachment, owners, " +
			"members with role and description, links in and out) is as open as groups ls and groups members. " +
			"The startup context (its text and hash, saved prompts and reinject policy) needs groups.context.read, " +
			"which membership and ownership of the group confer. The settings (default directory, spawn and sandbox " +
			"profile, environment names but never values, member cap, notifications, remote-control policy, owner " +
			"scopes, default spawn target, taskboard writers and the group's grants) need groups.settings.read, which " +
			"ownership confers. A section the caller cannot read is left out and named, with the slug it needs. Read only.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *groupsShowParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Name).SetAlternativesFunc(completeGroupNames)
			return nil
		},
		RunFunc: func(p *groupsShowParams, _ *cobra.Command, _ []string) {
			os.Exit(RunGroupsShow(p.Name, p.JSON, cliStdout, cliStderr))
		},
	}.ToCobra()
}

// GroupShow is the daemon's GET /v1/groups/{name} reply.
type GroupShow struct {
	Name       string `json:"name"`
	Descr      string `json:"descr,omitempty"`
	Archived   bool   `json:"archived,omitempty"`
	Parent     string `json:"parent,omitempty"`
	Attachment struct {
		URL   string `json:"attachment_url,omitempty"`
		Label string `json:"attachment_label,omitempty"`
	} `json:"attachment"`
	Mission        string        `json:"mission,omitempty"`
	SourceTemplate string        `json:"source_template,omitempty"`
	Owners         []ownerEntry  `json:"owners"`
	Members        []memberEntry `json:"members"`
	Links          []struct {
		Peer string `json:"peer,omitempty"`
		From string `json:"from"`
		To   string `json:"to"`
		Mode string `json:"mode"`
	} `json:"links"`
	Context *struct {
		Text                 string   `json:"text"`
		Hash                 string   `json:"hash"`
		DefaultPrompts       []string `json:"default_prompts,omitempty"`
		ReinjectAfterCompact string   `json:"reinject_after_compact"`
	} `json:"context,omitempty"`
	Settings *struct {
		DefaultCwd          string                     `json:"default_cwd,omitempty"`
		DefaultProfile      string                     `json:"default_profile,omitempty"`
		SandboxProfile      string                     `json:"sandbox_profile,omitempty"`
		EnvironmentKeys     []string                   `json:"environment_keys,omitempty"`
		MaxMembers          int                        `json:"max_members,omitempty"`
		NotifyEnabled       bool                       `json:"notify_enabled"`
		RemoteControlPolicy string                     `json:"remote_control_policy"`
		OwnerScopes         map[string]json.RawMessage `json:"owner_scopes,omitempty"`
		DefaultSpawnGroup   bool                       `json:"default_spawn_group,omitempty"`
		TaskboardLocked     bool                       `json:"taskboard_locked,omitempty"`
		TaskboardWriters    []string                   `json:"taskboard_writers,omitempty"`
		Permissions         []string                   `json:"permissions,omitempty"`
		PermissionScopes    map[string]json.RawMessage `json:"permission_scopes,omitempty"`
	} `json:"settings,omitempty"`
	Withheld []struct {
		Section string `json:"section"`
		Slug    string `json:"slug"`
	} `json:"withheld,omitempty"`
}

// RunGroupsShow prints one group as GET /v1/groups/{name} returns it.
func RunGroupsShow(name string, jsonOut bool, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var raw json.RawMessage
	if err := DaemonGet("/v1/groups/"+url.PathEscape(name), &raw); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if jsonOut {
		var pretty any
		if err := json.Unmarshal(raw, &pretty); err != nil {
			return rcIOFailure
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(pretty); err != nil {
			return rcIOFailure
		}
		return rcOK
	}
	var g GroupShow
	if err := json.Unmarshal(raw, &g); err != nil {
		fmt.Fprintf(stderr, "Error: decode group: %v\n", err)
		return rcIOFailure
	}
	printGroupShow(stdout, g)
	return rcOK
}

func printGroupShow(w io.Writer, g GroupShow) {
	line := func(label, value string) {
		if value != "" {
			fmt.Fprintf(w, "%-22s %s\n", label+":", value)
		}
	}
	fmt.Fprintf(w, "Group %s", g.Name)
	if g.Archived {
		fmt.Fprint(w, " (archived)")
	}
	fmt.Fprintln(w)
	line("description", g.Descr)
	line("parent", g.Parent)
	if g.Attachment.URL != "" {
		line("attachment", strings.TrimSpace(g.Attachment.Label+" "+g.Attachment.URL))
	}
	line("mission", g.Mission)
	line("source template", g.SourceTemplate)

	fmt.Fprintln(w, "\nOwners:")
	if len(g.Owners) == 0 {
		fmt.Fprintln(w, "  (none)")
	}
	for _, o := range g.Owners {
		fmt.Fprintf(w, "  %s  %s\n", firstNonEmptyStr(o.AgentID, o.ConvID), o.Title)
	}
	fmt.Fprintln(w, "\nMembers:")
	if len(g.Members) == 0 {
		fmt.Fprintln(w, "  (none)")
	}
	for _, m := range g.Members {
		fmt.Fprintf(w, "  %s  %s", firstNonEmptyStr(m.AgentID, m.ConvID), m.Title)
		if m.Role != "" {
			fmt.Fprintf(w, "  role=%s", m.Role)
		}
		if m.Descr != "" {
			fmt.Fprintf(w, "  descr=%q", m.Descr)
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintln(w, "\nLinks:")
	if len(g.Links) == 0 {
		fmt.Fprintln(w, "  (none)")
	}
	for _, l := range g.Links {
		peer := ""
		if l.Peer != "" {
			peer = "  @" + l.Peer
		}
		fmt.Fprintf(w, "  %s -> %s  (%s)%s\n", l.From, l.To, l.Mode, peer)
	}

	if c := g.Context; c != nil {
		fmt.Fprintln(w, "\nStartup context:")
		line("  hash", c.Hash)
		line("  reinject", c.ReinjectAfterCompact)
		line("  saved prompts", strings.Join(c.DefaultPrompts, ", "))
		if c.Text == "" {
			fmt.Fprintln(w, "  (empty)")
		} else {
			fmt.Fprintln(w, "  ---")
			for _, l := range strings.Split(strings.TrimRight(c.Text, "\n"), "\n") {
				fmt.Fprintln(w, "  "+l)
			}
			fmt.Fprintln(w, "  ---")
		}
	}
	if s := g.Settings; s != nil {
		fmt.Fprintln(w, "\nSettings:")
		line("  default dir", s.DefaultCwd)
		line("  default profile", s.DefaultProfile)
		line("  sandbox profile", s.SandboxProfile)
		line("  environment (names)", strings.Join(s.EnvironmentKeys, ", "))
		if s.MaxMembers > 0 {
			line("  max members", fmt.Sprint(s.MaxMembers))
		}
		notify := "on"
		if !s.NotifyEnabled {
			notify = "muted"
		}
		line("  notifications", notify)
		line("  remote control", s.RemoteControlPolicy)
		if s.DefaultSpawnGroup {
			line("  default spawn target", "yes")
		}
		if len(s.OwnerScopes) > 0 {
			scopes := make([]string, 0, len(s.OwnerScopes))
			for slug, scope := range s.OwnerScopes {
				scopes = append(scopes, slug+"="+string(scope))
			}
			line("  owner scopes", strings.Join(sortStrings(scopes), "; "))
		}
		if s.TaskboardLocked {
			line("  taskboard writers", strings.Join(s.TaskboardWriters, ", ")+" (locked)")
		}
		grants := make([]string, 0, len(s.Permissions))
		for _, slug := range s.Permissions {
			if scope, ok := s.PermissionScopes[slug]; ok {
				slug += " " + string(scope)
			}
			grants = append(grants, slug)
		}
		line("  grants", strings.Join(grants, ", "))
	}
	if len(g.Withheld) > 0 {
		fmt.Fprintln(w)
		for _, h := range g.Withheld {
			fmt.Fprintf(w, "withheld: %s (needs %s)\n", h.Section, h.Slug)
		}
	}
}

func sortStrings(in []string) []string {
	slices.Sort(in)
	return in
}
