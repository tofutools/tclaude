package ask

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func setGlobalAskDefault(t *testing.T, profile db.SpawnProfile) {
	t.Helper()
	id, err := db.CreateSpawnProfile(&profile)
	require.NoError(t, err)
	require.NoError(t, db.SetDashboardProfileRef(db.DashboardDefaultSpawnProfileNamePrefKey, db.DashboardDefaultSpawnProfileIDPrefKey, profile.Name, id))
}

func TestAskGlobalDefaultResolutionAcrossHarnesses(t *testing.T) {
	for _, name := range []string{"claude", "codex", "opencode", "copilot", "gemini", "pi"} {
		t.Run(name, func(t *testing.T) {
			setupAskTestDB(t)
			setGlobalAskDefault(t, db.SpawnProfile{Name: "global", Harness: name})
			target, err := resolveAskTargetDetails("", "", &config.Config{})
			require.NoError(t, err)
			require.Equal(t, name, target.Harness)
			if name == harness.DefaultName {
				require.Equal(t, config.DefaultAskModel, target.Model)
				require.Equal(t, config.DefaultAskEffort, target.Effort)
			} else {
				require.Empty(t, target.Model)
				require.Empty(t, target.Effort)
			}
		})
	}
}

func TestAskDefaultsPrecedenceAndGroupIsolation(t *testing.T) {
	setupAskTestDB(t)
	setGlobalAskDefault(t, db.SpawnProfile{Name: "global", Harness: "codex", Model: "gpt-5", Effort: "low"})
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "group-only", Harness: "claude", Model: "opus", Effort: "high"})
	require.NoError(t, err)
	_, err = db.CreateAgentGroup("unrelated", "")
	require.NoError(t, err)
	_, err = db.SetAgentGroupDefaultProfile("unrelated", "group-only")
	require.NoError(t, err)
	target, err := resolveAskTargetDetails("", "", &config.Config{})
	require.NoError(t, err)
	require.Equal(t, "codex", target.Harness)
	require.Equal(t, "gpt-5", target.Model)
	require.Equal(t, "low", target.Effort)
	require.True(t, target.ModelGlobal)
	require.True(t, target.EffortGlobal)
	cfg := &config.Config{Ask: &config.AskConfig{Model: "gpt-5-codex", Effort: "high"}}
	target, err = resolveAskTargetDetails("", "", cfg)
	require.NoError(t, err)
	require.Equal(t, "gpt-5-codex", target.Model)
	require.Equal(t, "high", target.Effort)
	require.False(t, target.ModelGlobal)
	require.False(t, target.EffortGlobal)
	_, err = db.CreateSpawnProfile(&db.SpawnProfile{Name: "ask", Harness: "claude", Model: "haiku", Effort: "low"})
	require.NoError(t, err)
	cfg.Ask.Profile = "ask"
	target, err = resolveAskTargetDetails("", "", cfg)
	require.NoError(t, err)
	require.Equal(t, "claude", target.Harness)
	require.Equal(t, "haiku", target.Model)
	require.Equal(t, "low", target.Effort)
	target, err = resolveAskTargetDetails("opus", "high", cfg)
	require.NoError(t, err)
	require.Equal(t, "opus", target.Model)
	require.Equal(t, "high", target.Effort)
}

func TestAskMissingProfileFallsBackToGlobalAndSkipsIncompatibleValues(t *testing.T) {
	setupAskTestDB(t)
	setGlobalAskDefault(t, db.SpawnProfile{Name: "global", Harness: "codex", Model: "gpt-5", Effort: "low"})
	cfg := &config.Config{Ask: &config.AskConfig{Profile: "deleted"}}
	target, err := resolveAskTargetDetails("", "", cfg)
	require.NoError(t, err)
	require.Equal(t, "codex", target.Harness)
	require.Equal(t, "gpt-5", target.Model)
	_, err = db.CreateSpawnProfile(&db.SpawnProfile{Name: "claude-ask", Harness: "claude"})
	require.NoError(t, err)
	cfg.Ask.Profile = "claude-ask"
	target, err = resolveAskTargetDetails("", "", cfg)
	require.NoError(t, err)
	require.Equal(t, "claude", target.Harness)
	require.Equal(t, config.DefaultAskModel, target.Model)
	require.False(t, target.ModelGlobal)
	require.Equal(t, "low", target.Effort)
}

func TestAskResumedThreadSkipsIncompatibleGlobalVendorDefault(t *testing.T) {
	setupAskTestDB(t)
	forceConvExists(t, true)
	f := &fakeRun{answer: "ok\n", started: true}
	f.install(t)
	require.NoError(t, db.SetAskThread("existing", "/repo", "existing-claude", harness.DefaultName))
	in := ttyInput("existing", "/repo", "follow up")
	in.Harness = harness.CodexName
	in.Model = "gpt-5"
	in.ModelGlobal = true
	aio, _, _ := io2buf()
	require.NoError(t, runAsk(in, aio))
	require.Equal(t, "claude", f.last().Argv[0])
	require.NotContains(t, f.last().Argv, "gpt-5")
}

func TestAskGlobalModelDoesNotCrossPermissiveHarnessCatalogs(t *testing.T) {
	for _, test := range []struct{ globalHarness, model, askHarness string }{
		{"claude", "opus[1m]", "copilot"}, {"gemini", "gemini-2.5-pro", "codex"},
	} {
		t.Run(test.askHarness, func(t *testing.T) {
			setupAskTestDB(t)
			setGlobalAskDefault(t, db.SpawnProfile{Name: "global", Harness: test.globalHarness, Model: test.model})
			_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "ask", Harness: test.askHarness})
			require.NoError(t, err)
			target, err := resolveAskTargetDetails("", "", &config.Config{Ask: &config.AskConfig{Profile: "ask"}})
			require.NoError(t, err)
			require.Empty(t, target.Model, "ambient models are harness-owned even with permissive validation")
			forceConvExists(t, true)
			f := &fakeRun{answer: "ok\n", started: true}
			f.install(t)
			require.NoError(t, db.SetAskThread("existing", "/repo", "existing-conv", test.askHarness))
			in := ttyInput("existing", "/repo", "follow up")
			in.Harness = test.globalHarness
			in.GlobalHarness = test.globalHarness
			in.Model = test.model
			in.ModelGlobal = true
			aio, _, _ := io2buf()
			require.NoError(t, runAsk(in, aio))
			require.NotContains(t, f.last().Argv, test.model)
		})
	}
}
