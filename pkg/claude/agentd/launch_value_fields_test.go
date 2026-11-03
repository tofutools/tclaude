package agentd

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
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

// TestLaunchBoolFieldSpecs_ProfileColumns pins which *bool SpawnProfile column
// feeds each bool field, by pointer identity.
func TestLaunchBoolFieldSpecs_ProfileColumns(t *testing.T) {
	var p db.SpawnProfile
	want := map[string]*bool{}
	rv := reflect.ValueOf(&p).Elem()
	for i := 0; i < rv.NumField(); i++ {
		if rv.Field(i).Type() == reflect.TypeOf((*bool)(nil)) {
			b := new(bool)
			rv.Field(i).Set(reflect.ValueOf(b))
			want[rv.Type().Field(i).Name] = b
		}
	}
	columns := map[string]string{
		"auto_review": "AutoReview", "trust_dir": "TrustDir", "copilot_api": "CopilotAPI",
		"codex_app_server": "CodexAppServer", "fast_mode": "FastMode", "ssh_workaround": "SSHWorkaround",
	}
	require.Len(t, columns, int(launchBoolFieldCount))
	names := map[string]bool{}
	for _, spec := range launchBoolFieldSpecs {
		require.False(t, names[spec.name], "duplicate %s", spec.name)
		names[spec.name] = true
		require.NotNil(t, spec.validate, spec.name)
		col, ok := columns[spec.name]
		require.True(t, ok, "unexpected field %s", spec.name)
		assert.Same(t, want[col], spec.profile(&p), spec.name)
	}
}

func TestResolveLaunchBoolFields(t *testing.T) {
	claude, err := resolveSpawnHarness("claude")
	require.NoError(t, err)
	yes, no := true, false
	group := func(p *db.SpawnProfile) launchProfileTier {
		return launchProfileTier{profile: p, source: profileSource(p, agent.ProvGroupProfileSource), defaultTier: true}
	}

	t.Run("unset falls to harness default", func(t *testing.T) {
		var res launchBoolResolution
		notes, fail := resolveLaunchBoolFields(claude, nil, &res, launchBoolTrustDir, launchBoolAutoReview)
		require.Nil(t, fail)
		assert.Empty(t, notes)
		assert.False(t, res.Value[launchBoolTrustDir])
		assert.False(t, res.Set[launchBoolTrustDir])
		assert.Equal(t, agent.ProvHarnessDefault, res.Source[launchBoolTrustDir])
	})
	t.Run("explicit false is a decision and beats a profile true", func(t *testing.T) {
		var res launchBoolResolution
		res.Set[launchBoolTrustDir] = true
		_, fail := resolveLaunchBoolFields(claude, []launchProfileTier{group(&db.SpawnProfile{Name: "g", TrustDir: &yes})},
			&res, launchBoolTrustDir)
		require.Nil(t, fail)
		assert.False(t, res.Value[launchBoolTrustDir])
		assert.True(t, res.Set[launchBoolTrustDir])
		assert.Equal(t, agent.ProvExplicit, res.Source[launchBoolTrustDir])
	})
	t.Run("profile tier decides when nothing explicit", func(t *testing.T) {
		var res launchBoolResolution
		g := &db.SpawnProfile{Name: "g", TrustDir: &yes}
		_, fail := resolveLaunchBoolFields(claude, []launchProfileTier{group(g)}, &res, launchBoolTrustDir)
		require.Nil(t, fail)
		assert.True(t, res.Value[launchBoolTrustDir])
		assert.True(t, res.Set[launchBoolTrustDir])
		assert.Equal(t, profileSource(g, agent.ProvGroupProfileSource), res.Source[launchBoolTrustDir])
	})
	t.Run("explicit value invalid for harness fails", func(t *testing.T) {
		var res launchBoolResolution
		res.Value[launchBoolAutoReview], res.Set[launchBoolAutoReview] = true, true
		_, fail := resolveLaunchBoolFields(claude, nil, &res, launchBoolAutoReview)
		require.NotNil(t, fail)
		assert.Equal(t, "invalid_auto_review", fail.Kind)
	})
	t.Run("foreign-harness tiers skipped with notes in caller order; failure keeps prior notes", func(t *testing.T) {
		codex := &db.SpawnProfile{Name: "cx", Harness: "codex", TrustDir: &no, AutoReview: &yes}
		var res launchBoolResolution
		notes, fail := resolveLaunchBoolFields(claude, []launchProfileTier{group(codex)}, &res,
			launchBoolAutoReview, launchBoolTrustDir)
		require.Nil(t, fail)
		require.Len(t, notes, 2)
		assert.Contains(t, notes[0], "auto_review")
		assert.Contains(t, notes[1], "trust_dir")

		res = launchBoolResolution{}
		res.Value[launchBoolCodexAppServer], res.Set[launchBoolCodexAppServer] = true, true
		notes, fail = resolveLaunchBoolFields(claude, []launchProfileTier{group(codex)}, &res,
			launchBoolTrustDir, launchBoolCodexAppServer)
		require.NotNil(t, fail)
		assert.Equal(t, "invalid_codex_app_server", fail.Kind)
		require.Len(t, notes, 1)
		assert.Contains(t, notes[0], "trust_dir")
	})
}

// TestLaunchBoolFieldSpecs_Validators pins each bool spec's validator to the
// harness function it must call: for every registered harness and both
// values, the spec answers exactly what that function answers.
func TestLaunchBoolFieldSpecs_Validators(t *testing.T) {
	want := map[launchBoolField]func(*harness.Harness, bool) (bool, error){
		launchBoolAutoReview: harness.ResolveAutoReview,
		launchBoolTrustDir:   harness.ResolveTrustDir,
		launchBoolCopilotAPI: func(h *harness.Harness, v bool) (bool, error) { return harness.ResolveCopilotAPI(h, &v) },
		launchBoolCodexAppServer: func(h *harness.Harness, v bool) (bool, error) {
			return harness.ResolveCodexAppServer(h, &v)
		},
		launchBoolFastMode: func(h *harness.Harness, v bool) (bool, error) {
			_, err := harness.ResolveFastMode(h, &v)
			return v, err
		},
		launchBoolSSHWorkaround: func(h *harness.Harness, v bool) (bool, error) {
			return harness.ResolveSSHWorkaround(h, &v)
		},
	}
	require.Len(t, want, int(launchBoolFieldCount))
	for _, name := range []string{"claude", "codex", "opencode", "copilot", "gemini", "pi"} {
		h, err := resolveSpawnHarness(name)
		require.NoError(t, err)
		for f, expected := range want {
			spec := launchBoolFieldSpecs[f]
			for _, v := range []bool{true, false} {
				gotV, gotErr := spec.validate(h)(v)
				wantV, wantErr := expected(h, v)
				assert.Equal(t, wantV, gotV, "%s on %s (%v)", spec.name, name, v)
				assert.Equal(t, fmt.Sprint(wantErr), fmt.Sprint(gotErr), "%s on %s (%v)", spec.name, name, v)
			}
		}
	}
}
