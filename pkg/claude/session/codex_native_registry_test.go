//go:build unix

package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	tclcommon "github.com/tofutools/tclaude/pkg/common"
)

func nativeRegistryFixture(t *testing.T) CodexNativeRegistryOptions {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	tmuxBase := filepath.Join(home, "tmux")
	require.NoError(t, os.Mkdir(tmuxBase, 0o700))
	t.Setenv("TMUX_TMPDIR", tmuxBase)
	db.ResetForTest()
	t.Cleanup(db.ResetForTest)
	tclaudeDir := filepath.Join(home, ".tclaude")
	dataDir := filepath.Join(tclaudeDir, "data")
	managedDir := filepath.Join(dataDir, "codex-sb-cfg")
	require.NoError(t, os.MkdirAll(managedDir, 0o700))
	for _, path := range []string{tclaudeDir, dataDir, managedDir} {
		require.NoError(t, os.Chmod(path, 0o700))
	}
	systemRoot := filepath.Join(home, "system")
	require.NoError(t, os.Mkdir(systemRoot, 0o755))
	systemDir := filepath.Join(systemRoot, "codex")
	require.NoError(t, os.Symlink(managedDir, systemDir))
	uid := currentUID()
	return CodexNativeRegistryOptions{
		SystemDir: systemDir, ManagedDir: managedDir, RootUID: uid, UserUID: uid,
	}
}

func nativeProfile(name, writable string) string {
	tmuxDir, err := clcommon.TclaudeTmuxSocketDir()
	if err != nil {
		panic(err)
	}
	privateState := tclcommon.TclaudeDataDir()
	agentdSocket := agentipc.CanonicalSocketPath()
	agentdDir := agentipc.CanonicalSocketDir()
	return fmt.Sprintf(`# Managed generated profile
default_permissions = %q

[features]
network_proxy = false
use_legacy_landlock = false

[permissions.%s]
extends = ":workspace"

[permissions.%s.filesystem]
%q = "write"
%q = "none"
%q = "none"
%q = "read"
%q = "read"

[permissions.%s.network]
enabled = true

[permissions.%s.network.unix_sockets]
%q = "allow"
`, name, name, name, writable, privateState, tmuxDir, agentdSocket, agentdDir, name, name, agentdSocket)
}

func requireNativeRegistryCode(t *testing.T, err error, code string) {
	t.Helper()
	var setupErr *CodexNativeRegistryError
	require.ErrorAs(t, err, &setupErr)
	assert.Equal(t, code, setupErr.Code)
	assert.Contains(t, err.Error(), CodexNativeRegistrySetupDoc)
}

func TestCodexNativeRegistryApplicableOnlyToBuiltinSandboxAppServer(t *testing.T) {
	builtin := string(sandboxpolicy.ImplementationHarnessBuiltin)
	assert.True(t, CodexNativeRegistryApplicable(true, harness.CodexName,
		harness.SandboxManagedProfile, builtin))
	for _, test := range []struct {
		appServer      bool
		harnessName    string
		sandbox        string
		implementation string
	}{
		{false, harness.CodexName, harness.SandboxManagedProfile, builtin},
		{true, harness.DefaultName, harness.SandboxManagedProfile, builtin},
		{true, harness.CodexName, harness.SandboxDangerFull, builtin},
		{true, harness.CodexName, harness.SandboxManagedProfile, string(sandboxpolicy.ImplementationTclaudeLayer)},
		{true, harness.CodexName, harness.SandboxManagedProfile, string(sandboxpolicy.ImplementationStacked)},
	} {
		assert.False(t, CodexNativeRegistryApplicable(test.appServer, test.harnessName,
			test.sandbox, test.implementation))
	}
}

