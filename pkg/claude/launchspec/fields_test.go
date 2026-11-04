//ci:reads docs/launch-resolution.md

package launchspec_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/launchspec"
)

// Every field of the three structs that carry launch inputs must have a
// registry row or a stated reason to be left out, so a new profile column,
// group setting or spawn flag cannot ship without a relaunch decision.
func TestRegistryCoversEveryLaunchInput(t *testing.T) {
	structs := []struct {
		name string
		typ  reflect.Type
		of   func(launchspec.Field) []string
	}{
		{"SpawnProfile", reflect.TypeOf(db.SpawnProfile{}), func(f launchspec.Field) []string { return f.ProfileFields }},
		{"AgentGroup", reflect.TypeOf(db.AgentGroup{}), func(f launchspec.Field) []string { return f.GroupFields }},
		{"SpawnRequest", reflect.TypeOf(agent.SpawnRequest{}), func(f launchspec.Field) []string { return f.RequestFields }},
	}
	for _, s := range structs {
		owner := map[string]string{}
		for _, f := range launchspec.Fields {
			for _, name := range s.of(f) {
				_, exists := s.typ.FieldByName(name)
				assert.Truef(t, exists, "row %q names %s.%s, which does not exist", f.Key, s.name, name)
				if prev, dup := owner[name]; dup {
					t.Errorf("%s.%s is claimed by both %q and %q", s.name, name, prev, f.Key)
				}
				owner[name] = f.Key
			}
		}
		for i := 0; i < s.typ.NumField(); i++ {
			sf := s.typ.Field(i)
			if !sf.IsExported() {
				continue
			}
			qualified := s.name + "." + sf.Name
			_, excluded := launchspec.NotLaunchInputs[qualified]
			_, claimed := owner[sf.Name]
			if !excluded && !claimed {
				t.Errorf("%s has no row in launchspec.Fields and no reason in NotLaunchInputs; "+
					"decide its layer, then regenerate docs/launch-resolution.md", qualified)
			}
			if excluded && claimed {
				t.Errorf("%s is both a launch input (%q) and listed in NotLaunchInputs", qualified, owner[sf.Name])
			}
		}
	}
	for qualified := range launchspec.NotLaunchInputs {
		name, field, _ := strings.Cut(qualified, ".")
		known := false
		for _, s := range structs {
			known = known || s.name == name
		}
		assert.Truef(t, known, "NotLaunchInputs key %q names no known struct", qualified)
		for _, s := range structs {
			if s.name == name {
				_, exists := s.typ.FieldByName(field)
				assert.Truef(t, exists, "NotLaunchInputs names %s, which does not exist", qualified)
			}
		}
	}
}

func TestRegistryRowsAreComplete(t *testing.T) {
	keys := map[string]bool{}
	for _, f := range launchspec.Fields {
		assert.NotEmptyf(t, f.Key, "row %q", f.Label)
		assert.Falsef(t, keys[f.Key], "duplicate key %q", f.Key)
		keys[f.Key] = true
		assert.NotEmptyf(t, f.Label, "row %q has no label", f.Key)
		assert.NotEmptyf(t, f.Layer.Relaunch(), "row %q has an unknown layer %q", f.Key, f.Layer)
		assert.NotEmptyf(t, f.Why, "row %q has no Why", f.Key)
		if f.Configurable {
			assert.Containsf(t, []launchspec.Layer{launchspec.LayerGroup, launchspec.LayerReferenced}, f.Layer,
				"row %q is configurable, but only group settings and referenced contents can be", f.Key)
		}
	}
}

// docs/launch-resolution.md carries the registry as generated tables between
// markers. Regenerate with
//
//	TCLAUDE_UPDATE_LAUNCH_DOC=1 go test ./pkg/claude/launchspec -run TestLaunchResolutionDoc
func TestLaunchResolutionDoc(t *testing.T) {
	path := filepath.Join("..", "..", "..", "docs", "launch-resolution.md")
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	doc := string(raw)
	for _, block := range []struct{ name, body string }{
		{"launch-fields", renderFieldTables()},
		{"launch-gaps", renderGaps()},
	} {
		begin, end := "<!-- "+block.name+":begin -->\n", "<!-- "+block.name+":end -->"
		i, j := strings.Index(doc, begin), strings.Index(doc, end)
		require.Truef(t, i >= 0 && j > i, "%s: markers %q ... %q missing", path, strings.TrimSpace(begin), end)
		current := doc[i+len(begin) : j]
		if os.Getenv("TCLAUDE_UPDATE_LAUNCH_DOC") != "" {
			doc = doc[:i+len(begin)] + block.body + doc[j:]
			continue
		}
		assert.Equalf(t, block.body, current, "%s: the %s block is out of date with launchspec.Fields; regenerate with "+
			"TCLAUDE_UPDATE_LAUNCH_DOC=1 go test ./pkg/claude/launchspec -run TestLaunchResolutionDoc", path, block.name)
	}
	if os.Getenv("TCLAUDE_UPDATE_LAUNCH_DOC") != "" {
		require.NoError(t, os.WriteFile(path, []byte(doc), 0o644))
	}
}

var layerHeadings = []struct {
	layer   launchspec.Layer
	heading string
}{
	{launchspec.LayerFixed, "Fixed at first spawn"},
	{launchspec.LayerSpawnCopy, "Copied from the spawn profile at first spawn"},
	{launchspec.LayerSpawnOnly, "Used by the first spawn only"},
	{launchspec.LayerGroup, "Group and workspace settings"},
	{launchspec.LayerReferenced, "Contents of referenced profiles and roles"},
	{launchspec.LayerLive, "Live, not a launch input"},
}

func renderFieldTables() string {
	var b strings.Builder
	for _, lh := range layerHeadings {
		b.WriteString("\n### " + lh.heading + "\n\n")
		b.WriteString("| Field | Key | On relaunch | Spawn flag | Why |\n|---|---|---|---|---|\n")
		for _, f := range launchspec.Fields {
			if f.Layer != lh.layer {
				continue
			}
			relaunch := f.Layer.Relaunch()
			if f.Configurable {
				relaunch += ", configurable"
			}
			flag := "no"
			if f.Explicit() {
				flag = "yes"
			}
			b.WriteString("| " + f.Label + " | `" + f.Key + "` | " + relaunch + " | " + flag + " | " + f.Why + " |\n")
		}
	}
	b.WriteString("\n")
	return b.String()
}

func renderGaps() string {
	var b strings.Builder
	b.WriteString("\n")
	for _, f := range launchspec.Fields {
		if f.Gap != "" {
			b.WriteString("- **" + f.Label + "** (`" + f.Key + "`): " + f.Gap + "\n")
		}
	}
	b.WriteString("\n")
	return b.String()
}
