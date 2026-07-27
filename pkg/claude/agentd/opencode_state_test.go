//go:build linux || darwin

package agentd

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc/agentipctest"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/opencodeapi"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"golang.org/x/sys/unix"
)

func TestOpenCodeUnixRelayBuildsV4WithoutChangingPublicPostureGate(t *testing.T) {
	setupTestDB(t)
	shortHome := agentipctest.ShortSocketDir(t)
	t.Setenv("HOME", shortHome)
	t.Setenv("USERPROFILE", shortHome)
	shortData, err := os.MkdirTemp("/tmp", "tcl780-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(shortData) })
	t.Setenv("XDG_DATA_HOME", shortData)
	db.ResetForTest()
	cwd := filepath.Join(shortHome, "work")
	require.NoError(t, os.MkdirAll(cwd, 0o700))
	agentID := "agt_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	allocation, err := allocatePrivateOpenCodeState(agentID)
	require.NoError(t, err)
	snapshot := sandboxpolicy.EmptySnapshot()
	snapshot.Effective.NetworkAccess = sandboxpolicy.NetworkAccessNone

	_, err = openCodeTclaudeLayerLaunchSpec(
		string(sandboxpolicy.ImplementationTclaudeLayer),
		cwd, nil, &snapshot, agentID)
	require.ErrorContains(t, err, "requires hosted model traffic",
		"the Unix engine must not default-enable the isolated posture")

	spec, err := openCodeUnixRelayLaunchSpec(
		string(sandboxpolicy.ImplementationTclaudeLayer),
		cwd, nil, &snapshot, agentID)
	if runtime.GOOS != "linux" {
		require.ErrorContains(t, err, "Linux-only",
			"Darwin must retain its existing unsupported server-wrap boundary")
		return
	}
	require.NoError(t, err)
	require.Equal(t, session.TclaudeLayerUnixRelaySpecVersion, spec.Version)
	require.NotNil(t, spec.Contract.OpenCodeControl)
	controlPath := filepath.Join(allocation.StateRoot, "control.sock")
	assert.Equal(t, controlPath, spec.Contract.OpenCodeControl.SocketPath)
	assert.Equal(t, session.TclaudeLayerUnixRelayTransport,
		spec.Contract.OpenCodeControl.Transport)
	for _, bind := range spec.Contract.ReadOnlyBinds {
		assert.NotEqual(t, controlPath, bind.Source)
		assert.NotEqual(t, controlPath, bind.Target)
	}

	listener, device, inode, err := opencodeapi.CreateUnixListener(controlPath)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = listener.Close()
		_ = os.Remove(controlPath)
	})
	require.NoError(t, session.ValidateTclaudeLayerLaunchSpec(*spec))
	encodedV4, err := json.Marshal(spec)
	require.NoError(t, err)
	v4Runtime := db.OpenCodeRuntime{
		Cwd: cwd, SandboxImplementation: string(sandboxpolicy.ImplementationTclaudeLayer),
		SandboxLaunchSpecJSON: string(encodedV4),
		Transport:             db.OpenCodeTransportUnixRelay,
		ControlSocketPath:     controlPath, ControlSocketDevice: device,
		ControlSocketInode: inode,
	}
	replayed, err := openCodeRuntimeSandboxSpec(v4Runtime)
	require.NoError(t, err)
	require.Equal(t, session.TclaudeLayerUnixRelaySpecVersion, replayed.Version)
	missingRuntimeAuthority := v4Runtime
	missingRuntimeAuthority.ControlSocketInode = 0
	_, err = openCodeRuntimeSandboxSpec(missingRuntimeAuthority)
	require.ErrorContains(t, err, "incomplete socket authority")

	hostSpec, err := openCodeTclaudeLayerLaunchSpec(
		string(sandboxpolicy.ImplementationTclaudeLayer),
		cwd, nil, nil, agentID)
	require.NoError(t, err)
	encodedV3, err := json.Marshal(hostSpec)
	require.NoError(t, err)
	v3ClaimingUnix := v4Runtime
	v3ClaimingUnix.SandboxLaunchSpecJSON = string(encodedV3)
	_, err = openCodeRuntimeSandboxSpec(v3ClaimingUnix)
	require.ErrorContains(t, err, "host-open loopback control plane")
	if runtime.GOOS == "linux" {
		agentdSocket := filepath.Join(shortHome, ".tclaude", "api", "agentd.sock")
		require.NoError(t, os.MkdirAll(filepath.Dir(agentdSocket), 0o700))
		agentdListener, listenErr := net.Listen("unix", agentdSocket)
		require.NoError(t, listenErr)
		t.Cleanup(func() { _ = agentdListener.Close() })
		require.NoError(t, listener.Close())
		require.NoError(t, os.Remove(controlPath))
		previousResolver := resolveOpenCodeTclaudeLayer
		resolveOpenCodeTclaudeLayer = func(
			sandboxpolicy.NetworkPosture,
		) (string, harness.LaunchOSSandbox, error) {
			return "/usr/bin/bwrap", harness.LaunchOSSandbox{}, nil
		}
		previousRelayExecutable := openCodeRelayExecutable
		openCodeRelayExecutable = os.Executable
		t.Cleanup(func() {
			resolveOpenCodeTclaudeLayer = previousResolver
			openCodeRelayExecutable = previousRelayExecutable
		})
		command, args, extraFiles, handshake, cleanup, renderErr := openCodeServeProcessExec(
			"/usr/bin/opencode", "43210", &v4Runtime, spec)
		require.NoError(t, renderErr)
		require.NotEmpty(t, command)
		require.Len(t, extraFiles, 2,
			"ExtraFiles must map authority status→fd3 and durable-ack gate→fd4")
		require.NotNil(t, handshake)
		t.Cleanup(func() {
			cleanup()
		})
		argv := append([]string{command}, args...)
		joined := strings.Join(argv, " ")
		serverJoined := strings.Join(args[3:], " ")
		assert.Contains(t, joined, opencodeapi.UnixLaunchMode+" "+controlPath+" -- /usr/bin/bwrap")
		assert.Contains(t, serverJoined, "--unshare-net")
		assert.Contains(t, serverJoined, "--unshare-pid")
		assert.NotContains(t, serverJoined, "--preserve-fds",
			"upstream bubblewrap preserves inherited non-CLOEXEC fds without a flag")
		assert.Contains(t, serverJoined,
			"/proc/self/fd/4 "+opencodeapi.InheritedUnixRelayMode+" 3 ",
			"the relay executable and listener must retain their exact fd mapping")
		assert.NotContains(t, serverJoined, controlPath,
			"the server namespace must receive only the inherited control fd")
		assert.NotContains(t, serverJoined, ".tclaude-opencode-v4-state",
			"the server mount plan must not rely on a namespace-created bind source")
		assert.NotContains(t, serverJoined, "--ro-bind "+controlPath+" "+controlPath)
		assert.NotContains(t, serverJoined, "--bind "+controlPath+" "+controlPath)
	}

	missing := *spec
	missing.Contract.OpenCodeControl = nil
	require.ErrorContains(t, session.ValidateTclaudeLayerLaunchSpec(missing),
		"no Unix-relay control authority")
	v3Claim := *spec
	v3Claim.Version = session.TclaudeLayerLaunchSpecVersion
	require.ErrorContains(t, session.ValidateTclaudeLayerLaunchSpec(v3Claim),
		"unexpectedly carries OpenCode Unix control authority")
}

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
	for _, name := range []string{
		"XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME",
	} {
		t.Setenv(name, "")
	}
	home := os.Getenv("HOME")
	cwd := filepath.Join(home, "work")
	config := filepath.Join(home, ".config", "opencode")
	install := filepath.Join(home, ".opencode")
	for _, path := range []string{cwd, config, filepath.Join(install, "bin")} {
		require.NoError(t, os.MkdirAll(path, 0o700))
	}
	resolvedConfig, err := filepath.EvalSymlinks(config)
	require.NoError(t, err)
	resolvedInstall, err := filepath.EvalSymlinks(install)
	require.NoError(t, err)
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
		session.TclaudeLayerReadOnlyBind{Source: resolvedConfig, Target: resolvedConfig})
	assert.Contains(t, spec.Contract.ReadOnlyBinds,
		session.TclaudeLayerReadOnlyBind{Source: resolvedInstall, Target: resolvedInstall})
	raw, err := os.ReadFile(filepath.Join(install, openCodeInstallBootstrapFile))
	require.NoError(t, err)
	assert.Equal(t, openCodeInstallGitignore, string(raw),
		"legacy replay must bootstrap before applying the same read-only install bind")

	if runtime.GOOS != "linux" {
		return
	}
	shortData, err := os.MkdirTemp("/tmp", "tcl780-legacy-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(shortData) })
	t.Setenv("XDG_DATA_HOME", shortData)
	isolated := sandboxpolicy.EmptySnapshot()
	isolated.Effective.NetworkAccess = sandboxpolicy.NetworkAccessNone
	v4, err := openCodeUnixRelayLaunchSpec(
		string(sandboxpolicy.ImplementationTclaudeLayer),
		cwd, nil, &isolated, agentID)
	require.NoError(t, err)
	require.Equal(t, session.TclaudeLayerUnixRelaySpecVersion, v4.Version)
	controlPath := filepath.Join(v4.Contract.FinalHideDirs[0], agentID, "control.sock")
	require.Equal(t, controlPath, v4.Contract.OpenCodeControl.SocketPath)
	assert.Empty(t, v4.Contract.PrivateWriteDirs,
		"legacy state stays shared; its private child is control authority only")
	stillLegacy, err := db.GetOpenCodeAgentStateAllocation(agentID)
	require.NoError(t, err)
	require.Equal(t, db.OpenCodeStateLegacyShared, stillLegacy.Mode)
	assert.Empty(t, stillLegacy.StateRoot)
	control, _, _, err := opencodeapi.CreateUnixListener(controlPath)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = control.Close()
		_ = os.Remove(controlPath)
	})
	require.NoError(t, session.ValidateTclaudeLayerLaunchSpec(*v4))
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

	symlinkDestinationSourceDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(symlinkDestinationSourceDir, "auth.json"), []byte("ambient"), 0o600))
	symlinkDestinationDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(symlinkDestinationDir, "elsewhere.json"), []byte("elsewhere"), 0o600))
	require.NoError(t, os.Symlink(
		"elsewhere.json", filepath.Join(symlinkDestinationDir, "auth.json")))
	err = seedOpenCodeCredentials(symlinkDestinationSourceDir, symlinkDestinationDir)
	require.ErrorContains(t, err, "existing private OpenCode credential")

	fifoDestinationDir := t.TempDir()
	require.NoError(t, unix.Mkfifo(filepath.Join(fifoDestinationDir, "auth.json"), 0o600))
	err = seedOpenCodeCredentials(symlinkDestinationSourceDir, fifoDestinationDir)
	require.ErrorContains(t, err, "not a regular file")
}