func TestValidateCodexNativeRegistrySetupRejectsUnsafeTopologyAndFiles(t *testing.T) {
	t.Run("canonical ancestor alias", func(t *testing.T) {
		opts := nativeRegistryFixture(t)
		home := filepath.Dir(filepath.Dir(filepath.Dir(opts.ManagedDir)))
		aliasHome := filepath.Join(filepath.Dir(home), "home-alias")
		require.NoError(t, os.Symlink(home, aliasHome))
		aliasManaged := filepath.Join(aliasHome, ".tclaude", "data", "codex-sb-cfg")
		require.NoError(t, os.Remove(opts.SystemDir))
		require.NoError(t, os.Symlink(aliasManaged, opts.SystemDir))
		opts.ManagedDir = aliasManaged
		require.NoError(t, validateCodexNativeRegistrySetup(opts),
			"macOS-style lexical aliases such as /var -> /private/var must compare canonically")
	})
	t.Run("missing symlink", func(t *testing.T) {
		opts := nativeRegistryFixture(t)
		require.NoError(t, os.Remove(opts.SystemDir))
		requireNativeRegistryCode(t, validateCodexNativeRegistrySetup(opts), CodexNativeRegistryMissingSymlink)
	})
	t.Run("wrong target", func(t *testing.T) {
		opts := nativeRegistryFixture(t)
		require.NoError(t, os.Remove(opts.SystemDir))
		require.NoError(t, os.Symlink(filepath.Dir(opts.ManagedDir), opts.SystemDir))
		requireNativeRegistryCode(t, validateCodexNativeRegistrySetup(opts), CodexNativeRegistryWrongTarget)
	})
	t.Run("unsafe target mode", func(t *testing.T) {
		opts := nativeRegistryFixture(t)
		require.NoError(t, os.Chmod(opts.ManagedDir, 0o755))
		requireNativeRegistryCode(t, validateCodexNativeRegistrySetup(opts), CodexNativeRegistryUnsafeMode)
	})
	t.Run("nested symlink", func(t *testing.T) {
		opts := nativeRegistryFixture(t)
		dataDir := filepath.Dir(opts.ManagedDir)
		realData := filepath.Join(filepath.Dir(dataDir), "real-data")
		require.NoError(t, os.Rename(dataDir, realData))
		require.NoError(t, os.Symlink(realData, dataDir))
		requireNativeRegistryCode(t, validateCodexNativeRegistrySetup(opts), CodexNativeRegistryWrongTarget)
	})
	t.Run("unmanaged file", func(t *testing.T) {
		opts := nativeRegistryFixture(t)
		require.NoError(t, os.WriteFile(filepath.Join(opts.ManagedDir, "config.toml"), []byte("enterprise = true\n"), 0o600))
		requireNativeRegistryCode(t, validateCodexNativeRegistrySetup(opts), CodexNativeRegistryConflict)
	})
	t.Run("unsafe file mode", func(t *testing.T) {
		opts := nativeRegistryFixture(t)
		require.NoError(t, os.WriteFile(filepath.Join(opts.ManagedDir, "requirements.toml"),
			[]byte(registryRequirementsMarker), 0o644))
		requireNativeRegistryCode(t, validateCodexNativeRegistrySetup(opts), CodexNativeRegistryUnsafeMode)
	})
	t.Run("managed file symlink", func(t *testing.T) {
		opts := nativeRegistryFixture(t)
		target := filepath.Join(filepath.Dir(opts.ManagedDir), "elsewhere")
		require.NoError(t, os.WriteFile(target, []byte(registryConfigMarker), 0o600))
		require.NoError(t, os.Symlink(target, filepath.Join(opts.ManagedDir, "config.toml")))
		requireNativeRegistryCode(t, validateCodexNativeRegistrySetup(opts), CodexNativeRegistryConflict)
	})
	t.Run("unexpected unmanaged entry", func(t *testing.T) {
		opts := nativeRegistryFixture(t)
		require.NoError(t, os.WriteFile(filepath.Join(opts.ManagedDir, "enterprise.toml"), []byte("policy = true\n"), 0o600))
		requireNativeRegistryCode(t, validateCodexNativeRegistrySetup(opts), CodexNativeRegistryConflict)
	})
}

