package agentd

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/common/groupexport"
)

func TestPermissionRegistryScopeDeclarations(t *testing.T) {
	want := map[string][]ScopeDim{
		PermAgentSpawn:         {ScopeDimGroup, ScopeDimSpawnProfile, ScopeDimSandboxProfile},
		PermGroupsMembersSpawn: {ScopeDimGroup, ScopeDimSpawnProfile, ScopeDimSandboxProfile},
		PermProcessRunsManage:  {ScopeDimProcessTemplate},
		PermAgentRetire:        {ScopeDimGroup, ScopeDimTargetAgent},
		PermAgentStanddown:     {ScopeDimGroup, ScopeDimTargetAgent},
		PermRoutesPublish:      {ScopeDimGroup},
		PermRoutesConsume:      {ScopeDimGroup},
		PermGitRead:            {ScopeDimRemote},
		PermGitPush:            {ScopeDimRemote},
		PermGitHubRead:         {ScopeDimRemote},
		PermGitHubWrite:        {ScopeDimRemote},
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
		{"absent", "", PermGroupsMembersSpawn, "", ""},
		{"empty object", `{}`, PermGroupsMembersSpawn, "", ""},
		{"exact and canonical", `{"spawn_profile":["p2","p1","p1"],"group":["dev"]}`, PermGroupsMembersSpawn, `{"group":["dev"],"spawn_profile":["p1","p2"]}`, ""},
		{"reserved selector", `{"target_agent":["@descendants","@self-spawned"]}`, PermAgentRetire, `{"target_agent":["@descendants","@self-spawned"]}`, ""},
		{"undeclared dimension", `{"group":["dev"]}`, PermProcessRunsManage, "", "does not declare"},
		{"remote pattern", `{"remote":["github.com/tofutools/*"]}`, PermGitPush, `{"remote":["github.com/tofutools/*"]}`, ""},
		{"undeclared remote dimension", `{"remote":["origin"]}`, PermGroupsMembersSpawn, "", "does not declare"},
		{"dimension whitespace", `{" group":["dev"]}`, PermGroupsMembersSpawn, "", "surrounding whitespace"},
		{"unknown selector", `{"target_agent":["@parent"]}`, PermAgentRetire, "", "unknown selector"},
		{"selector on wrong dimension", `{"group":["@descendants"]}`, PermGroupsMembersSpawn, "", "unknown selector"},
		{"control character", `{"group":["dev\nadmin"]}`, PermGroupsMembersSpawn, "", "control characters"},
		{"empty matcher list", `{"group":[]}`, PermGroupsMembersSpawn, "", "at least one matcher"},
		{"empty matcher", `{"group":[""]}`, PermGroupsMembersSpawn, "", "empty matcher"},
		{"not an object", `[]`, PermGroupsMembersSpawn, "", "must be a JSON object"},
		{"linear team key", `{"linear_team":["JOH","TCL"]}`, PermLinearRead, `{"linear_team":["JOH","TCL"]}`, ""},
		// A team key is matched WHOLE, so a matcher that cannot be one would
		// store and render as a narrow grant while matching nothing: every
		// single-issue verb refused, every listing empty. That silence is worse
		// than an error, so the shape rule is the same one a caller's --team
		// passes through.
		{"linear team wildcard is not a key", `{"linear_team":["*"]}`, PermLinearRead, "", "not a letter or a digit"},
		{"linear team prefix glob is not a key", `{"linear_team":["TCL*"]}`, PermLinearRead, "", "not a letter or a digit"},
		{"linear team path is not a key", `{"linear_team":["acme/TCL"]}`, PermLinearRead, "", "not a letter or a digit"},
		{"linear team key length is bounded", `{"linear_team":["THISKEYISWAYTOOLONG"]}`, PermLinearRead, "", "longer than 16"},
		{"undeclared linear_team dimension", `{"linear_team":["TCL"]}`, PermGroupsMembersSpawn, "", "does not declare"},
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
			Slug: PermGroupsMembersSpawn, ScopeJSON: raw,
		}}},
		Permissions: []groupexport.Permission{{
			ConvID: "conv", Slug: PermGroupsMembersSpawn, Effect: "grant", ScopeJSON: raw,
		}},
		SudoGrants: []groupexport.SudoGrant{{
			ConvID: "conv", Slug: PermGroupsMembersSpawn, ScopeJSON: raw,
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
