package agent

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Shell completion helpers for `tclaude agent ...`. Read-only and
// fast: completions fire on every <tab> keystroke, so they bypass
// the daemon and read SQLite directly. The "must go through
// daemon" rule from PR #47 applies to mutations and identity-bearing
// operations; completions are neither.
//
// Each helper has the signature boa's SetAlternativesFunc expects:
// func(*cobra.Command, []string, string) []string. Boa wraps these
// into ShellCompDirectiveDefault internally, so file-completion may
// kick in for empty results — that's acceptable for our case
// (groups/agents always have completable values when they exist).

// completeGroupNames returns every known group name, prefix-filtered.
// Includes archived groups so completion works for verbs that operate
// on them (e.g. `groups unarchive`); the standard listing surface
// filters archived groups out separately.
func completeGroupNames(_ *cobra.Command, _ []string, toComplete string) []string {
	groups, err := db.ListAgentGroups()
	if err != nil {
		return nil
	}
	out := []string{}
	for _, g := range groups {
		if strings.HasPrefix(g.Name, toComplete) {
			out = append(out, g.Name)
		}
	}
	return out
}

// completeTemplateNames returns every group-template name,
// prefix-filtered — for the template-name argument of
// `templates instantiate` (and a model for other template verbs).
func completeTemplateNames(_ *cobra.Command, _ []string, toComplete string) []string {
	templates, err := db.ListGroupTemplates()
	if err != nil {
		return nil
	}
	out := []string{}
	for _, t := range templates {
		if strings.HasPrefix(t.Name, toComplete) {
			out = append(out, t.Name)
		}
	}
	return out
}

// completeArchivedGroupNames returns ONLY archived group names —
// useful for `groups unarchive` where active groups are no-ops and
// shouldn't be tab-suggested.
func completeArchivedGroupNames(_ *cobra.Command, _ []string, toComplete string) []string {
	groups, err := db.ListAgentGroups()
	if err != nil {
		return nil
	}
	out := []string{}
	for _, g := range groups {
		if !g.IsArchived() {
			continue
		}
		if strings.HasPrefix(g.Name, toComplete) {
			out = append(out, g.Name)
		}
	}
	return out
}

// completeConvSelectors returns the 8-char prefix of every known
// conv-id, decorated with `\t<title>` so shells with descriptions
// (zsh, fish) can show context. We don't suggest titles directly
// because they often contain spaces / shell-unfriendly chars; the
// resolver still accepts those at run time.
func completeConvSelectors(_ *cobra.Command, _ []string, toComplete string) []string {
	rows, err := db.ListAllConvIndex()
	if err != nil {
		return nil
	}
	out := []string{}
	seen := map[string]bool{}
	for _, r := range rows {
		if len(r.ConvID) < 8 {
			continue
		}
		short := r.ConvID[:8]
		if seen[short] {
			continue
		}
		if !strings.HasPrefix(short, toComplete) {
			continue
		}
		seen[short] = true
		desc := r.CustomTitle
		if desc == "" {
			desc = r.Summary
		}
		if desc == "" {
			desc = r.FirstPrompt
		}
		desc = sanitizeDesc(desc)
		if desc != "" {
			out = append(out, short+"\t"+desc)
		} else {
			out = append(out, short)
		}
	}
	return out
}

// completeRoles returns the distinct non-empty role values across all
// group memberships, prefix-filtered. Used by `agent message --role`.
// The membership table is small (humans curate it), so the per-group
// scan is cheap enough for a completion path.
func completeRoles(_ *cobra.Command, _ []string, toComplete string) []string {
	groups, err := db.ListAgentGroups()
	if err != nil {
		return nil
	}
	// Match the case-insensitive delivery semantics — a lowercase
	// `--role po` must still complete a stored role `PO`.
	needle := strings.ToLower(strings.TrimSpace(toComplete))
	seen := map[string]bool{}
	out := []string{}
	for _, g := range groups {
		members, err := db.ListAgentGroupMembers(g.ID)
		if err != nil {
			continue
		}
		for _, m := range members {
			role := strings.TrimSpace(m.Role)
			key := strings.ToLower(role)
			if role == "" || seen[key] || !strings.HasPrefix(key, needle) {
				continue
			}
			seen[key] = true
			out = append(out, role)
		}
	}
	return out
}

// completeMessageTargets is `completeConvSelectors` plus the
// `group:<name>` multicast prefix. Used for `agent message <peer>`.
func completeMessageTargets(cmd *cobra.Command, args []string, toComplete string) []string {
	out := []string{}
	groups, _ := db.ListAgentGroups()
	if strings.HasPrefix(toComplete, "group:") {
		// User typed the multicast prefix already — only offer groups.
		groupPart := strings.TrimPrefix(toComplete, "group:")
		for _, g := range groups {
			if strings.HasPrefix(g.Name, groupPart) {
				out = append(out, "group:"+g.Name)
			}
		}
		return out
	}
	// Empty / partial: offer the prefix scaffolding plus conv-ids.
	if strings.HasPrefix("group:", toComplete) {
		for _, g := range groups {
			out = append(out, "group:"+g.Name)
		}
	}
	out = append(out, completeConvSelectors(cmd, args, toComplete)...)
	return out
}