func TestOpenCodeInstallGitignoreSeedAbsentPresentAndRefusesSpecialFiles(t *testing.T) {
	absent := t.TempDir()
	require.NoError(t, ensureOpenCodeInstallGitignore(absent))
	path := filepath.Join(absent, openCodeInstallBootstrapFile)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, openCodeInstallGitignore, string(raw))
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	present := t.TempDir()
	presentPath := filepath.Join(present, openCodeInstallBootstrapFile)
	require.NoError(t, os.WriteFile(presentPath, []byte("operator-owned"), 0o640))
	require.NoError(t, ensureOpenCodeInstallGitignore(present))
	raw, err = os.ReadFile(presentPath)
	require.NoError(t, err)
	assert.Equal(t, "operator-owned", string(raw))
	info, err = os.Stat(presentPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o640), info.Mode().Perm())

	symlink := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(symlink, "elsewhere"), []byte("x"), 0o600))
	require.NoError(t, os.Symlink("elsewhere",
		filepath.Join(symlink, openCodeInstallBootstrapFile)))
	require.ErrorContains(t, ensureOpenCodeInstallGitignore(symlink),
		"existing OpenCode install bootstrap")

	fifo := t.TempDir()
	require.NoError(t, unix.Mkfifo(filepath.Join(fifo, openCodeInstallBootstrapFile), 0o600))
	require.ErrorContains(t, ensureOpenCodeInstallGitignore(fifo), "not a regular file")
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
