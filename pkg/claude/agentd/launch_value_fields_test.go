package agentd

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// TestLaunchValueFieldSpecs_Complete pins the table's shape: every field has a
// unique name, exactly one accessor family, and its accessor addresses its own
// field of launchValueFields (no two specs write the same struct field).
func TestLaunchValueFieldSpecs_Complete(t *testing.T) {
	names := map[string]bool{}
	written := map[int]launchValueField{}
	for i, spec := range launchValueFieldSpecs {
		f := launchValueField(i)
		require.NotEmpty(t, spec.name, "field %d has no name", i)
		require.False(t, names[spec.name], "duplicate field name %q", spec.name)
		names[spec.name] = true
		isStr := spec.str != nil && spec.profileStr != nil && spec.validateStr != nil
		isNum := spec.num != nil && spec.profileNum != nil && spec.validateNum != nil
		require.True(t, isStr != isNum, "%s must have exactly one complete accessor family", spec.name)

		var v launchValueFields
		if isStr {
			*spec.str(&v) = "x"
		} else {
			*spec.num(&v) = 1
		}
		rv := reflect.ValueOf(v)
		changed := -1
		for j := 0; j < rv.NumField(); j++ {
			if !rv.Field(j).IsZero() {
				require.Equal(t, -1, changed, "%s writes more than one field", spec.name)
				changed = j
			}
		}
		require.NotEqual(t, -1, changed, "%s writes no field", spec.name)
		prior, dup := written[changed]
		require.False(t, dup, "%s and %s write the same struct field", spec.name, launchValueFieldSpecs[prior].name)
		written[changed] = f
	}
	assert.Len(t, written, reflect.TypeOf(launchValueFields{}).NumField(),
		"every launchValueFields field needs a spec")
}

func TestResolveLaunchValueFields_Precedence(t *testing.T) {
	claude, err := resolveSpawnHarness("claude")
	require.NoError(t, err)

	named := func(p *db.SpawnProfile) launchProfileTier {
		return launchProfileTier{profile: p, source: profileSource(p, agent.ProvCLIProfileSource)}
	}
	group := func(p *db.SpawnProfile) launchProfileTier {
		return launchProfileTier{profile: p, source: profileSource(p, agent.ProvGroupProfileSource), defaultTier: true}
	}
	global := func(p *db.SpawnProfile) launchProfileTier {
		return launchProfileTier{profile: p, source: profileSource(p, agent.ProvGlobalProfileSource), defaultTier: true}
	}
	codexHigh := &db.SpawnProfile{Name: "codex-high", Harness: "codex", Effort: "high"}

	cases := []struct {
		name       string
		explicit   launchValueFields
		tiers      []launchProfileTier
		wantEffort string
		wantSource string
		wantNote   bool
		wantFail   string // spawnFailure.Kind
	}{
		{name: "explicit valid", explicit: launchValueFields{Effort: "high"},
			tiers:      []launchProfileTier{named(&db.SpawnProfile{Name: "n", Effort: "low"})},
			wantEffort: "high", wantSource: agent.ProvExplicit},
		{name: "explicit invalid", explicit: launchValueFields{Effort: "bogus"}, wantFail: "invalid_effort"},
		{name: "named profile wins over defaults",
			tiers: []launchProfileTier{named(&db.SpawnProfile{Name: "n", Effort: "low"}),
				group(&db.SpawnProfile{Name: "g", Effort: "high"})},
			wantEffort: "low", wantSource: profileSource(&db.SpawnProfile{Name: "n"}, agent.ProvCLIProfileSource)},
		{name: "same-harness profile invalid fails loudly",
			tiers:    []launchProfileTier{named(&db.SpawnProfile{Name: "n", Effort: "bogus"})},
			wantFail: "invalid_effort"},
		{name: "foreign-harness default tier skipped with note",
			tiers:      []launchProfileTier{group(codexHigh), global(&db.SpawnProfile{Name: "glob", Effort: "medium"})},
			wantEffort: "medium", wantSource: profileSource(&db.SpawnProfile{Name: "glob"}, agent.ProvGlobalProfileSource),
			wantNote: true},
		{name: "group default only",
			tiers:      []launchProfileTier{group(&db.SpawnProfile{Name: "g", Effort: "high"})},
			wantEffort: "high", wantSource: profileSource(&db.SpawnProfile{Name: "g"}, agent.ProvGroupProfileSource)},
		{name: "nothing set falls to harness default", wantSource: agent.ProvHarnessDefault},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := launchValueResolution{Values: tc.explicit}
			fail := resolveLaunchValueFields(claude, tc.tiers, &res, launchValueOptions{})
			if tc.wantFail != "" {
				require.NotNil(t, fail)
				assert.Equal(t, tc.wantFail, fail.Kind)
				return
			}
			require.Nil(t, fail)
			assert.Equal(t, tc.wantEffort, res.Values.Effort)
			assert.Equal(t, tc.wantSource, res.Source[launchValueEffort])
			assert.Equal(t, tc.wantNote, res.Note[launchValueEffort] != "", "note: %q", res.Note[launchValueEffort])
			assert.Equal(t, res.Note[launchValueEffort] != "", len(res.Notes()) == 1)
		})
	}
}

