package agent

import (
	"bytes"
	"strings"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// TestRenderPermissionsState_LeadsWithAgentID locks in the JOH-325 (D3)
// display cleanup: the per-agent overrides roster leads its ID column with
// the stable agent_id projected onto each conv key (state.AgentIDs), not
// the conv-id prefix. Storage was already agent-keyed — this is the
// display half.
func TestRenderPermissionsState_LeadsWithAgentID(t *testing.T) {
	// Hermetic: titles now arrive on the wire (state.Titles), so this
	// renderer touches no DB at all — point HOME at a fresh temp store
	// anyway so a regression back to a local lookup can't reach the real
	// ~/.tclaude.
	t.Setenv("HOME", t.TempDir())
	db.ResetForTest()

	const conv = "11112222-3333-4444-5555-666677778888"
	const agentID = "agt_032fdfcfbb0578a5a1cf6493db7264fb"

	t.Run("known agent_id leads the ID column", func(t *testing.T) {
		state := permissionsState{
			Defaults:  []string{"groups.create"},
			Overrides: map[string]map[string]string{conv: {"groups.members.spawn": "grant", "human.notify": "deny"}},
			AgentIDs:  map[string]string{conv: agentID},
		}
		var buf bytes.Buffer
		if rc := renderPermissionsState(state, &buf); rc != rcOK {
			t.Fatalf("renderPermissionsState rc = %d, want %d", rc, rcOK)
		}
		out := buf.String()
		if want := agentID[:12]; !strings.Contains(out, want) {
			t.Errorf("roster must lead with the short agent_id %q; got:\n%s", want, out)
		}
		if convPrefix := conv[:8]; strings.Contains(out, convPrefix) {
			t.Errorf("roster must not show the conv prefix %q in the ID column when an agent_id is known; got:\n%s", convPrefix, out)
		}
	})

	t.Run("missing agent_id falls back to the conv prefix", func(t *testing.T) {
		state := permissionsState{
			Overrides: map[string]map[string]string{conv: {"groups.members.spawn": "grant"}},
			// AgentIDs intentionally empty — the daemon couldn't project one.
		}
		var buf bytes.Buffer
		if rc := renderPermissionsState(state, &buf); rc != rcOK {
			t.Fatalf("renderPermissionsState rc = %d, want %d", rc, rcOK)
		}
		out := buf.String()
		if convPrefix := conv[:8]; !strings.Contains(out, convPrefix) {
			t.Errorf("roster must fall back to the conv prefix %q when no agent_id is known; got:\n%s", convPrefix, out)
		}
	})
}

// TestPermSourceNote_OwnerScope pins ordinary scoped owner-grant provenance.
func TestPermSourceNote_OwnerScope(t *testing.T) {
	owned := []string{"dev", "qa"}

	for _, tc := range []struct {
		name   string
		source string
		owned  []string
		want   string
	}{
		{
			name:   "group-scoped owner grant displays its ordinary scope",
			source: "owner [group=dev]",
			owned:  owned,
			want:   "(via ownership; scope: [group=dev])",
		},
		{
			// An older daemon sends bare "owner" with no scope.
			name:   "older daemon keeps the unscoped wording",
			source: "owner",
			owned:  nil,
			want:   "(via ownership)",
		},
		{
			// Scope known but no groups to name — say less, not more.
			name:   "no owned groups falls back rather than inventing a scope",
			source: "owner:group",
			owned:  nil,
			want:   "(via ownership)",
		},
		{
			// A NEWER daemon could invent a fourth scope. Guessing the
			// narrower group phrasing would misreport it — exactly the
			// failure this change exists to prevent.
			name:   "an unknown scope falls back to the unscoped wording",
			source: "owner:fleet",
			owned:  owned,
			want:   "(via ownership)",
		},
		{
			name:   "a plain default stays unannotated",
			source: "default",
			owned:  owned,
			want:   "",
		},
		{
			name:   "sudo is called out as temporary",
			source: "sudo",
			owned:  nil,
			want:   "(via sudo elevation)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := permSourceNote(tc.source, false, tc.owned); got != tc.want {
				t.Errorf("permSourceNote(%q, false, %v) = %q, want %q",
					tc.source, tc.owned, got, tc.want)
			}
		})
	}
}

// TestOwnerScopeCell pins the OWNER GRANT column of `permissions slugs`.
func TestOwnerScopeCell(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   permSlugEntry
		want string
	}{
		{"group scope", permSlugEntry{OwnerImplied: true, ScopeDims: []string{"group"}}, "✔ owned groups"},
		{"global", permSlugEntry{OwnerImplied: true}, "✔ global"},
		{"no owner bypass", permSlugEntry{}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ownerScopeCell(tc.in); got != tc.want {
				t.Errorf("ownerScopeCell(%+v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestPermissionScopeFlagAndDisplay(t *testing.T) {
	scope, err := parsePermissionScopeFlags([]string{
		"spawn_profile=reviewer,locked",
		"group=dev",
		"spawn_profile=locked",
	})
	if err != nil {
		t.Fatalf("parsePermissionScopeFlags: %v", err)
	}
	if got, want := renderPermissionScope(scope), "group=dev spawn_profile=locked,reviewer"; got != want {
		t.Errorf("renderPermissionScope = %q, want %q", got, want)
	}
	if got, want := permSourceNote("override [group=dev spawn_profile=locked,reviewer]", false, nil),
		"(via agent grant; scope: [group=dev spawn_profile=locked,reviewer])"; got != want {
		t.Errorf("permSourceNote scoped = %q, want %q", got, want)
	}

	for _, bad := range []string{"group", "=dev", "group=", "group=dev,"} {
		if _, err := parsePermissionScopeFlags([]string{bad}); err == nil {
			t.Errorf("parsePermissionScopeFlags(%q) succeeded, want error", bad)
		}
	}
}

func TestRenderPermissionsStateShowsScopes(t *testing.T) {
	const conv = "scope-1111-2222-3333"
	state := permissionsState{
		Overrides: map[string]map[string]string{conv: {"groups.members.spawn": "grant"}},
		Scopes: map[string]map[string]permissionScope{conv: {
			"groups.members.spawn": {"group": {"dev"}, "spawn_profile": {"locked"}},
		}},
	}
	var out bytes.Buffer
	if rc := renderPermissionsState(state, &out); rc != rcOK {
		t.Fatalf("renderPermissionsState rc = %d", rc)
	}
	if want := "groups.members.spawn [group=dev spawn_profile=locked]"; !strings.Contains(out.String(), want) {
		t.Errorf("scope missing from roster; want %q in:\n%s", want, out.String())
	}
}