func TestCodexNativeRegistryConcurrentRegistrationKeepsAllowedProfilesDefined(t *testing.T) {
	opts := nativeRegistryFixture(t)
	previousWriter := writeNativeRegistryFile
	var snapshots sync.Mutex
	writeNativeRegistryFile = func(path string, data []byte) error {
		if err := atomicWriteNativeRegistryFile(path, data); err != nil {
			return err
		}
		config, configErr := os.ReadFile(filepath.Join(opts.ManagedDir, "config.toml"))
		requirements, requirementsErr := os.ReadFile(filepath.Join(opts.ManagedDir, "requirements.toml"))
		if configErr == nil && requirementsErr == nil {
			snapshots.Lock()
			for _, line := range strings.Split(string(requirements), "\n") {
				if !strings.HasPrefix(line, `"tclaude-agent-`) {
					continue
				}
				name := strings.Trim(strings.SplitN(line, "=", 2)[0], " \"")
				assert.Contains(t, string(config), "[permissions."+name+"]",
					"an allowlisted generated profile must already be defined")
			}
			snapshots.Unlock()
		}
		return nil
	}
	t.Cleanup(func() { writeNativeRegistryFile = previousWriter })

	const count = 8
	errs := make(chan error, count)
	var wg sync.WaitGroup
	for i := range count {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("tclaude-agent-%016x", i+1)
			errs <- registerCodexNativePermissionProfile(opts, fmt.Sprintf("generation-%d", i),
				name, nativeProfile(name, fmt.Sprintf("/workspace/%d", i)))
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	profiles, err := db.ListCodexNativePermissionProfiles()
	require.NoError(t, err)
	require.Len(t, profiles, count)
	config, err := os.ReadFile(filepath.Join(opts.ManagedDir, "config.toml"))
	require.NoError(t, err)
	requirements, err := os.ReadFile(filepath.Join(opts.ManagedDir, "requirements.toml"))
	require.NoError(t, err)
	assert.Contains(t, string(config), `default_permissions = ":workspace"`)
	for _, builtin := range []string{":read-only", ":workspace", ":danger-full-access"} {
		assert.Contains(t, string(requirements), fmt.Sprintf("%q = true", builtin))
	}
	for _, profile := range profiles {
		assert.Contains(t, string(config), "[permissions."+profile.ProfileName+"]")
		assert.Contains(t, string(requirements), fmt.Sprintf("%q = true", profile.ProfileName))
	}
	for _, file := range []string{"config.toml", "requirements.toml", "registry.lock"} {
		info, statErr := os.Lstat(filepath.Join(opts.ManagedDir, file))
		require.NoError(t, statErr)
		assert.True(t, info.Mode().IsRegular())
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}
}

func TestCodexNativeRegistryFailedRegistrationRollsBackDurableAndPublishedState(t *testing.T) {
	opts := nativeRegistryFixture(t)
	first := "tclaude-agent-1111111111111111"
	require.NoError(t, registerCodexNativePermissionProfile(opts, "generation-1", first,
		nativeProfile(first, "/workspace/one")))

	second := "tclaude-agent-2222222222222222"
	previousWriter := writeNativeRegistryFile
	t.Cleanup(func() { writeNativeRegistryFile = previousWriter })
	var failed atomic.Bool
	writeNativeRegistryFile = func(path string, data []byte) error {
		if filepath.Base(path) == "requirements.toml" && bytesContain(data, second) && failed.CompareAndSwap(false, true) {
			return errors.New("injected requirements publish failure")
		}
		return atomicWriteNativeRegistryFile(path, data)
	}

	err := registerCodexNativePermissionProfile(opts, "generation-2", second,
		nativeProfile(second, "/workspace/two"))
	require.ErrorContains(t, err, "injected requirements publish failure")
	profiles, listErr := db.ListCodexNativePermissionProfiles()
	require.NoError(t, listErr)
	require.Len(t, profiles, 1)
	assert.Equal(t, first, profiles[0].ProfileName)
	for _, file := range []string{"config.toml", "requirements.toml"} {
		data, readErr := os.ReadFile(filepath.Join(opts.ManagedDir, file))
		require.NoError(t, readErr)
		assert.Contains(t, string(data), first)
		assert.NotContains(t, string(data), second)
	}
}

func TestCodexNativeRegistryRestartReconcilePrunesUnregisteredProfiles(t *testing.T) {
	opts := nativeRegistryFixture(t)
	name := "tclaude-agent-3333333333333333"
	require.NoError(t, registerCodexNativePermissionProfile(opts, "generation-3", name,
		nativeProfile(name, "/workspace/three")))
	require.NoError(t, db.DeleteCodexNativePermissionProfile("generation-3"),
		"simulate a durable rollback completed before daemon restart")
	require.NoError(t, reconcileCodexNativePermissionRegistry(opts))
	for _, file := range []string{"config.toml", "requirements.toml"} {
		data, err := os.ReadFile(filepath.Join(opts.ManagedDir, file))
		require.NoError(t, err)
		assert.NotContains(t, string(data), name)
	}
}

func TestCodexNativeRegistryFailedUnregisterPersistsCleanupForRestartRetry(t *testing.T) {
	opts := nativeRegistryFixture(t)
	name := "tclaude-agent-5555555555555555"
	require.NoError(t, registerCodexNativePermissionProfile(opts, "generation-5", name,
		nativeProfile(name, "/workspace/five")))

	previousWriter := writeNativeRegistryFile
	t.Cleanup(func() { writeNativeRegistryFile = previousWriter })
	var failed atomic.Bool
	writeNativeRegistryFile = func(path string, data []byte) error {
		if filepath.Base(path) == "requirements.toml" && !bytesContain(data, name) &&
			failed.CompareAndSwap(false, true) {
			return errors.New("injected cleanup publish failure")
		}
		return atomicWriteNativeRegistryFile(path, data)
	}
	err := unregisterCodexNativePermissionProfile(opts, "generation-5")
	require.ErrorContains(t, err, "injected cleanup publish failure")
	pending, getErr := db.GetCodexNativePermissionProfile("generation-5")
	require.NoError(t, getErr)
	require.NotNil(t, pending)
	assert.True(t, pending.CleanupPending)

	writeNativeRegistryFile = previousWriter
	require.NoError(t, reconcileCodexNativePermissionRegistry(opts),
		"daemon restart reconciliation retries durable cleanup intent")
	pending, getErr = db.GetCodexNativePermissionProfile("generation-5")
	require.NoError(t, getErr)
	assert.Nil(t, pending)
	for _, file := range []string{"config.toml", "requirements.toml"} {
		data, readErr := os.ReadFile(filepath.Join(opts.ManagedDir, file))
		require.NoError(t, readErr)
		assert.NotContains(t, string(data), name)
	}
}

func TestCodexNativeRegistryStartupSweepRemovesOnlyDefinitiveOrphans(t *testing.T) {
	opts := nativeRegistryFixture(t)
	seed := func(generation, state string, created int) string {
		name := fmt.Sprintf("tclaude-agent-%016x", created)
		require.NoError(t, db.UpsertCodexAppServerRuntime(db.CodexAppServerRuntime{
			Generation: generation, LaunchID: "launch-" + generation, AgentID: "agent",
			ConvID: "conv", SocketPath: filepath.Join(opts.ManagedDir, generation, "app.sock"),
			State: state, CreatedAt: time.Unix(int64(created), 0),
		}))
		require.NoError(t, registerCodexNativePermissionProfile(opts, generation, name,
			nativeProfile(name, "/workspace/"+generation)))
		return name
	}

	liveName := seed("generation-live", db.CodexAppServerReady, 1)
	terminalName := seed("generation-terminal", db.CodexAppServerDead, 2)
	orphanName := "tclaude-agent-0000000000000003"
	require.NoError(t, db.UpsertCodexNativePermissionProfile(db.CodexNativePermissionProfile{
		Generation: "generation-orphan", ProfileName: orphanName,
		ProfileTOML: nativeProfile(orphanName, "/workspace/orphan"),
	}))
	resumableName := "tclaude-agent-0000000000000004"
	managedMode := harness.SandboxManagedProfile
	require.NoError(t, db.SetConversationResumeProfile("resumable-conv", db.ConversationResumeProfile{
		Version: db.RelaunchProfileVersion, Harness: harness.CodexName, Cwd: "/workspace/resumable",
		FallbackRelaunch: &db.AgentRelaunchProfile{
			Version: db.RelaunchProfileVersion, HarnessBuiltinMode: &managedMode,
		},
	}))
	require.NoError(t, db.UpsertCodexNativePermissionProfile(db.CodexNativePermissionProfile{
		Generation: "generation-resumable", ProfileName: resumableName,
		ProfileTOML: nativeProfile(resumableName, "/workspace/resumable"),
		OwnerConvID: "resumable-conv", LaunchID: "stopped-launch", LaunchReady: true,
	}))
	startingName := "tclaude-agent-0000000000000005"
	require.NoError(t, db.UpsertCodexNativePermissionProfile(db.CodexNativePermissionProfile{
		Generation: "generation-starting", ProfileName: startingName,
		ProfileTOML: nativeProfile(startingName, "/workspace/starting"),
		LaunchID:    "starting-launch", LaunchReady: false,
	}))
	starting, err := db.GetCodexNativePermissionProfile("generation-starting")
	require.NoError(t, err)
	require.NotNil(t, starting)
	require.WithinDuration(t, time.Now(), starting.CreatedAt, time.Second)

	require.NoError(t, markOrphanedCodexNativePermissionProfiles())
	require.NoError(t, reconcileCodexNativePermissionRegistry(opts))
	profiles, err := db.ListCodexNativePermissionProfiles()
	require.NoError(t, err)
	require.Len(t, profiles, 3)
	assert.ElementsMatch(t, []string{"generation-live", "generation-resumable", "generation-starting"},
		[]string{profiles[0].Generation, profiles[1].Generation, profiles[2].Generation})
	config, err := os.ReadFile(filepath.Join(opts.ManagedDir, "config.toml"))
	require.NoError(t, err)
	assert.Contains(t, string(config), liveName)
	assert.Contains(t, string(config), resumableName)
	assert.Contains(t, string(config), startingName)
	assert.NotContains(t, string(config), terminalName)
	assert.NotContains(t, string(config), orphanName)
}

func TestCodexNativeRegistryKeepsPredecessorUntilSuccessorReady(t *testing.T) {
	opts := nativeRegistryFixture(t)
	oldName := "tclaude-agent-0000000000000011"
	newName := "tclaude-agent-0000000000000012"
	for _, runtime := range []db.CodexAppServerRuntime{
		{Generation: "old", LaunchID: "launch-old", AgentID: "agent", ConvID: "conv",
			SocketPath: filepath.Join(opts.ManagedDir, "old", "app.sock"), State: db.CodexAppServerReady,
			CreatedAt: time.Unix(11, 0)},
		{Generation: "new", LaunchID: "launch-new", AgentID: "agent", ConvID: "conv",
			SocketPath: filepath.Join(opts.ManagedDir, "new", "app.sock"), State: db.CodexAppServerWarming,
			CreatedAt: time.Unix(12, 0)},
	} {
		require.NoError(t, db.UpsertCodexAppServerRuntime(runtime))
	}
	require.NoError(t, registerCodexNativePermissionProfile(opts, "old", oldName,
		nativeProfile(oldName, "/workspace/old")))
	require.NoError(t, registerCodexNativePermissionProfile(opts, "new", newName,
		nativeProfile(newName, "/workspace/new")))
	require.NoError(t, markOrphanedCodexNativePermissionProfiles())
	require.NoError(t, reconcileCodexNativePermissionRegistry(opts))
	profiles, err := db.ListCodexNativePermissionProfiles()
	require.NoError(t, err)
	require.Len(t, profiles, 2, "warming successor cannot evict predecessor enforcement")

	newRuntime, err := db.GetCodexAppServerRuntime("new")
	require.NoError(t, err)
	newRuntime.State = db.CodexAppServerReady
	require.NoError(t, db.UpsertCodexAppServerRuntime(*newRuntime))
	require.NoError(t, reconcileCodexNativePermissionRegistry(opts))
	profiles, err = db.ListCodexNativePermissionProfiles()
	require.NoError(t, err)
	require.Len(t, profiles, 1)
	assert.Equal(t, "new", profiles[0].Generation)
	config, err := os.ReadFile(filepath.Join(opts.ManagedDir, "config.toml"))
	require.NoError(t, err)
	assert.NotContains(t, string(config), oldName)
	assert.Contains(t, string(config), newName)
}

func TestOrdinaryGeneratedCodexProfileParticipatesOnlyWhenRegistryInstalled(t *testing.T) {
	opts := nativeRegistryFixture(t)
	previousAdopter := codexNativeRegistryBeforeOrdinaryPublish
	adoptions := 0
	codexNativeRegistryBeforeOrdinaryPublish = func() error {
		adoptions++
		return nil
	}
	t.Cleanup(func() { codexNativeRegistryBeforeOrdinaryPublish = previousAdopter })
	name := "tclaude-agent-0000000000000021"
	path := filepath.Join(t.TempDir(), name+".config.toml")
	require.NoError(t, os.WriteFile(path, []byte(nativeProfile(name, "/workspace/ordinary")), 0o600))
	profile := db.CodexNativePermissionProfile{
		Generation: "launch:ordinary", ProfileName: name, OwnerAgentID: "agent",
		OwnerConvID: "conv", LaunchID: "launch", LaunchReady: false,
	}
	registered, err := registerCodexNativePermissionProfileIfInstalled(opts, profile, path)
	require.NoError(t, err)
	require.True(t, registered)
	assert.Equal(t, 1, adoptions, "every installed-registry ordinary publisher protects existing live profiles first")
	stored, err := db.GetCodexNativePermissionProfile(profile.Generation)
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, "conv", stored.OwnerConvID)
	assert.False(t, stored.LaunchReady)

	require.NoError(t, os.Remove(opts.SystemDir))
	skippedName := "tclaude-agent-0000000000000022"
	skippedPath := filepath.Join(t.TempDir(), skippedName+".config.toml")
	require.NoError(t, os.WriteFile(skippedPath,
		[]byte(nativeProfile(skippedName, "/workspace/skipped")), 0o600))
	registered, err = registerCodexNativePermissionProfileIfInstalled(opts,
		db.CodexNativePermissionProfile{Generation: "launch:skipped", ProfileName: skippedName}, skippedPath)
	require.NoError(t, err)
	assert.False(t, registered, "missing optional registry must not block an ordinary launch")
	assert.Equal(t, 1, adoptions, "an absent registry must skip without broadening launch preflight")
	skipped, err := db.GetCodexNativePermissionProfile("launch:skipped")
	require.NoError(t, err)
	assert.Nil(t, skipped)
}