// Every field resolves to the harness default when nothing names it, and
// takes an explicit value with explicit provenance.
func TestResolveLaunchValueFields_AllFieldsUnsetAndExplicit(t *testing.T) {
	claude, err := resolveSpawnHarness("claude")
	require.NoError(t, err)
	var res launchValueResolution
	require.Nil(t, resolveLaunchValueFields(claude, nil, &res, launchValueOptions{}))
	assert.Equal(t, launchValueFields{}, res.Values)
	for i, spec := range launchValueFieldSpecs {
		assert.Equal(t, agent.ProvHarnessDefault, res.Source[i], spec.name)
	}

	res = launchValueResolution{Values: launchValueFields{Model: "opus", Effort: "high"}}
	require.Nil(t, resolveLaunchValueFields(claude, nil, &res, launchValueOptions{}))
	assert.Equal(t, agent.ProvExplicit, res.Source[launchValueModel])
	assert.Equal(t, agent.ProvExplicit, res.Source[launchValueEffort])
}

// Fields resolve in table order and stop at the first failure, keeping the
// notes of fields resolved before it.
func TestResolveLaunchValueFields_OrderAndPartialResult(t *testing.T) {
	claude, err := resolveSpawnHarness("claude")
	require.NoError(t, err)
	res := launchValueResolution{Values: launchValueFields{Model: "not-a-model-xyz", Effort: "bogus"}}
	fail := resolveLaunchValueFields(claude, nil, &res, launchValueOptions{})
	require.NotNil(t, fail)
	assert.Equal(t, "invalid_model", fail.Kind, "model precedes effort in the table")

	res = launchValueResolution{Values: launchValueFields{Effort: "bogus"}}
	tiers := []launchProfileTier{{profile: &db.SpawnProfile{Name: "codex-high", Harness: "codex", Effort: "high", Model: "gpt-5"},
		source: "group default profile \"codex-high\"", defaultTier: true}}
	fail = resolveLaunchValueFields(claude, tiers, &res, launchValueOptions{})
	require.NotNil(t, fail)
	assert.Equal(t, "invalid_effort", fail.Kind)
	assert.NotEmpty(t, res.Note[launchValueModel], "the model skip note survives a later failure")
	assert.Equal(t, agent.ProvHarnessDefault, res.Source[launchValueModel])
}

func TestResolveLaunchValueFields_ModelValidatorOverride(t *testing.T) {
	claude, err := resolveSpawnHarness("claude")
	require.NoError(t, err)
	called := false
	res := launchValueResolution{Values: launchValueFields{Model: "custom"}}
	fail := resolveLaunchValueFields(claude, nil, &res, launchValueOptions{validateModel: func(raw string) (string, error) {
		called = true
		return raw + "!", nil
	}}, launchValueModel)
	require.Nil(t, fail)
	assert.True(t, called)
	assert.Equal(t, "custom!", res.Values.Model)
	assert.Empty(t, res.Source[launchValueEffort], "fields not named are not resolved")
}

// TestLaunchValueFieldSpecs_ProfileColumns pins which SpawnProfile column
// feeds each field: every string column is set to its own Go field name and
// every int column to a distinct number, so a spec reading the wrong column
// reports the column it actually read.
func TestLaunchValueFieldSpecs_ProfileColumns(t *testing.T) {
	want := map[string]string{
		"model_proxy":               "ModelProxy",
		modelField:                  "Model",
		effortField:                 "Effort",
		"sandbox":                   "Sandbox",
		"approval":                  "Approval",
		"tools":                     "ToolGovernance",
		"ask_user_question_timeout": "AskUserQuestionTimeout",
		"auto_compact_window":       "AutoCompactWindow",
		contextWindowMaxField:       "ContextWindowMax",
	}
	var p db.SpawnProfile
	rv := reflect.ValueOf(&p).Elem()
	intColumn := map[int64]string{}
	for i := 0; i < rv.NumField(); i++ {
		field := rv.Field(i)
		switch field.Kind() {
		case reflect.String:
			field.SetString(rv.Type().Field(i).Name)
		case reflect.Int64:
			field.SetInt(int64(i + 1))
			intColumn[int64(i+1)] = rv.Type().Field(i).Name
		}
	}
	require.Len(t, want, int(launchValueFieldCount))
	for _, spec := range launchValueFieldSpecs {
		if spec.profileStr != nil {
			assert.Equal(t, want[spec.name], spec.profileStr(&p), spec.name)
		} else {
			assert.Equal(t, want[spec.name], intColumn[spec.profileNum(&p)], spec.name)
		}
	}
}
