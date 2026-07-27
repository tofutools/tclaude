//go:build linux || darwin

package agentd

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

func TestPrivateOpenCodeStateBuildsPerAgentV3Contract(t *testing.T) {
	setupTestDB(t)
	home, err := filepath.EvalSymlinks(os.Getenv("HOME"))
	require.NoError(t, err)
	for _, item := range []struct{ name, dir string }{
		{"XDG_DATA_HOME", "ambient-data"},
		{"XDG_CACHE_HOME", "ambient-cache"},
		{"XDG_CONFIG_HOME", "ambient-config"},
		{"XDG_STATE_HOME", "ambient-state"},
	} {
		t.Setenv(item.name, filepath.Join(home, item.dir))
	}
	ambientData := filepath.Join(home, "ambient-data", "opencode")
	ambientConfig := filepath.Join(home, "ambient-config", "opencode")
	install := filepath.Join(home, ".opencode")
	cwd := filepath.Join(home, "work")
	for _, path := range []string{ambientData, ambientConfig, install, cwd} {
		require.NoError(t, os.MkdirAll(path, 0o700))
	}
	require.NoError(t, os.WriteFile(filepath.Join(ambientData, "auth.json"),
		[]byte(`{"provider":"seed"}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(ambientConfig, "opencode.jsonc"),
		[]byte(`{"model":"project-may-override-this"}`), 0o600))

	agentA := "agt_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	agentB := "agt_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	allocationA, err := allocatePrivateOpenCodeState(agentA)
	require.NoError(t, err)
	allocationB, err := allocatePrivateOpenCodeState(agentB)
	require.NoError(t, err)
	assert.Equal(t, agentA, filepath.Base(allocationA.StateRoot))
	assert.Equal(t, agentB, filepath.Base(allocationB.StateRoot))
	assert.Equal(t, filepath.Dir(allocationA.StateRoot), filepath.Dir(allocationB.StateRoot))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "moved-data-home"))
	stableAllocation, err := allocatePrivateOpenCodeState(agentA)
	require.NoError(t, err)
	assert.Equal(t, allocationA.StateRoot, stableAllocation.StateRoot,
		"a later XDG environment must not move an existing allocation")
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "ambient-data"))

	snapshot := sandboxpolicy.EmptySnapshot()
	snapshot.Effective.Filesystem = []sandboxpolicy.FilesystemGrant{
		{Path: filepath.Join(home, "ambient-data"), Access: sandboxpolicy.AccessWrite},
		{Path: filepath.Join(home, "ambient-config"), Access: sandboxpolicy.AccessWrite},
	}
	specA, err := openCodeTclaudeLayerLaunchSpec(
		string(sandboxpolicy.ImplementationTclaudeLayer), cwd, nil, &snapshot, agentA)
	require.NoError(t, err)
	specB, err := openCodeTclaudeLayerLaunchSpec(
		string(sandboxpolicy.ImplementationTclaudeLayer), cwd, nil, &snapshot, agentB)
	require.NoError(t, err)
	require.Equal(t, session.TclaudeLayerLaunchSpecVersion, specA.Version)
	require.NoError(t, validateOpenCodeV3LaunchContract(specA.Contract))
	require.Len(t, specA.Contract.StateDirs, 4)
	require.Equal(t, []sandboxpolicy.EnvironmentEntry{
		{Name: "XDG_DATA_HOME", Value: filepath.Join(allocationA.StateRoot, "data")},
		{Name: "XDG_CACHE_HOME", Value: filepath.Join(allocationA.StateRoot, "cache")},
		{Name: "XDG_CONFIG_HOME", Value: filepath.Join(allocationA.StateRoot, "config")},
		{Name: "XDG_STATE_HOME", Value: filepath.Join(allocationA.StateRoot, "state")},
	}, specA.Contract.Environment)
	assert.NotContains(t, environmentNames(specA.Contract.Environment), "OPENCODE_CONFIG_DIR")
	assert.ElementsMatch(t, []string{
		filepath.Join(home, "ambient-data", "opencode"),
		filepath.Join(home, "ambient-cache", "opencode"),
		filepath.Join(home, "ambient-state", "opencode"),
	}, specA.Contract.FinalHideDirs)
	require.Contains(t, specA.Contract.ReadOnlyBinds, session.TclaudeLayerReadOnlyBind{
		Source: ambientConfig,
		Target: filepath.Join(allocationA.StateRoot, "config", "opencode"),
	})
	require.Contains(t, specA.Contract.ReadOnlyBinds, session.TclaudeLayerReadOnlyBind{
		Source: install, Target: install,
	})
	missingEnvironment := specA.Contract
	missingEnvironment.Environment = nil
	require.ErrorContains(t, validateOpenCodeV3LaunchContract(missingEnvironment),
		"no enforced XDG environment")
	missingPrivatePair := specA.Contract
	missingPrivatePair.PrivateWriteDirs = nil
	require.ErrorContains(t, validateOpenCodeV3LaunchContract(missingPrivatePair),
		"does not hide siblings")
	missingLegacyHides := specA.Contract
	missingLegacyHides.FinalHideDirs = nil
	require.ErrorContains(t, validateOpenCodeV3LaunchContract(missingLegacyHides),
		"must hide the three ambient")
	missingConfigBind := specA.Contract
	missingConfigBind.ReadOnlyBinds = []session.TclaudeLayerReadOnlyBind{
		{Source: install, Target: install},
	}
	require.ErrorContains(t, validateOpenCodeV3LaunchContract(missingConfigBind),
		"does not bind global config read-only")
	seeded, err := os.ReadFile(filepath.Join(allocationA.StateRoot, "data", "opencode", "auth.json"))
	require.NoError(t, err)
	assert.JSONEq(t, `{"provider":"seed"}`, string(seeded))
	info, err := os.Stat(filepath.Join(allocationA.StateRoot, "data", "opencode", "auth.json"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	if runtime.GOOS == "linux" {
		commandA, err := session.WrapTclaudeLayerServerSpec("/usr/bin/bwrap", *specA, "true")
		require.NoError(t, err)
		commandB, err := session.WrapTclaudeLayerServerSpec("/usr/bin/bwrap", *specB, "true")
		require.NoError(t, err)
		assert.Contains(t, commandA, allocationA.StateRoot)
		assert.NotContains(t, commandA, allocationB.StateRoot)
		assert.Contains(t, commandB, allocationB.StateRoot)
		assert.NotContains(t, commandB, allocationA.StateRoot)
		assert.Contains(t, commandA, "--ro-bind")
		policyDataWrite := strings.Index(commandA, "--bind "+
			clcommon.ShellQuoteArg(filepath.Join(home, "ambient-data")))
		ambientDataHide := strings.Index(commandA, "--tmpfs "+
			clcommon.ShellQuoteArg(ambientData))
		policyConfigWrite := strings.Index(commandA, "--bind "+
			clcommon.ShellQuoteArg(filepath.Join(home, "ambient-config")))
		configReadOnly := strings.LastIndex(commandA, "--ro-bind "+
			clcommon.ShellQuoteArg(ambientConfig)+" "+
			clcommon.ShellQuoteArg(ambientConfig))
		privateParentHide := strings.LastIndex(commandA, "--tmpfs "+
			clcommon.ShellQuoteArg(filepath.Dir(allocationA.StateRoot)))
		privateCurrentReopen := strings.LastIndex(commandA, "--bind "+
			clcommon.ShellQuoteArg(allocationA.StateRoot)+" "+
			clcommon.ShellQuoteArg(allocationA.StateRoot))
		require.GreaterOrEqual(t, policyDataWrite, 0)
		require.GreaterOrEqual(t, ambientDataHide, 0)
		require.GreaterOrEqual(t, policyConfigWrite, 0)
		require.GreaterOrEqual(t, configReadOnly, 0)
		require.GreaterOrEqual(t, privateParentHide, 0)
		require.GreaterOrEqual(t, privateCurrentReopen, 0)
		assert.Less(t, policyDataWrite, ambientDataHide,
			"daemon-final legacy-state hide must repair a broad policy write")
		assert.Less(t, policyConfigWrite, configReadOnly,
			"daemon-final global config bind must repair a broad policy write")
		assert.Less(t, privateParentHide, privateCurrentReopen,
			"the daemon must hide the shared parent before reopening only this agent")
	}
}

func TestPrivateOpenCodeStateRefusesInvalidAndMissingAllocations(t *testing.T) {
	setupTestDB(t)
	_, err := allocatePrivateOpenCodeState("agt_not-hex")
	require.ErrorContains(t, err, "invalid OpenCode state agent id")

	cwd := t.TempDir()
	_, err = openCodeTclaudeLayerLaunchSpec(
		string(sandboxpolicy.ImplementationTclaudeLayer), cwd, nil, nil,
		"agt_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	require.ErrorContains(t, err, "refusing shared-state fallback")

	protectedData := filepath.Join(os.Getenv("HOME"), ".tclaude", "data")
	t.Setenv("XDG_DATA_HOME", protectedData)
	forbiddenParent := filepath.Join(protectedData, "tclaude", "opencode-agents")
	_, err = allocatePrivateOpenCodeState("agt_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	require.ErrorContains(t, err, "under protected root")
	_, statErr := os.Stat(forbiddenParent)
	require.ErrorIs(t, statErr, os.ErrNotExist,
		"allocation refusal must happen before creating a writable protected-root child")
}

func TestLegacyOpenCodeStateKeepsAmbientXDGAndHidesNewPrivateParent(t *testing.T) {
	setupTestDB(t)
	home := os.Getenv("HOME")
	cwd := filepath.Join(home, "work")
	config := filepath.Join(home, ".config", "opencode")
	install := filepath.Join(home, ".opencode")
	for _, path := range []string{cwd, config, filepath.Join(install, "bin")} {
		require.NoError(t, os.MkdirAll(path, 0o700))
	}
	agentID := "agt_cccccccccccccccccccccccccccccccc"
	inserted, err := db.InsertOpenCodeAgentStateAllocation(
		db.OpenCodeAgentStateAllocation{
			AgentID: agentID, Mode: db.OpenCodeStateLegacyShared,
		})
	require.NoError(t, err)
	require.True(t, inserted)

	spec, err := openCodeTclaudeLayerLaunchSpec(
		string(sandboxpolicy.ImplementationTclaudeLayer), cwd, nil, nil, agentID)
	require.NoError(t, err)
	assert.Empty(t, spec.Contract.Environment)
	assert.Empty(t, spec.Contract.PrivateWriteDirs)
	require.Equal(t, []string{
		filepath.Join(home, ".local", "share", "tclaude", "opencode-agents"),
	}, spec.Contract.FinalHideDirs)
	assert.Contains(t, spec.Contract.ReadOnlyBinds,
		session.TclaudeLayerReadOnlyBind{Source: config, Target: config})
	assert.Contains(t, spec.Contract.ReadOnlyBinds,
		session.TclaudeLayerReadOnlyBind{Source: install, Target: install})
}

func TestPrivateOpenCodeCredentialSeedNeverOverwritesAndRefusesSymlink(t *testing.T) {
	sourceDir := t.TempDir()
	destinationDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(sourceDir, "auth.json"), []byte("ambient"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(destinationDir, "auth.json"), []byte("private"), 0o600))
	require.NoError(t, seedOpenCodeCredentials(sourceDir, destinationDir))
	got, err := os.ReadFile(filepath.Join(destinationDir, "auth.json"))
	require.NoError(t, err)
	assert.Equal(t, "private", string(got))

	require.NoError(t, os.WriteFile(filepath.Join(sourceDir, "real-mcp.json"), []byte("secret"), 0o600))
	require.NoError(t, os.Symlink("real-mcp.json", filepath.Join(sourceDir, "mcp-auth.json")))
	err = seedOpenCodeCredentials(sourceDir, destinationDir)
	require.ErrorContains(t, err, "ambient OpenCode credential")
}

func TestOpenCodeServerEnvironmentPinsPrivateXDGAfterProfile(t *testing.T) {
	spec := &session.TclaudeLayerLaunchSpec{
		Effective: sandboxpolicy.EffectiveProfile{Environment: []sandboxpolicy.EnvironmentEntry{
			{Name: "XDG_DATA_HOME", Value: "/profile/data"},
			{Name: "XDG_CACHE_HOME", Value: "/profile/cache"},
		}},
		Contract: session.TclaudeLayerLaunchContract{Environment: []sandboxpolicy.EnvironmentEntry{
			{Name: "XDG_DATA_HOME", Value: "/private/data"},
			{Name: "XDG_CACHE_HOME", Value: "/private/cache"},
		}},
	}
	env := openCodeServerEnvironment([]string{
		"XDG_DATA_HOME=/ambient/data",
		"OPENCODE_CONFIG_DIR=/ambient/custom-config",
	}, spec)
	assert.Equal(t, "/private/data", lastOpenCodeEnvironmentValue(env, "XDG_DATA_HOME"))
	assert.Equal(t, "/private/cache", lastOpenCodeEnvironmentValue(env, "XDG_CACHE_HOME"))
	assert.Empty(t, lastOpenCodeEnvironmentValue(env, "OPENCODE_CONFIG_DIR"))

	attach := openCodeAttachProcessEnvironment([]string{
		"PATH=/usr/bin",
		"XDG_DATA_HOME=/ambient/data",
		"OPENCODE_CONFIG_DIR=/ambient/custom-config",
	})
	assert.Equal(t, "/usr/bin", lastOpenCodeEnvironmentValue(attach, "PATH"))
	assert.Empty(t, lastOpenCodeEnvironmentValue(attach, "XDG_DATA_HOME"))
	assert.Empty(t, lastOpenCodeEnvironmentValue(attach, "OPENCODE_CONFIG_DIR"))
}

func environmentNames(entries []sandboxpolicy.EnvironmentEntry) string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name)
	}
	return strings.Join(names, ",")
}
