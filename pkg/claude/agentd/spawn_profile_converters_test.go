package agentd

import (
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// The spawn-profile converters (profileToJSON, buildProfileFromJSON) are keyed
// struct literals, which compile fine when a newly added field is left out.
// These tests turn a forgotten converter field into a failure.

// spawnProfileWireTypeChanges are the fields whose db and wire types differ
// on purpose: Disabled is a pointer on the wire so file input can tell an
// explicit false from the pre-v122 shape, and the timestamps are RFC3339.
var spawnProfileWireTypeChanges = map[string]bool{"Disabled": true, "CreatedAt": true, "UpdatedAt": true}

// TestSpawnProfileWireMirrorsDBFields pins that db.SpawnProfile and the wire
// shape carry the same fields by name, so the converters can be checked field
// by field. ID is the row key and never on the wire.
func TestSpawnProfileWireMirrorsDBFields(t *testing.T) {
	dbType := reflect.TypeOf(db.SpawnProfile{})
	wireType := reflect.TypeOf(spawnProfileJSON{})
	for i := range dbType.NumField() {
		f := dbType.Field(i)
		if f.Name == "ID" {
			continue
		}
		w, ok := wireType.FieldByName(f.Name)
		if !assert.True(t, ok, "db.SpawnProfile.%s has no SpawnProfileJSON field", f.Name) {
			continue
		}
		if !spawnProfileWireTypeChanges[f.Name] {
			assert.Equal(t, f.Type, w.Type, "SpawnProfileJSON.%s type differs from db.SpawnProfile's", f.Name)
		}
	}
	for i := range wireType.NumField() {
		name := wireType.Field(i).Name
		_, ok := dbType.FieldByName(name)
		assert.True(t, ok, "SpawnProfileJSON.%s has no db.SpawnProfile field", name)
	}
}

// TestProfileToJSONCopiesEveryField fills every db field with a distinct
// value and requires each to arrive in the wire field of the same name.
// Each *bool gets its own allocation, so a swapped pair of toggles shows up
// as a pointer mismatch even when the values agree.
func TestProfileToJSONCopiesEveryField(t *testing.T) {
	p := &db.SpawnProfile{}
	v := reflect.ValueOf(p).Elem()
	typ := v.Type()
	for i := range typ.NumField() {
		name := typ.Field(i).Name
		field := v.Field(i)
		switch field.Interface().(type) {
		case string:
			field.SetString("v-" + name)
		case int64:
			field.SetInt(int64(1000 + i))
		case bool:
			field.SetBool(true)
		case *bool:
			b := i%2 == 0
			field.Set(reflect.ValueOf(&b))
		case []string:
			field.Set(reflect.ValueOf([]string{"v-" + name + "-a", "v-" + name + "-b"}))
		case map[string]string:
			field.Set(reflect.ValueOf(map[string]string{"v-" + name: "on"}))
		case map[string]db.PermissionOverride:
			field.Set(reflect.ValueOf(map[string]db.PermissionOverride{"v-" + name: {Effect: db.PermEffectGrant}}))
		case []sandboxpolicy.EnvironmentEntry:
			field.Set(reflect.ValueOf([]sandboxpolicy.EnvironmentEntry{{Name: "V_ENV", Value: "v-" + name}}))
		case time.Time:
			field.Set(reflect.ValueOf(time.Date(2026, 1, 2, 3, 4, i, 0, time.UTC)))
		default:
			t.Fatalf("db.SpawnProfile.%s has an unhandled type %s; give it a distinct value here", name, field.Type())
		}
	}

	out := profileToJSON(p)
	ov := reflect.ValueOf(out)
	for i := range typ.NumField() {
		name := typ.Field(i).Name
		if name == "ID" {
			continue
		}
		src, dst := v.Field(i), ov.FieldByName(name)
		switch name {
		case "Disabled":
			require.NotNil(t, out.Disabled)
			assert.Equal(t, p.Disabled, *out.Disabled, "Disabled")
		case "CreatedAt", "UpdatedAt":
			assert.Equal(t, src.Interface().(time.Time).Format(time.RFC3339), dst.String(), name)
		default:
			if src.Kind() == reflect.Pointer {
				assert.True(t, src.Pointer() == dst.Pointer(), "profileToJSON does not carry %s", name)
				continue
			}
			assert.Equal(t, src.Interface(), dst.Interface(), "profileToJSON does not carry %s", name)
		}
	}
}

// TestBuildProfileFromJSONNamesEveryField reads buildProfileFromJSON's
// returned db.SpawnProfile literal and requires a key for every field it
// stores. Field-level validation makes a single profile that sets every field
// impossible (some are Claude-only, some Codex- or Copilot-only), so this is a
// source check; TestBuildProfileFromJSONRoundTripsClaudeProfile covers the
// values for a Claude profile.
func TestBuildProfileFromJSONNamesEveryField(t *testing.T) {
	keys := spawnProfileLiteralKeys(t, "spawn_profiles.go", "buildProfileFromJSON")
	typ := reflect.TypeOf(db.SpawnProfile{})
	for i := range typ.NumField() {
		name := typ.Field(i).Name
		switch name {
		case "ID", "CreatedAt", "UpdatedAt": // assigned by the database
			continue
		}
		assert.True(t, keys[name], "buildProfileFromJSON's db.SpawnProfile literal does not set %s", name)
	}
}

// spawnProfileLiteralKeys returns the keys of the db.SpawnProfile composite
// literal inside function fn of file.
func spawnProfileLiteralKeys(t *testing.T, file, fn string) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	require.NoError(t, err)
	var keys map[string]bool
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Name.Name != fn {
			continue
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			sel, ok := lit.Type.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "SpawnProfile" {
				return true
			}
			require.Nil(t, keys, "%s has more than one db.SpawnProfile literal; extend this test", fn)
			keys = map[string]bool{}
			for _, elt := range lit.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				require.True(t, ok, "%s's db.SpawnProfile literal must be keyed", fn)
				keys[kv.Key.(*ast.Ident).Name] = true
			}
			return false
		})
	}
	require.NotNil(t, keys, "no db.SpawnProfile literal found in %s", fn)
	return keys
}

