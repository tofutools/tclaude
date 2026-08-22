//go:build linux || darwin

package agentd

import (
	"database/sql"
	"encoding/json"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc/agentipctest"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/opencodeapi"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"golang.org/x/sys/unix"
)

func TestRefuseOpenCodeFilteredActiveAccount(t *testing.T) {
	stateRoot := t.TempDir()
	require.NoError(t, refuseOpenCodeFilteredActiveAccount(stateRoot),
		"a fresh private state has no account authority")

	databaseDir := filepath.Join(stateRoot, "data", "opencode")
	require.NoError(t, os.MkdirAll(databaseDir, 0o700))
	databasePath := filepath.Join(databaseDir, "opencode.db")
	store, err := sql.Open("sqlite", databasePath)
	require.NoError(t, err)
	_, err = store.Exec(`CREATE TABLE account_state (
		id INTEGER PRIMARY KEY,
		active_account_id TEXT,
		active_org_id TEXT
	)`)
	require.NoError(t, err)
	_, err = store.Exec(
		`INSERT INTO account_state(id, active_account_id, active_org_id) VALUES (1, NULL, NULL)`)
	require.NoError(t, err)
	require.NoError(t, store.Close())
	require.NoError(t, refuseOpenCodeFilteredActiveAccount(stateRoot))

	store, err = sql.Open("sqlite", databasePath)
	require.NoError(t, err)
	_, err = store.Exec(
		`UPDATE account_state SET active_account_id = 'account', active_org_id = 'org' WHERE id = 1`)
	require.NoError(t, err)
	require.NoError(t, store.Close())
	err = refuseOpenCodeFilteredActiveAccount(stateRoot)
	require.ErrorContains(t, err, "active persistent account/org")
	require.ErrorContains(t, err, "network open")
}

func plantOpenCodeFilteredActiveAccount(
	t *testing.T,
	stateRoot, accountURL string,
) {
	t.Helper()
	databaseDir := filepath.Join(stateRoot, "data", "opencode")
	require.NoError(t, os.MkdirAll(databaseDir, 0o700))
	databasePath := filepath.Join(databaseDir, "opencode.db")
	dsn := (&url.URL{
		Scheme: "file", Path: databasePath,
		RawQuery: "_pragma=busy_timeout(5000)",
	}).String()
	store, err := sql.Open("sqlite", dsn)
	require.NoError(t, err)
	_, err = store.Exec(`CREATE TABLE IF NOT EXISTS account (
		id TEXT PRIMARY KEY,
		email TEXT NOT NULL,
		url TEXT NOT NULL,
		access_token TEXT NOT NULL,
		refresh_token TEXT NOT NULL,
		token_expiry INTEGER,
		time_created INTEGER NOT NULL,
		time_updated INTEGER NOT NULL
	)`)
	require.NoError(t, err)
	_, err = store.Exec(`CREATE TABLE IF NOT EXISTS account_state (
		id INTEGER PRIMARY KEY,
		active_account_id TEXT,
		active_org_id TEXT
	)`)
	require.NoError(t, err)
	_, err = store.Exec(
		`INSERT INTO account(
			id, email, url, access_token, refresh_token, token_expiry,
			time_created, time_updated
		 ) VALUES (
			'account', 'fixture@example.invalid', ?, 'access', 'refresh',
			4102444800000, 1, 1
		 )
		 ON CONFLICT(id) DO UPDATE SET
			email = excluded.email,
			url = excluded.url,
			access_token = excluded.access_token,
			refresh_token = excluded.refresh_token,
			token_expiry = excluded.token_expiry,
			time_updated = excluded.time_updated`,
		accountURL)
	require.NoError(t, err)
	_, err = store.Exec(
		`INSERT INTO account_state(id, active_account_id, active_org_id)
		 VALUES (1, 'account', 'org')
		 ON CONFLICT(id) DO UPDATE SET
			active_account_id = excluded.active_account_id,
			active_org_id = excluded.active_org_id`)
	require.NoError(t, err)
	require.NoError(t, store.Close())
}

