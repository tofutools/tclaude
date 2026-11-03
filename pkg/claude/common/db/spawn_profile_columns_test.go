package db

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// spawnProfileUnpersistedFields are SpawnProfile fields that no
// spawn_profiles column in spawnProfileColumns carries: ID is the row key,
// read separately, and Aliases live in spawn_profile_aliases.
var spawnProfileUnpersistedFields = map[string]bool{"ID": true, "Aliases": true}

// TestSpawnProfileColumnsCoverEveryField fails when a SpawnProfile field is
// added without a column entry, or when two entries claim one field.
func TestSpawnProfileColumnsCoverEveryField(t *testing.T) {
	claimed := map[string]string{}
	names := map[string]bool{}
	for _, c := range spawnProfileColumns {
		require.False(t, names[c.name], "column %q listed twice", c.name)
		names[c.name] = true
		require.NotEmpty(t, c.fields, "column %q names no field", c.name)
		for _, f := range c.fields {
			prev, dup := claimed[f]
			require.False(t, dup, "field %s claimed by both %q and %q", f, prev, c.name)
			claimed[f] = c.name
		}
	}
	typ := reflect.TypeOf(SpawnProfile{})
	persisted := 0
	for i := range typ.NumField() {
		f := typ.Field(i).Name
		if spawnProfileUnpersistedFields[f] {
			continue
		}
		persisted++
		assert.Contains(t, claimed, f, "SpawnProfile.%s has no spawn_profiles column entry", f)
	}
	for f := range claimed {
		_, ok := typ.FieldByName(f)
		assert.True(t, ok, "column entry names unknown field %s", f)
	}
	assert.Equal(t, persisted, len(claimed), "every persisted SpawnProfile field maps to exactly one column")
}

// TestSpawnProfileColumnsMatchSchema keeps the table and the migrated schema
// in step: every table column exists, and no schema column is left unread.
func TestSpawnProfileColumnsMatchSchema(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)
	rows, err := d.Query(`SELECT name FROM pragma_table_info('spawn_profiles')`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var schema []string
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		schema = append(schema, name)
	}
	require.NoError(t, rows.Err())
	want := []string{"id"}
	for _, c := range spawnProfileColumns {
		want = append(want, c.name)
	}
	assert.ElementsMatch(t, schema, want)
}

// fillSpawnProfile sets every persisted field to a value distinct from every
// other field's, so a swapped pair of columns or arguments reads back wrong.
// Bool fields can't all be distinct in one row, so each takes a digit of its
// own code, and across runs 0..2 any two of them differ at least once:
// *bool fields a base-3 digit (nil/false/true), bool fields a binary one.
func fillSpawnProfile(t *testing.T, run int, seed string) *SpawnProfile {
	t.Helper()
	p := &SpawnProfile{}
	v := reflect.ValueOf(p).Elem()
	typ := v.Type()
	triCode, boolCode := 0, 0
	digit := func(code *int, base int) int {
		*code++
		c := *code
		for range run {
			c /= base
		}
		return c % base
	}
	for i := range typ.NumField() {
		name := typ.Field(i).Name
		field := v.Field(i)
		if spawnProfileUnpersistedFields[name] || name == "CreatedAt" || name == "UpdatedAt" {
			continue
		}
		switch field.Interface().(type) {
		case string:
			field.SetString(fmt.Sprintf("%s-%d-%s", seed, run, name))
		case int64:
			field.SetInt(int64(1000*(run+1) + i))
		case bool:
			field.SetBool(digit(&boolCode, 2) == 1)
		case *bool:
			switch digit(&triCode, 3) {
			case 1:
				field.Set(reflect.ValueOf(boolp(false)))
			case 2:
				field.Set(reflect.ValueOf(boolp(true)))
			}
		case []string, map[string]string, map[string]PermissionOverride, []sandboxpolicy.EnvironmentEntry:
			// Set below.
		default:
			t.Fatalf("fillSpawnProfile: SpawnProfile.%s has an unhandled type %s; give it a distinct value here", name, field.Type())
		}
	}
	tag := fmt.Sprintf("%s-%d", seed, run)
	p.RoleRefs = []string{tag + "-role-a", tag + "-role-b"}
	p.RoleRef = p.RoleRefs[0] // the column is the projection of RoleRefs[0]
	p.PromptRefs = []string{tag + "-prompt"}
	p.ContextFeatures = map[string]string{tag + "-feature": "on"}
	p.PermissionOverrides = map[string]PermissionOverride{tag + "-slug": {Effect: PermEffectGrant}}
	p.Environment = []sandboxpolicy.EnvironmentEntry{{Name: strings.ToUpper("TCL_" + seed), Value: tag + "-env"}}
	return p
}

// TestSpawnProfile_EveryColumnRoundTrips writes and rewrites a fully
// populated profile through CreateSpawnProfile and UpdateSpawnProfile and
// requires every field back unchanged.
func TestSpawnProfile_EveryColumnRoundTrips(t *testing.T) {
	setupTestDB(t)
	for run := range 3 {
		t.Run(fmt.Sprintf("run%d", run), func(t *testing.T) {
			want := fillSpawnProfile(t, run, "create")
			id, err := CreateSpawnProfile(want)
			require.NoError(t, err)

			got, err := GetSpawnProfileByID(id)
			require.NoError(t, err)
			require.NotNil(t, got)
			require.False(t, got.CreatedAt.IsZero())
			assert.Equal(t, got.CreatedAt, got.UpdatedAt)
			want.ID, want.Aliases = id, []string{}
			want.CreatedAt, want.UpdatedAt = got.CreatedAt, got.UpdatedAt
			assert.Equal(t, want, got, "create round-trip")

			update := fillSpawnProfile(t, (run+1)%3, "update")
			update.ID = id
			require.NoError(t, UpdateSpawnProfile(update))
			got, err = GetSpawnProfileByID(id)
			require.NoError(t, err)
			require.NotNil(t, got)
			assert.Equal(t, want.CreatedAt, got.CreatedAt, "update leaves created_at alone")
			update.Aliases = []string{}
			update.CreatedAt, update.UpdatedAt = got.CreatedAt, got.UpdatedAt
			assert.Equal(t, update, got, "update round-trip")
		})
	}
}

// TestSpawnProfileColumnsWriteTheirNamedField catches a symmetric mix-up the
// round trip cannot: column A reading and writing field B while column B
// reads and writes field A still round-trips, but stores data under the
// wrong column. Each column's write value must change when its named field
// is zeroed.
func TestSpawnProfileColumnsWriteTheirNamedField(t *testing.T) {
	// role_ref is derived from RoleRefs[0] on write; the RoleRef field is only
	// the legacy fallback when RoleRefs is empty.
	derived := map[string]bool{"role_ref": true}
	for _, c := range spawnProfileColumns {
		if derived[c.name] || c.name == "created_at" || c.name == "updated_at" {
			continue
		}
		for _, field := range c.fields {
			bound := false
			for run := range 3 {
				p := fillSpawnProfile(t, run, "bind")
				full, err := newSpawnProfileWrite(p, nil)
				require.NoError(t, err)
				q := *p
				f := reflect.ValueOf(&q).Elem().FieldByName(field)
				f.Set(reflect.Zero(f.Type()))
				zeroed, err := newSpawnProfileWrite(&q, nil)
				require.NoError(t, err)
				if !reflect.DeepEqual(c.value(full), c.value(zeroed)) {
					bound = true
					break
				}
			}
			assert.True(t, bound, "column %q does not write SpawnProfile.%s", c.name, field)
		}
	}
}