func TestFirstActivationBatchPublishesEveryLiveProfileTogether(t *testing.T) {
	opts := nativeRegistryFixture(t)
	registrations := make([]CodexNativePermissionProfileRegistration, 0, 2)
	var names []string
	for i := 1; i <= 2; i++ {
		name := fmt.Sprintf("tclaude-agent-%016x", 40+i)
		path := filepath.Join(t.TempDir(), name+".config.toml")
		require.NoError(t, os.WriteFile(path,
			[]byte(nativeProfile(name, fmt.Sprintf("/workspace/live-%d", i))), 0o600))
		names = append(names, name)
		registrations = append(registrations, CodexNativePermissionProfileRegistration{
			Profile: db.CodexNativePermissionProfile{
				Generation: fmt.Sprintf("adopted:%d", i), ProfileName: name,
				LaunchID: fmt.Sprintf("launch-%d", i), LaunchReady: true,
			},
			ProfilePath: path,
		})
	}
	previousWriter := writeNativeRegistryFile
	requirementsWrites := 0
	writeNativeRegistryFile = func(path string, data []byte) error {
		if filepath.Base(path) == "requirements.toml" {
			requirementsWrites++
			for _, name := range names {
				assert.Contains(t, string(data), name,
					"first global publication must include the complete proved-live set")
			}
		}
		return atomicWriteNativeRegistryFile(path, data)
	}
	t.Cleanup(func() { writeNativeRegistryFile = previousWriter })

	registered, err := registerCodexNativePermissionProfilesIfInstalled(opts, registrations)
	require.NoError(t, err)
	assert.True(t, registered)
	assert.Equal(t, 1, requirementsWrites)
}

