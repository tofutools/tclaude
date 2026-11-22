package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// WhoamiFull is the daemon's GET /v1/whoami/full reply.
type WhoamiFull struct {
	IsHuman bool   `json:"is_human"`
	AgentID string `json:"agent_id"`
	ConvID  string `json:"conv_id"`
	Title   string `json:"title"`
	TaskRef *struct {
		URL   string `json:"url"`
		Label string `json:"label"`
	} `json:"task_ref"`
	Spawn struct {
		Profile        string `json:"profile"`
		SandboxProfile string `json:"sandbox_profile"`
		Branch         string `json:"branch"`
		StartupDir     string `json:"startup_dir"`
		CurrentDir     string `json:"current_dir"`
		WorktreePath   string `json:"worktree_path"`
		WorktreeBranch string `json:"worktree_branch"`
	} `json:"spawn"`
	Groups []struct {
		Name     string `json:"name"`
		Member   bool   `json:"member"`
		Owner    bool   `json:"owner"`
		Archived bool   `json:"archived"`
		Role     string `json:"role"`
		Descr    string `json:"descr"`
		LinksIn  []struct {
			Peer string `json:"peer"`
			From string `json:"from"`
			To   string `json:"to"`
			Mode string `json:"mode"`
		} `json:"links_in"`
	} `json:"groups"`
	Startup struct {
		SpawnGroup          string `json:"spawn_group"`
		SpawnedBy           string `json:"spawned_by"`
		IncludeGroupContext bool   `json:"include_group_context"`
		ProfileContext      string `json:"profile_context"`
		BriefMessageID      int64  `json:"brief_message_id"`
		Brief               string `json:"brief"`
		Recorded            bool   `json:"recorded"`
	} `json:"startup"`
	Reinject *struct {
		Group           string `json:"group"`
		Policy          string `json:"policy"`
		GroupContext    string `json:"group_context"`
		AfterCompaction string `json:"after_compaction"`
	} `json:"reinject"`
	Permissions struct {
		Effective    []string            `json:"effective"`
		Provenance   map[string]string   `json:"provenance"`
		Source       string              `json:"source"`
		OwnedGroups  []string            `json:"owned_groups"`
		MemberGroups map[string][]string `json:"member_groups"`
	} `json:"permissions"`
}

// RunWhoamiFull prints the caller's whole setup from GET /v1/whoami/full.
func RunWhoamiFull(jsonOut bool, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var raw json.RawMessage
	if err := DaemonGet("/v1/whoami/full", &raw); err != nil {
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
	var me WhoamiFull
	if err := json.Unmarshal(raw, &me); err != nil {
		fmt.Fprintf(stderr, "Error: decode whoami: %v\n", err)
		return rcIOFailure
	}
	if me.IsHuman {
		fmt.Fprintln(stdout, HumanIdentity)
		return rcOK
	}
	printWhoamiFull(stdout, me)
	return rcOK
}

func printWhoamiFull(w io.Writer, me WhoamiFull) {
	line := func(label, value string) {
		if value != "" {
			fmt.Fprintf(w, "%-22s %s\n", label+":", value)
		}
	}
	fmt.Fprintf(w, "%s\t%s\n", firstNonEmptyStr(me.AgentID, me.ConvID), me.Title)
	if me.TaskRef != nil {
		line("task", strings.TrimSpace(me.TaskRef.Label+" "+me.TaskRef.URL))
	}
	line("spawn profile (named)", me.Spawn.Profile)
	line("sandbox profile (named)", me.Spawn.SandboxProfile)
	line("startup dir", me.Spawn.StartupDir)
	line("current dir", me.Spawn.CurrentDir)
	line("branch", me.Spawn.Branch)
	line("worktree", strings.TrimSpace(me.Spawn.WorktreePath+" "+me.Spawn.WorktreeBranch))

	fmt.Fprintln(w, "\nGroups:")
	if len(me.Groups) == 0 {
		fmt.Fprintln(w, "  (none)")
	}
	for _, g := range me.Groups {
		var tags []string
		if g.Member {
			tags = append(tags, "member")
		}
		if g.Owner {
			tags = append(tags, "owner")
		}
		if g.Archived {
			tags = append(tags, "archived")
		}
		fmt.Fprintf(w, "  %s (%s)", g.Name, strings.Join(tags, ", "))
		if g.Role != "" {
			fmt.Fprintf(w, "  role=%s", g.Role)
		}
		if g.Descr != "" {
			fmt.Fprintf(w, "  descr=%q", g.Descr)
		}
		fmt.Fprintln(w)
		for _, l := range g.LinksIn {
			from := l.From
			if l.Peer != "" {
				from += "@" + l.Peer
			}
			fmt.Fprintf(w, "    reached from %s (%s)\n", from, l.Mode)
		}
	}

	fmt.Fprintln(w, "\nStartup:")
	if !me.Startup.Recorded {
		fmt.Fprintln(w, "  (no startup record: spawned before tclaude kept one, or not spawned)")
	} else {
		line("  spawn group", me.Startup.SpawnGroup)
		spawner := me.Startup.SpawnedBy
		if spawner == "" {
			spawner = "the human"
		}
		line("  spawned by", spawner)
		groupContext := "included"
		if !me.Startup.IncludeGroupContext {
			groupContext = "opted out"
		}
		line("  group context", groupContext)
		if me.Startup.BriefMessageID > 0 {
			line("  briefing", fmt.Sprintf("inbox message #%d", me.Startup.BriefMessageID))
		}
		if me.Startup.Brief != "" {
			fmt.Fprintln(w, "  ---")
			for _, l := range strings.Split(strings.TrimRight(me.Startup.Brief, "\n"), "\n") {
				fmt.Fprintln(w, "  "+l)
			}
			fmt.Fprintln(w, "  ---")
		}
	}
	if r := me.Reinject; r != nil {
		line("reinject", fmt.Sprintf("%s (group %s)", r.Policy, r.Group))
		if r.AfterCompaction == "" {
			fmt.Fprintln(w, "  nothing is re-injected after a compaction")
		} else {
			fmt.Fprintln(w, "  after a compaction, tclaude re-injects:")
			fmt.Fprintln(w, "  ---")
			for _, l := range strings.Split(strings.TrimRight(r.AfterCompaction, "\n"), "\n") {
				fmt.Fprintln(w, "  "+l)
			}
			fmt.Fprintln(w, "  ---")
		}
	}

	fmt.Fprintln(w, "\nPermissions:")
	line("  source", me.Permissions.Source)
	slugs := append([]string(nil), me.Permissions.Effective...)
	sort.Strings(slugs)
	for _, slug := range slugs {
		from := me.Permissions.Provenance[slug]
		if groups := me.Permissions.MemberGroups[slug]; len(groups) > 0 {
			member := "member:" + strings.Join(groups, ",")
			if from == "" || from == "member:group" {
				from = member
			} else {
				from += " + " + member
			}
		}
		fmt.Fprintf(w, "  %-40s %s\n", slug, from)
	}
}