// completePermissionSlugs returns the slug registry. Tries the
// daemon first (so future builds that add slugs surface immediately
// without a CLI rebuild), falls back to the in-process list.
func completePermissionSlugs(_ *cobra.Command, _ []string, toComplete string) []string {
	slugs := fetchSlugsFromDaemon()
	if len(slugs) == 0 {
		// Keep in sync with pkg/claude/agentd/permissions.go's registry.
		slugs = []slugEntry{
			{"self.rename", "Rename own conversation"},
			{"self.compact", "Compact own conversation"},
			{"self.reincarnate", "Replace self with fresh successor"},
			{"self.clone", "Fork self into a sibling"},
			{"agent.reincarnate", "Reincarnate ANOTHER agent"},
			{"agent.compact", "Compact ANOTHER agent"},
			{"agent.rename", "Rename ANOTHER agent"},
			{"agent.clone", "Clone ANOTHER agent"},
			{"groups.create", "Create new groups"},
			{"groups.rm", "Delete groups"},
			{"groups.stop", "Stop a group's running members"},
			{"groups.resume", "Resume a group's offline members"},
			{"member.add", "Add members to a group"},
			{"member.remove", "Remove members from a group"},
			{"member.redesignate", "Edit role/descr on group members"},
			{"permissions.grant", "Grant agent permissions"},
			{"permissions.revoke", "Revoke agent permissions"},
		}
	}
	out := []string{}
	for _, s := range slugs {
		if !strings.HasPrefix(s.Slug, toComplete) {
			continue
		}
		if s.Description != "" {
			out = append(out, s.Slug+"\t"+sanitizeDesc(s.Description))
		} else {
			out = append(out, s.Slug)
		}
	}
	return out
}

// completePermissionTargets returns "default" + every conv selector,
// matching the shape `permissions grant|revoke` accepts.
func completePermissionTargets(cmd *cobra.Command, args []string, toComplete string) []string {
	out := []string{}
	if strings.HasPrefix("default", toComplete) {
		out = append(out, "default\tmodify the global defaults list")
	}
	out = append(out, completeConvSelectors(cmd, args, toComplete)...)
	return out
}

// completeInboxMessageIDs returns the most-recent N message IDs in
// the caller's inbox, with sender + subject as the description. Used
// by `inbox read <id>` and `reply <id>`.
//
// The caller's conv-id is resolved the same way every CLI command
// resolves it: CC's per-pid session file first, then $TCLAUDE_SESSION_ID.
// Fails silently to no-completion if neither is available — this is a
// completion path, not a control flow error.
func completeInboxMessageIDs(_ *cobra.Command, _ []string, toComplete string) []string {
	myID, err := currentConvID()
	if err != nil || myID == "" {
		return nil
	}
	msgs, err := db.ListAgentMessagesForConv(myID, 50)
	if err != nil {
		return nil
	}
	out := []string{}
	for _, m := range msgs {
		idStr := strconv.FormatInt(m.ID, 10)
		if !strings.HasPrefix(idStr, toComplete) {
			continue
		}
		desc := "from " + m.FromConv
		if len(m.FromConv) >= 8 {
			desc = "from " + m.FromConv[:8]
		}
		if m.Subject != "" {
			desc += " — " + m.Subject
		}
		desc = sanitizeDesc(desc)
		out = append(out, idStr+"\t"+desc)
	}
	return out
}

// completeAskHumanDurations offers a few common values for the
// --ask-human flag. The flag accepts any duration string, so this
// is just a convenience hint — boa won't validate against this list
// since we never call SetStrictAlts.
func completeAskHumanDurations(_ *cobra.Command, _ []string, toComplete string) []string {
	suggestions := []string{"15s", "30s", "60s", "2m", "5m"}
	out := []string{}
	for _, s := range suggestions {
		if strings.HasPrefix(s, toComplete) {
			out = append(out, s)
		}
	}
	return out
}

// slugEntry mirrors /v1/permissions/slugs's response shape.
type slugEntry struct {
	Slug        string `json:"slug"`
	Description string `json:"description"`
}

// fetchSlugsFromDaemon does a 250ms GET on /v1/permissions/slugs.
// Returns nil on any failure so callers fall back to a static list.
// Bypasses the normal daemon-required gate to keep completion
// silent when the daemon isn't running.
func fetchSlugsFromDaemon() []slugEntry {
	sock := SocketPath()
	if sock == "" {
		return nil
	}
	client := &http.Client{
		Timeout: 250 * time.Millisecond,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		},
	}
	resp, err := client.Get("http://_/v1/permissions/slugs")
	if err != nil {
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var out []slugEntry
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil
	}
	return out
}

// sanitizeDesc keeps descriptions on a single line and bounds their
// length so long titles don't blow out the completion column.
func sanitizeDesc(s string) string {
	s = strings.ReplaceAll(s, "\t", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", "")
	if len(s) > 60 {
		s = s[:57] + "..."
	}
	return s
}