func TestCodexNativeRegistryFailedRestoreRemainsDurableForRestartRetry(t *testing.T) {
	opts := nativeRegistryFixture(t)
	name := "tclaude-agent-7777777777777777"
	previousWriter := writeNativeRegistryFile
	t.Cleanup(func() { writeNativeRegistryFile = previousWriter })
	writeNativeRegistryFile = func(path string, data []byte) error {
		if filepath.Base(path) == "requirements.toml" && bytesContain(data, name) {
			return errors.New("injected restore publish failure")
		}
		return atomicWriteNativeRegistryFile(path, data)
	}
	err := restoreCodexNativePermissionProfile(opts, "generation-7", name,
		nativeProfile(name, "/workspace/seven"))
	require.ErrorContains(t, err, "injected restore publish failure")
	restored, getErr := db.GetCodexNativePermissionProfile("generation-7")
	require.NoError(t, getErr)
	require.NotNil(t, restored, "compensating restore must survive a publication failure")
	assert.False(t, restored.CleanupPending)

	writeNativeRegistryFile = previousWriter
	require.NoError(t, reconcileCodexNativePermissionRegistry(opts))
	data, readErr := os.ReadFile(filepath.Join(opts.ManagedDir, "requirements.toml"))
	require.NoError(t, readErr)
	assert.Contains(t, string(data), name)
}

func TestValidateStoredNativeProfileRejectsExtraConfiguration(t *testing.T) {
	_ = nativeRegistryFixture(t)
	name := "tclaude-agent-4444444444444444"
	content := nativeProfile(name, "/workspace/four") + "\n[mcp_servers.attacker]\ncommand = \"sh\"\n"
	require.ErrorContains(t, validateStoredNativeProfile(name, content), "decode generated profile TOML")
}

func bytesContain(data []byte, needle string) bool { return strings.Contains(string(data), needle) }