// TestBuildProfileFromJSONRoundTripsClaudeProfile sends a Claude profile that
// sets every field a Claude profile can hold without registry rows (role and
// prompt refs need stored roles/prompts) through wire → db → wire, and
// requires it back unchanged.
func TestBuildProfileFromJSONRoundTripsClaudeProfile(t *testing.T) {
	on, off := true, false
	in := spawnProfileJSON{
		Name:                       "claude-full",
		Aliases:                    []string{"claude-alias"},
		Disabled:                   &on,
		DisabledReason:             "paused for the test",
		OperatorOnly:               true,
		Harness:                    "claude",
		Model:                      "opus",
		Effort:                     "high",
		AskUserQuestionTimeout:     "5m",
		AutoCompactWindow:          "450000",
		RemoteControl:              &on,
		AutoMemory:                 &off,
		PeerMessaging:              &on,
		AgentName:                  "worker",
		Role:                       "dev",
		Descr:                      "a full claude profile",
		InitialMessage:             "hello",
		StartupContext:             "read the docs first",
		Environment:                []sandboxpolicy.EnvironmentEntry{{Name: "FOO", Value: "bar"}},
		SyncWorktree:               &on,
		FetchLatestWorktree:        &off,
		AutoFocus:                  &on,
		IncludeGroupDefaultContext: &off,
		IsOwner:                    &on,
		PermissionOverrides:        map[string]db.PermissionOverride{"human.notify": {Effect: db.PermEffectGrant}},
	}
	p, fail := buildProfileFromJSON(in)
	require.Nil(t, fail, "%+v", fail)
	got := profileToJSON(p)
	want := in
	want.RoleRefs, want.PromptRefs = []string{}, []string{}
	assert.Equal(t, want, got, "wire → db → wire")
}