func TestOpenCodeUnixRelayBuildsV4ForIsolatedSmokeAndFilteredPublicLaunch(t *testing.T) {
	setupTestDB(t)
	shortHome := agentipctest.ShortSocketDir(t)
	t.Setenv("HOME", shortHome)
	t.Setenv("USERPROFILE", shortHome)
	shortData, err := os.MkdirTemp("/tmp", "tcl780-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(shortData) })
	t.Setenv("XDG_DATA_HOME", shortData)
	db.ResetForTest()
	cleanupAgentdTestDB(t)
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
		agentdSocket := agentipc.CanonicalSocketPath()
		require.NoError(t, os.MkdirAll(filepath.Dir(agentdSocket), 0o700))
		agentdListener, listenErr := net.Listen("unix", agentdSocket)
		require.NoError(t, listenErr)
		t.Cleanup(func() { _ = agentdListener.Close() })
		require.NoError(t, listener.Close())
		require.NoError(t, os.Remove(controlPath))
		previousResolver := resolveOpenCodeTclaudeLayer
		resolveOpenCodeTclaudeLayer = func(
			_ sandboxpolicy.NetworkPosture,
			_ sandboxpolicy.RootPosture,
			_ sandboxpolicy.NetworkEngine,
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

		filteredSnapshot := sandboxpolicy.EmptySnapshot()
		filteredSnapshot.Effective.Network = &sandboxpolicy.NetworkRules{
			Mode: sandboxpolicy.AccessModeList,
			Allow: []sandboxpolicy.NetworkAllowEntry{{
				Loopback: true, Ports: []int{43210},
			}},
		}
		filteredSnapshot.Effective.Environment = []sandboxpolicy.EnvironmentEntry{{
			Name:  "OPENCODE_CONFIG_CONTENT",
			Value: `{"enabled_providers":["test"],"provider":{"test":{"npm":"@ai-sdk/openai-compatible","whitelist":["model"],"models":{"model":{}},"options":{"baseURL":"http://host.tclaude.internal:43210","apiKey":"test"}}}}`,
		}}
		filteredSpec, filteredErr := openCodeTclaudeLayerLaunchSpec(
			string(sandboxpolicy.ImplementationTclaudeLayer),
			cwd, nil, &filteredSnapshot, agentID)
		require.NoError(t, filteredErr)
		require.Equal(t, session.TclaudeLayerUnixRelaySpecVersion, filteredSpec.Version)
		require.NoError(t, validateOpenCodeV3LaunchContract(
			filteredSpec.Contract, false))
		require.Len(t, filteredSpec.Contract.Environment, 4)
		assert.Equal(t, sandboxpolicy.EnvironmentEntry{
			Name: "XDG_CONFIG_HOME",
			Value: filepath.Join(
				filteredSpec.Contract.StateRoot, "config"),
		}, filteredSpec.Contract.Environment[2])
		assert.NotEmpty(t, openCodeReadOnlyConfigBindSource(filteredSpec.Contract),
			"filtered launches must retain the read-only harness config projection")
		require.NoError(t, validateOpenCodeFilteredProviderAuthority(filteredSpec))
		listenerFD, executableFD, fdErr :=
			session.TclaudeLayerUnixRelayServerFDs(*filteredSpec)
		require.NoError(t, fdErr)
		assert.Equal(t, 8, listenerFD)
		assert.Equal(t, 9, executableFD)

		filteredCommand, filteredArgs, filteredExtraFiles, filteredHandshake,
			filteredCleanup, filteredRenderErr := openCodeServeProcessExec(
			"/usr/bin/opencode", "43210", &v4Runtime, filteredSpec)
		require.NoError(t, filteredRenderErr)
		require.NotEmpty(t, filteredCommand)
		require.Len(t, filteredExtraFiles, 2)
		require.NotNil(t, filteredHandshake)
		t.Cleanup(filteredCleanup)
		filteredJoined := strings.Join(filteredArgs, " ")
		assert.Contains(t, filteredJoined,
			"-- /proc/self/fd/4 session tclaude-layer-winch-relay")
		assert.Contains(t, filteredJoined, "--preserve-fds 2")
		assert.Contains(t, filteredJoined, "--filtered-network-policy")
		configSource := openCodeReadOnlyConfigBindSource(filteredSpec.Contract)
		configTarget := filepath.Join(filteredSpec.Contract.StateRoot, "config", "opencode")
		assert.Contains(t, filteredJoined,
			"--ro-bind "+configSource+" "+configTarget,
			"filtered launches must mount the harness config read-only")
		assert.NotContains(t, filteredJoined, openCodeFilteredConfigBase)
		assert.NotContains(t, filteredJoined, openCodeFilteredHomeBase)
		assert.Contains(t, filteredJoined,
			"/proc/self/fd/9 "+opencodeapi.InheritedUnixRelayMode+" 8 ",
			"the filtered supervisor must remap both inherited authorities after its sealed gateway fds")
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
	require.NoError(t, validateOpenCodeV3LaunchContract(specA.Contract, false))
	require.Len(t, specA.Contract.StateDirs, 4)
	wantEnvironment := []sandboxpolicy.EnvironmentEntry{
		{Name: "XDG_DATA_HOME", Value: filepath.Join(allocationA.StateRoot, "data")},
		{Name: "XDG_CACHE_HOME", Value: filepath.Join(allocationA.StateRoot, "cache")},
		{Name: "XDG_CONFIG_HOME", Value: filepath.Join(allocationA.StateRoot, "config")},
		{Name: "XDG_STATE_HOME", Value: filepath.Join(allocationA.StateRoot, "state")},
	}
	wantConfigBind := session.TclaudeLayerReadOnlyBind{
		Source: ambientConfig,
		Target: filepath.Join(allocationA.StateRoot, "config", "opencode"),
	}
	if runtime.GOOS == "darwin" {
		wantEnvironment[2].Value = filepath.Join(home, "ambient-config")
		wantConfigBind.Target = ambientConfig
	}
	require.Equal(t, wantEnvironment, specA.Contract.Environment)
	assert.NotContains(t, environmentNames(specA.Contract.Environment), "OPENCODE_CONFIG_DIR")
	assert.ElementsMatch(t, []string{
		filepath.Join(home, "ambient-data", "opencode"),
		filepath.Join(home, "ambient-cache", "opencode"),
		filepath.Join(home, "ambient-state", "opencode"),
	}, specA.Contract.FinalHideDirs)
	require.Contains(t, specA.Contract.ReadOnlyBinds, wantConfigBind)
	require.Contains(t, specA.Contract.ReadOnlyBinds, session.TclaudeLayerReadOnlyBind{
		Source: install, Target: install,
	})
	missingEnvironment := specA.Contract
	missingEnvironment.Environment = nil
	require.ErrorContains(t, validateOpenCodeV3LaunchContract(missingEnvironment, false),
		"no enforced XDG environment")
	missingPrivatePair := specA.Contract
	missingPrivatePair.PrivateWriteDirs = nil
	require.ErrorContains(t, validateOpenCodeV3LaunchContract(missingPrivatePair, false),
		"does not hide siblings")
	missingLegacyHides := specA.Contract
	missingLegacyHides.FinalHideDirs = nil
	require.ErrorContains(t, validateOpenCodeV3LaunchContract(missingLegacyHides, false),
		"must hide the three ambient")
	missingConfigBind := specA.Contract
	missingConfigBind.ReadOnlyBinds = []session.TclaudeLayerReadOnlyBind{
		{Source: install, Target: install},
	}
	require.ErrorContains(t, validateOpenCodeV3LaunchContract(missingConfigBind, false),
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

func TestOpenCodeRuntimePathsEquivalentAcceptsStableLeafSymlink(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "dotfiles", "opencode-global")
	require.NoError(t, os.MkdirAll(target, 0o700))
	configBase := filepath.Join(base, "config")
	require.NoError(t, os.MkdirAll(configBase, 0o700))
	require.NoError(t, os.Symlink(target, filepath.Join(configBase, "opencode")))

	assert.True(t, openCodeRuntimePathsEquivalent(
		target, filepath.Join(configBase, "opencode")))
	assert.False(t, openCodeRuntimePathsEquivalent(
		filepath.Join(base, "other"), filepath.Join(configBase, "opencode")))

	aliasBase := filepath.Join(t.TempDir(), "base-alias")
	require.NoError(t, os.Symlink(base, aliasBase))
	assert.True(t, openCodeRuntimePathsEquivalent(
		filepath.Join(aliasBase, "dotfiles", "opencode-global"),
		filepath.Join(configBase, "opencode")),
		"both sides may carry a stable parent alias, as /var does on macOS")
}

func environmentNames(entries []sandboxpolicy.EnvironmentEntry) string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name)
	}
	return strings.Join(names, ",")
}

