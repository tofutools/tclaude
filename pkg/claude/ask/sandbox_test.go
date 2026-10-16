package ask

import (
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func TestAskSandboxResolutionModes(t *testing.T) {
	setupAskTestDB(t)
	cwd := testutil.CanonicalTempDir(t)
	for _, name := range []string{"global-policy", "group-policy", "included"} {
		_, err := db.CreateSandboxProfile(&db.SandboxProfile{Name: name, Environment: []db.SandboxEnvironmentEntry{{Name: "FROM_" + map[string]string{"global-policy": "GLOBAL", "group-policy": "GROUP", "included": "INCLUDE"}[name], Value: "yes"}}})
		require.NoError(t, err)
	}
	_, err := db.CreateSandboxProfile(&db.SandboxProfile{Name: "named", Includes: []string{"included"}})
	require.NoError(t, err)
	require.NoError(t, db.SetGlobalSandboxProfile("global-policy"))
	_, err = db.CreateAgentGroup("project", "")
	require.NoError(t, err)
	_, err = db.SetAgentGroupDefaultCwd("project", cwd)
	require.NoError(t, err)
	_, err = db.SetAgentGroupSandboxProfile("project", "group-policy")
	require.NoError(t, err)
	for _, tc := range []struct {
		setting, cwd string
		names        []string
	}{
		{"off", cwd, nil}, {"global", cwd, []string{"global-policy"}},
		{"auto", cwd, []string{"global-policy", "group-policy"}},
		{"auto", testutil.CanonicalTempDir(t), []string{"global-policy"}},
		{"named", cwd, []string{"named"}},
		{"profile:named", cwd, []string{"named"}},
	} {
		t.Run(tc.setting+tc.cwd, func(t *testing.T) {
			got, err := resolveAskSandbox(tc.setting, tc.cwd)
			require.NoError(t, err)
			if tc.names == nil {
				require.Nil(t, got)
				return
			}
			var names []string
			for _, p := range got.Applied {
				names = append(names, p.Name)
			}
			require.Equal(t, tc.names, names)
			if tc.setting == "named" || tc.setting == "profile:named" {
				require.Equal(t, []sandboxpolicy.EnvironmentEntry{{Name: "FROM_INCLUDE", Value: "yes"}}, got.Effective.Environment)
			}
		})
	}
	_, err = resolveAskSandbox("missing", cwd)
	require.ErrorContains(t, err, "choose an existing library profile")
	require.NoError(t, db.SetGlobalSandboxProfile(""))
	got, err := resolveAskSandbox("global", cwd)
	require.NoError(t, err)
	require.Nil(t, got)
	require.Equal(t, "global", (&config.Config{}).AskSandboxSetting())
}

func TestAskSandboxAutoCanonicalGroupAndAmbiguity(t *testing.T) {
	setupAskTestDB(t)
	cwd := testutil.CanonicalTempDir(t)
	alias := filepath.Join(testutil.CanonicalTempDir(t), "alias")
	require.NoError(t, os.Symlink(cwd, alias))
	for _, name := range []string{"alpha", "beta"} {
		_, err := db.CreateSandboxProfile(&db.SandboxProfile{Name: name})
		require.NoError(t, err)
		_, err = db.CreateAgentGroup(name, "")
		require.NoError(t, err)
		_, err = db.SetAgentGroupDefaultCwd(name, cwd)
		require.NoError(t, err)
		_, err = db.SetAgentGroupSandboxProfile(name, name)
		require.NoError(t, err)
	}
	_, err := resolveAskSandbox("auto", alias)
	require.ErrorContains(t, err, "multiple groups")
	require.ErrorContains(t, err, "ask.sandbox=global")
	_, err = db.SetAgentGroupDefaultSpawn("beta", true)
	require.NoError(t, err)
	got, err := resolveAskSandbox("auto", alias)
	require.NoError(t, err)
	require.Equal(t, "beta", got.Applied[0].Name)
}

func TestAskSandboxRefusesUnconfinedOrUnsupportedLaunchBeforeRunning(t *testing.T) {
	for _, tc := range []struct{ name, impl, mode, detail string }{
		{"claude", "off", "", "cannot be enforced"},
		{"claude", "harness-builtin", "off", "cannot establish confinement"},
		{"codex", "harness-builtin", "danger-full-access", "cannot establish confinement"},
		{"copilot", "tclaude-layer", "", "cannot replay"},
		{"opencode", "tclaude-layer", "", "cannot replay"},
		{"pi", "tclaude-layer", "", "cannot replay"},
	} {
		t.Run(tc.name+tc.impl, func(t *testing.T) {
			setupAskTestDB(t)
			_, err := db.CreateSandboxProfile(&db.SandboxProfile{Name: "policy"})
			require.NoError(t, err)
			h, err := harness.Resolve(tc.name)
			require.NoError(t, err)
			got, _, err := prepareAskSandbox(askInput{Cwd: testutil.CanonicalTempDir(t), SandboxSetting: "policy", LaunchProfiles: []*db.SpawnProfile{{Harness: tc.name, SandboxImplementation: tc.impl, Sandbox: tc.mode}}}, h, harness.AskSpec{Print: true, Prompt: "hello"})
			require.Nil(t, got)
			require.ErrorContains(t, err, tc.detail)
		})
	}
}

func TestAskSandboxUsesCurrentPolicyForResumedThread(t *testing.T) {
	setupAskTestDB(t)
	forceConvExists(t, true)
	f := &fakeRun{answer: "ok\n", started: true}
	f.install(t)
	cwd := testutil.CanonicalTempDir(t)
	require.NoError(t, db.SetAskThread("terminal", cwd, "existing-copilot", harness.CopilotName))
	in := ttyInput("terminal", cwd, "next turn")
	in.SandboxSetting = "global"
	aio, _, _ := io2buf()
	require.NoError(t, runAsk(in, aio))
	require.Len(t, f.plans, 1)
	_, err := db.CreateSandboxProfile(&db.SandboxProfile{Name: "new-policy"})
	require.NoError(t, err)
	require.NoError(t, db.SetGlobalSandboxProfile("new-policy"))
	require.ErrorContains(t, runAsk(in, aio), "cannot replay")
	require.Len(t, f.plans, 1, "newly configured policy must refuse before the existing conversation is resumed")
	in.SandboxSetting = "off"
	require.NoError(t, runAsk(in, aio))
	require.Len(t, f.plans, 2)
	require.Contains(t, f.plans[1].Argv, "--resume=existing-copilot")
}

func TestAskSandboxGatewayOffOverridesGlobalGateway(t *testing.T) {
	setupAskTestDB(t)
	_, err := db.CreateSandboxProfile(&db.SandboxProfile{Name: "policy"})
	require.NoError(t, err)
	h, err := harness.Resolve(harness.CodexName)
	require.NoError(t, err)
	in := askInput{Cwd: testutil.CanonicalTempDir(t), SandboxSetting: "policy", LaunchProfiles: []*db.SpawnProfile{
		{Harness: harness.CodexName, ModelProxy: "off", SandboxImplementation: "off"},
		{Harness: harness.CodexName, ModelProxy: "global-gateway"},
	}}
	_, _, err = prepareAskSandbox(in, h, harness.AskSpec{Print: true, Prompt: "hello"})
	require.ErrorContains(t, err, "cannot be enforced") // Passed identity resolution, refused off confinement.
	in.LaunchProfiles[0].ModelProxy = ""
	_, _, err = prepareAskSandbox(in, h, harness.AskSpec{Print: true, Prompt: "hello"})
	require.ErrorContains(t, err, `model gateway "global-gateway"`)
}

func TestAskForegroundRunnerPreservesStdioAndPreparedEnvironment(t *testing.T) {
	cwd := testutil.CanonicalTempDir(t)
	aio, out, errout := io2buf()
	aio.Stdin = strings.NewReader("input from terminal\n")
	prepared := &agentd.PreparedOneShotLaunch{Argv: []string{"/bin/sh", "-c", `read line; printf '%s|%s' "$line" "$POLICY_VALUE"; printf diagnostic >&2`}, Env: append(os.Environ(), "POLICY_VALUE=prepared")}
	started, err := liveRunner(runPlan{Argv: prepared.Argv, Env: prepared.Env, Preparation: prepared, Cwd: cwd, Stdin: aio.Stdin, Stdout: aio.Stdout, Stderr: aio.Stderr})
	require.NoError(t, err)
	require.True(t, started)
	require.Equal(t, "input from terminal|prepared", out.String())
	require.Equal(t, "diagnostic", errout.String())
}
