package agentd

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/common/groupexport"
)

func TestPermissionRegistryScopeDeclarations(t *testing.T) {
	want := map[string][]ScopeDim{
		PermGroupsSpawn:       {ScopeDimGroup, ScopeDimSpawnProfile},
		PermProcessRunsManage: {ScopeDimProcessTemplate},
		PermAgentRetire:       {ScopeDimGroup, ScopeDimTargetAgent},
		PermAgentStanddown:    {ScopeDimGroup, ScopeDimTargetAgent},
		PermRoutesPublish:     {ScopeDimGroup},
		PermRoutesConsume:     {ScopeDimGroup},
	}
	for _, entry := range permissionRegistry {
		if dims, ok := want[entry.Slug]; ok {
			if strings.Join(scopeDimStrings(entry.ScopeDims), ",") != strings.Join(scopeDimStrings(dims), ",") {
				t.Errorf("scope dims for %s = %v, want %v", entry.Slug, entry.ScopeDims, dims)
			}
			delete(want, entry.Slug)
		}
	}
	for slug := range want {
		t.Errorf("scope declaration missing for %s", slug)
	}
}

func TestPermissionRegistryRejectsInconsistentScopeDeclarations(t *testing.T) {
	for _, tc := range []struct {
		name string
		reg  []PermSlug
	}{
		{"unknown dimension", []PermSlug{{Slug: "test", ScopeDims: []ScopeDim{"mystery"}}}},
		{"duplicate dimension", []PermSlug{{Slug: "test", ScopeDims: []ScopeDim{ScopeDimGroup, ScopeDimGroup}}}},
		{"duplicate slug", []PermSlug{{Slug: "test"}, {Slug: "test"}}},
		{"empty slug", []PermSlug{{Slug: ""}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("initPermissionRegistryEntries did not panic")
				}
			}()
			initPermissionRegistryEntries(tc.reg)
		})
	}
}

func TestPermissionScopeParseAndValidate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		raw     string
		slug    string
		want    string
		wantErr string
	}{
		{"absent", "", PermGroupsSpawn, "", ""},
		{"empty object", `{}`, PermGroupsSpawn, "", ""},
		{"exact and canonical", `{"spawn_profile":["p2","p1","p1"],"group":["dev"]}`, PermGroupsSpawn, `{"group":["dev"],"spawn_profile":["p1","p2"]}`, ""},
		{"reserved selector", `{"target_agent":["@descendants","@self-spawned"]}`, PermAgentRetire, `{"target_agent":["@descendants","@self-spawned"]}`, ""},
		{"undeclared dimension", `{"group":["dev"]}`, PermProcessRunsManage, "", "does not declare"},
		{"unknown dimension", `{"remote":["origin"]}`, PermGroupsSpawn, "", "unknown permission scope dimension"},
		{"dimension whitespace", `{" group":["dev"]}`, PermGroupsSpawn, "", "surrounding whitespace"},
		{"unknown selector", `{"target_agent":["@parent"]}`, PermAgentRetire, "", "unknown selector"},
		{"selector on wrong dimension", `{"group":["@descendants"]}`, PermGroupsSpawn, "", "unknown selector"},
		{"control character", `{"group":["dev\nadmin"]}`, PermGroupsSpawn, "", "control characters"},
		{"empty matcher list", `{"group":[]}`, PermGroupsSpawn, "", "at least one matcher"},
		{"empty matcher", `{"group":[""]}`, PermGroupsSpawn, "", "empty matcher"},
		{"not an object", `[]`, PermGroupsSpawn, "", "must be a JSON object"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scope, canonical, err := parsePermissionScope(json.RawMessage(tc.raw))
			if err == nil {
				err = validatePermissionScopeForSlug(tc.slug, scope)
			}
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parse/validate: %v", err)
			}
			if canonical != tc.want {
				t.Errorf("canonical = %q, want %q", canonical, tc.want)
			}
		})
	}
}

func TestCanonicalizeImportedPermissionScopesCoversAllGrantTables(t *testing.T) {
	const raw = `{"group":["ops","dev","dev"]}`
	const want = `{"group":["dev","ops"]}`
	exp := &groupexport.Export{
		Group: groupexport.Group{Permissions: []groupexport.GroupPermission{{
			Slug: PermGroupsSpawn, ScopeJSON: raw,
		}}},
		Permissions: []groupexport.Permission{{
			ConvID: "conv", Slug: PermGroupsSpawn, Effect: "grant", ScopeJSON: raw,
		}},
		SudoGrants: []groupexport.SudoGrant{{
			ConvID: "conv", Slug: PermGroupsSpawn, ScopeJSON: raw,
		}},
	}
	if err := canonicalizeImportedPermissionScopes(exp); err != nil {
		t.Fatalf("canonicalizeImportedPermissionScopes: %v", err)
	}
	if got := exp.Group.Permissions[0].ScopeJSON; got != want {
		t.Errorf("group permission scope = %s, want %s", got, want)
	}
	if got := exp.Permissions[0].ScopeJSON; got != want {
		t.Errorf("permanent permission scope = %s, want %s", got, want)
	}
	if got := exp.SudoGrants[0].ScopeJSON; got != want {
		t.Errorf("sudo grant scope = %s, want %s", got, want)
	}
}

func TestPermissionScopeRejectsOversizePayload(t *testing.T) {
	raw := json.RawMessage(`{"group":["` + strings.Repeat("x", permissionScopeMaxJSONBytes) + `"]}`)
	_, _, err := parsePermissionScope(raw)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("error = %v, want oversize rejection", err)
	}
}

func scopeDimStrings(dims []ScopeDim) []string {
	out := make([]string, len(dims))
	for i, dim := range dims {
		out[i] = string(dim)
	}
	return out
}