// TestOpenCodeInvalidAgentIDIsMatchableFromBothProducers pins the property
// TCL-911 restores: the two functions that refuse a malformed agent id render
// ONE operator-visible sentence and both answer errors.Is with that sentence's
// sentinel. Text equality is asserted on full Error() strings so a fix that
// changed the wording would be caught here rather than by an operator.
//
// BOTH, not EVERY: the two producers are enumerated by hand, so a third one
// added later emitting the literal — the exact failure mode this closes — would
// not fail here. That invariant is carried by errOpenCodeInvalidAgentID's doc
// comment, not by this assertion.
//
// The text assertions hold on the pre-fix tree as well — that is the point.
// What fails without the fix is only the errors.Is assertion on
// allocatePrivateOpenCodeState, which is exactly the drift being closed.
func TestOpenCodeInvalidAgentIDIsMatchableFromBothProducers(t *testing.T) {
	setupTestDB(t)
	const badID = "agt_not-hex"
	const wantSentence = `invalid OpenCode state agent id "agt_not-hex"`

	allocated, allocErr := allocatePrivateOpenCodeState(badID)
	require.Error(t, allocErr)
	require.Nil(t, allocated)
	required, requireErr := requireOpenCodeStateAllocation(badID)
	require.Error(t, requireErr)
	require.Nil(t, required)

	assert.Equal(t, wantSentence, allocErr.Error(),
		"allocatePrivateOpenCodeState must not change the operator-visible sentence")
	assert.Equal(t, wantSentence, requireErr.Error(),
		"requireOpenCodeStateAllocation must not change the operator-visible sentence")
	assert.Equal(t, requireErr.Error(), allocErr.Error(),
		"both producers must render byte-identically")

	assert.ErrorIs(t, allocErr, errOpenCodeInvalidAgentID,
		"a producer whose text matches the sentinel must also be matchable by errors.Is")
	assert.ErrorIs(t, requireErr, errOpenCodeInvalidAgentID)

	// The deliberately-similar sibling must NOT match: it is a different
	// sentence about a different subject (a stored allocation's recorded id,
	// not a caller-supplied one), so a caller acting on errOpenCodeInvalidAgentID
	// must not swallow it.
	sibling, siblingErr := validateOpenCodeStateAllocation(
		db.OpenCodeAgentStateAllocation{AgentID: badID, Mode: db.OpenCodeStatePrivate})
	require.Error(t, siblingErr)
	require.Nil(t, sibling)
	assert.Equal(t, `invalid OpenCode state allocation agent id "agt_not-hex"`,
		siblingErr.Error())
	assert.NotErrorIs(t, siblingErr, errOpenCodeInvalidAgentID)

	// A well-formed id must not reach the sentinel at all, so the assertions
	// above are about the id rule rather than about every refusal this
	// allocation authority can produce.
	_, missingErr := requireOpenCodeStateAllocation(
		"agt_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	require.Error(t, missingErr)
	assert.ErrorContains(t, missingErr, "refusing shared-state fallback")
	assert.NotErrorIs(t, missingErr, errOpenCodeInvalidAgentID)
}

// The replay arm, which had no test at all until a cold review's mutation
// reverted it and nothing failed (TCL-909).
//
// This is the path an isolated or filtered agent takes on RESTART, and it is
// where a stranded allocation was worst served: openCodeControlSocketPath
// produced a sentence naming the cause and the way out, and the caller threw it
// away in favour of "control path is outside its allocated agent authority" — a
// verdict about a comparison that never ran, because the authority could not be
// computed at all.
//
// Pinned end to end through the production replay entry point rather than by
// calling the inner helper, because the defect was in the CALLER's handling of
// the helper's error and a direct call would not have exercised it.
func TestOpenCodeRuntimeSandboxSpecSurfacesAStrandedControlAuthority(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the Unix relay posture is Linux-only")
	}
	setupTestDB(t)
	shortHome := agentipctest.ShortSocketDir(t)
	t.Setenv("HOME", shortHome)
	t.Setenv("USERPROFILE", shortHome)
	shortData, err := os.MkdirTemp("/tmp", "tcl909-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(shortData) })
	t.Setenv("XDG_DATA_HOME", shortData)
	db.ResetForTest()
	cwd := filepath.Join(shortHome, "work")
	require.NoError(t, os.MkdirAll(cwd, 0o700))

	agentID := "agt_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	allocation, err := allocatePrivateOpenCodeState(agentID)
	require.NoError(t, err)
	snapshot := sandboxpolicy.EmptySnapshot()
	snapshot.Effective.NetworkAccess = sandboxpolicy.NetworkAccessNone

	spec, err := openCodeUnixRelayLaunchSpec(
		string(sandboxpolicy.ImplementationTclaudeLayer),
		cwd, nil, &snapshot, agentID)
	require.NoError(t, err)
	controlPath := filepath.Join(allocation.StateRoot, "control.sock")
	listener, device, inode, err := opencodeapi.CreateUnixListener(controlPath)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = listener.Close()
		_ = os.Remove(controlPath)
	})
	encoded, err := json.Marshal(spec)
	require.NoError(t, err)
	replayable := db.OpenCodeRuntime{
		Cwd: cwd, SandboxImplementation: string(sandboxpolicy.ImplementationTclaudeLayer),
		SandboxLaunchSpecJSON: string(encoded),
		Transport:             db.OpenCodeTransportUnixRelay,
		ControlSocketPath:     controlPath, ControlSocketDevice: device,
		ControlSocketInode: inode,
	}

	// The accepting control. Without it a refusal below could be any of the
	// dozen other reasons this function rejects a spec.
	_, err = openCodeRuntimeSandboxSpec(replayable)
	require.NoError(t, err, "the spec replays before the environment moves")

	// The operator moves their XDG data base. The runtime row, the spec and the
	// socket on disk are all untouched.
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "moved"))

	_, err = openCodeRuntimeSandboxSpec(replayable)
	require.Error(t, err)
	require.ErrorContains(t, err, "control authority could not be established",
		"the caller must say the authority was uncomputable, not report a comparison it never made")
	require.ErrorContains(t, err, "is outside this daemon's private state parent",
		"the inner cause has to survive the wrap — that is the whole finding")
	require.ErrorContains(t, err, openCodeStrandedAllocationRemedy)
	require.ErrorContains(t, err, "recreate this agent")
	// The retired sentence, kept as a negative needle: it claimed the path was
	// outside an authority that had never been computed.
	require.NotContains(t, err.Error(),
		"control path is outside its allocated agent authority")
}
