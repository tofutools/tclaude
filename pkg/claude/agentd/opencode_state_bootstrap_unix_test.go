//go:build linux || darwin

package agentd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// The read-only config bootstrap is platform-parameterized precisely so both
// platforms' behavior is observable from either host: a darwin-only test file
// would leave the Linux path (TCL-892) unexercised in CI, which is how the gap
// survived in the first place.
func TestPrepareOpenCodeReadOnlyConfigBootstrapsWithoutOverwrite(t *testing.T) {
	for _, platform := range []string{"Linux", "Darwin"} {
		t.Run(platform, func(t *testing.T) {
			root, configDir := allocatedOpenCodeConfigDir(t)
			spec := openCodeConfigBootstrapSpec(root, configDir, configDir)

			require.NoError(t, prepareOpenCodeReadOnlyConfig(spec, platform))
			path := filepath.Join(configDir, openCodeInstallBootstrapFile)
			raw, err := os.ReadFile(path)
			require.NoError(t, err)
			// Content, not merely existence: a payload OpenCode would rewrite
			// leaves a dirty diff in the operator's own dotfiles.
			assert.Equal(t, openCodeInstallGitignore, string(raw))

			require.NoError(t, os.WriteFile(path, []byte("operator-owned"), 0o640))
			require.NoError(t, prepareOpenCodeReadOnlyConfig(spec, platform))
			raw, err = os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, "operator-owned", string(raw),
				"the pre-wall bootstrap must never overwrite existing host config metadata")
		})
	}
}

// The Linux private-state layout binds the AMBIENT config directory onto the
// per-agent one whenever an ambient ~/.config/opencode exists, so the file has
// to land in the bind's source — the per-agent target is only what the sandbox
// sees the source through.
func TestPrepareOpenCodeReadOnlyConfigSeedsProjectionSource(t *testing.T) {
	root := t.TempDir()
	configBase := filepath.Join(t.TempDir(), "config")
	ambientConfig := filepath.Join(configBase, "opencode")
	require.NoError(t, os.MkdirAll(ambientConfig, 0o700))
	t.Setenv("XDG_CONFIG_HOME", configBase)
	privateConfig := filepath.Join(root, "config", "opencode")
	require.NoError(t, os.MkdirAll(privateConfig, 0o700))

	spec := openCodeConfigBootstrapSpec(root, privateConfig, ambientConfig)
	require.NoError(t, prepareOpenCodeReadOnlyConfig(spec, "Linux"))

	raw, err := os.ReadFile(filepath.Join(ambientConfig, openCodeInstallBootstrapFile))
	require.NoError(t, err)
	assert.Equal(t, openCodeInstallGitignore, string(raw))
	assert.NoFileExists(t, filepath.Join(privateConfig, openCodeInstallBootstrapFile),
		"seeding the read-only target itself would not be visible inside the sandbox")
}

func TestPrepareOpenCodeReadOnlyConfigRefusesUnsafeBootstrap(t *testing.T) {
	for _, platform := range []string{"Linux", "Darwin"} {
		t.Run(platform, func(t *testing.T) {
			root, configDir := allocatedOpenCodeConfigDir(t)
			require.NoError(t, os.WriteFile(
				filepath.Join(configDir, "target"), []byte("x"), 0o600))
			require.NoError(t, os.Symlink("target",
				filepath.Join(configDir, openCodeInstallBootstrapFile)))

			err := prepareOpenCodeReadOnlyConfig(
				openCodeConfigBootstrapSpec(root, configDir, configDir), platform)
			require.ErrorContains(t, err, "opencode_read_only_config_bootstrap")
			require.ErrorContains(t, err, platform)
			require.ErrorContains(t, err, "existing OpenCode config bootstrap")
		})
	}
}

// A config app directory served by no read-only bind is writable in the
// sandbox, so nothing needs to be planted in the operator's tree for it.
func TestPrepareOpenCodeReadOnlyConfigSkipsWritableConfig(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(t.TempDir(), "opencode")
	require.NoError(t, os.Mkdir(configDir, 0o700))
	spec := openCodeConfigBootstrapSpec(root, configDir, configDir)
	spec.Contract.ReadOnlyBinds = nil

	require.NoError(t, prepareOpenCodeReadOnlyConfig(spec, "Linux"))
	assert.NoFileExists(t, filepath.Join(configDir, openCodeInstallBootstrapFile))
}

// Falsifiability anchor for TCL-892: the host's own hook must reach the shared
// implementation. With the Linux hook back to its pre-TCL-892 `return nil`,
// this fails on Linux.
func TestPrepareOpenCodeReadOnlyConfigForPlatformIsWired(t *testing.T) {
	root, configDir := allocatedOpenCodeConfigDir(t)

	require.NoError(t, prepareOpenCodeReadOnlyConfigForPlatform(
		openCodeConfigBootstrapSpec(root, configDir, configDir)))

	raw, err := os.ReadFile(filepath.Join(configDir, openCodeInstallBootstrapFile))
	require.NoError(t, err)
	assert.Equal(t, openCodeInstallGitignore, string(raw))
}

func openCodeConfigBootstrapSpec(
	root, configDir, bindSource string,
) *session.TclaudeLayerLaunchSpec {
	return &session.TclaudeLayerLaunchSpec{
		Contract: session.TclaudeLayerLaunchContract{
			HarnessName: harness.OpenCodeName,
			StateRoot:   root,
			StateDirs: []string{
				filepath.Join(root, "data", "opencode"),
				filepath.Join(root, "cache", "opencode"),
				configDir,
				filepath.Join(root, "state", "opencode"),
			},
			ReadOnlyBinds: []session.TclaudeLayerReadOnlyBind{{
				Source: bindSource,
				Target: configDir,
			}},
		},
	}
}

// resolvedTestPath is the file's idiom for "the spelling the production code
// will use". This area compares paths for IDENTITY, and a path has more than
// one true spelling: a host reaching its temp root through a symlink (macOS
// /var -> /private/var) hands a test one spelling while the code under test
// renders another. Build expectations through here rather than from a raw
// t.TempDir() path.
func resolvedTestPath(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	require.NoError(t, err)
	return resolved
}

// isolatedOpenCodeHost points every path the OpenCode state code derives at a
// disposable home with its own database, and returns that home.
func isolatedOpenCodeHost(t *testing.T) string {
	t.Helper()
	home := resolvedTestPath(t, t.TempDir())
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	db.ResetForTest()
	t.Cleanup(db.ResetForTest)
	return home
}

// allocatedOpenCodeConfigDir gives the test an isolated host whose OpenCode
// state allocation this daemon actually owns, and returns that agent's state
// root and its config app directory. A self-bound contract naming any other
// directory is refused (TCL-902), so the legitimate self-bind cases have to
// stand on a real allocation rather than on the contract's own word.
//
// The allocation comes from allocatePrivateOpenCodeState, not from a row this
// helper places where it thinks allocations go. The seed predicate now depends
// on the private state parent the daemon derives, so a test that restated that
// formula would agree with itself while the allocator drifted away from both.
func allocatedOpenCodeConfigDir(t *testing.T) (stateRoot, configDir string) {
	t.Helper()
	isolatedOpenCodeHost(t)

	allocation, err := allocatePrivateOpenCodeState(db.NewAgentID())
	require.NoError(t, err)
	require.Equal(t, db.OpenCodeStatePrivate, allocation.Mode)
	configDir = filepath.Join(allocation.StateRoot, "config", "opencode")
	require.NoError(t, os.MkdirAll(configDir, 0o700))
	return allocation.StateRoot, configDir
}

// TCL-902, grandfathered branch. validateOpenCodeV3LaunchContract's
// grandfathered branch validates no state directories at all, so a persisted
// spec tampered to name a victim directory as StateDirs[2] — plus the self-bind
// that makes it its own read-only source — clears the launch contract. Before
// this fix the seed's source check accepted it too, on nothing but
// `source == configDir`, and created a daemon-side .gitignore in the victim
// directory. The contract's acceptance is asserted, not assumed: it is what
// makes the seed the only thing standing between the tampered spec and the
// write.
func TestPrepareOpenCodeReadOnlyConfigRefusesGrandfatheredContractTarget(t *testing.T) {
	// Two victim shapes, because the refusal has to hold for both a plain
	// operator directory and one dressed up in the per-agent layout's own
	// <root>/config/opencode shape.
	for _, victimCase := range []struct {
		name, suffix  string
		want          func(victim string) string
		resolveVictim bool
	}{
		{
			name:   "PlainDirectory",
			suffix: "victim",
			want: func(string) string {
				return "does not have the per-agent <state root>/config/opencode shape"
			},
		},
		{
			name:   "LayoutShapedDirectory",
			suffix: filepath.Join("victim", "config", "opencode"),
			// The operator's own directory name must not be quoted back at them
			// as an "invalid agent id"; it is named as what it is.
			want: func(victim string) string {
				return fmt.Sprintf("names %q where a per-agent state root was expected",
					filepath.Dir(filepath.Dir(victim)))
			},
			resolveVictim: true,
		},
	} {
		t.Run(victimCase.name, func(t *testing.T) {
			_, _ = allocatedOpenCodeConfigDir(t)
			victim := filepath.Join(t.TempDir(), victimCase.suffix)
			require.NoError(t, os.MkdirAll(victim, 0o700))
			legacyRoot := filepath.Join(t.TempDir(), ".opencode")

			contract := session.TclaudeLayerLaunchContract{
				HarnessName: harness.OpenCodeName,
				StateRoot:   legacyRoot,
				StateDirs: []string{
					filepath.Join(legacyRoot, "data", "opencode"),
					filepath.Join(legacyRoot, "cache", "opencode"),
					victim,
					filepath.Join(legacyRoot, "state", "opencode"),
				},
				FinalHideDirs: []string{filepath.Dir(legacyRoot)},
				ReadOnlyBinds: []session.TclaudeLayerReadOnlyBind{{
					Source: victim, Target: victim,
				}},
			}
			require.NoError(t, validateOpenCodeV3LaunchContract(contract, false),
				"the grandfathered branch accepts this contract; the seed is the only remaining gate")

			err := prepareOpenCodeReadOnlyConfig(
				&session.TclaudeLayerLaunchSpec{Contract: contract}, "Linux")
			require.ErrorContains(t, err, "opencode_read_only_config_bootstrap")
			// The refusal quotes the path in the spelling the production code
			// resolved it to, which is not the one t.TempDir() handed us on a
			// host whose temp root is a symlink.
			expected := victim
			if victimCase.resolveVictim {
				expected = resolvedTestPath(t, victim)
			}
			require.ErrorContains(t, err, victimCase.want(expected))
			assert.NoFileExists(t, filepath.Join(victim, openCodeInstallBootstrapFile))
		})
	}
}

// TCL-902, private branch. Fixing only the grandfathered branch would leave the
// class open: the private branch ties StateDirs[2] to Environment[2].Value,
// which the same persisted artifact supplies. This contract passes the private
// branch in full — four XDG roots, an agent-id-shaped state root, the private
// write pair, three hidden ambient roots — while naming a victim directory, so
// the seed target must be checked against the allocation store rather than
// against the contract.
func TestPrepareOpenCodeReadOnlyConfigRefusesUnallocatedPrivateTarget(t *testing.T) {
	_, _ = allocatedOpenCodeConfigDir(t)
	// The victim carries an agent-id-shaped root of its own, so the refusal
	// comes from the allocation store rather than from the path shape.
	const unallocatedID = "agt_00000000000000000000000000000042"
	victimBase := filepath.Join(t.TempDir(), unallocatedID, "config")
	victim := filepath.Join(victimBase, "opencode")
	require.NoError(t, os.MkdirAll(victim, 0o700))
	forgedRoot := filepath.Join(t.TempDir(), "agt_0123456789abcdef")

	contract := session.TclaudeLayerLaunchContract{
		HarnessName: harness.OpenCodeName,
		StateRoot:   forgedRoot,
		Environment: []sandboxpolicy.EnvironmentEntry{
			{Name: "XDG_DATA_HOME", Value: filepath.Join(forgedRoot, "data")},
			{Name: "XDG_CACHE_HOME", Value: filepath.Join(forgedRoot, "cache")},
			{Name: "XDG_CONFIG_HOME", Value: victimBase},
			{Name: "XDG_STATE_HOME", Value: filepath.Join(forgedRoot, "state")},
		},
		StateDirs: []string{
			filepath.Join(forgedRoot, "data", "opencode"),
			filepath.Join(forgedRoot, "cache", "opencode"),
			victim,
			filepath.Join(forgedRoot, "state", "opencode"),
		},
		PrivateWriteDirs: []session.TclaudeLayerPrivateWriteDir{{
			Parent: filepath.Dir(forgedRoot), Current: forgedRoot,
		}},
		FinalHideDirs: []string{"/a", "/b", "/c"},
		ReadOnlyBinds: []session.TclaudeLayerReadOnlyBind{{
			Source: victim, Target: victim,
		}},
	}
	require.NoError(t, validateOpenCodeV3LaunchContract(contract, false),
		"the private branch accepts this contract; the seed is the only remaining gate")

	err := prepareOpenCodeReadOnlyConfig(
		&session.TclaudeLayerLaunchSpec{Contract: contract}, "Linux")
	require.ErrorContains(t, err, "opencode_read_only_config_bootstrap")
	require.ErrorContains(t, err,
		"is not an allocated per-agent config directory")
	require.ErrorContains(t, err, "has no durable state allocation")
	assert.NoFileExists(t, filepath.Join(victim, openCodeInstallBootstrapFile))
}

// A state root whose agent id IS allocated, but whose config directory belongs
// to a different root, must not pass on the agent id alone.
func TestPrepareOpenCodeReadOnlyConfigRefusesForeignRootOfAllocatedAgent(t *testing.T) {
	stateRoot, _ := allocatedOpenCodeConfigDir(t)
	agentID := filepath.Base(stateRoot)
	impostorRoot := filepath.Join(t.TempDir(), agentID)
	impostorConfig := filepath.Join(impostorRoot, "config", "opencode")
	require.NoError(t, os.MkdirAll(impostorConfig, 0o700))

	err := prepareOpenCodeReadOnlyConfig(
		openCodeConfigBootstrapSpec(impostorRoot, impostorConfig, impostorConfig),
		"Linux")
	require.ErrorContains(t, err, "opencode_read_only_config_bootstrap")
	require.ErrorContains(t, err,
		"does not belong to the private state allocation of agent "+agentID)
	assert.NoFileExists(t,
		filepath.Join(impostorConfig, openCodeInstallBootstrapFile))
}

// A legacy-shared allocation records no state root, so its agent id must not
// carry a per-agent config directory that only looks the part.
func TestPrepareOpenCodeReadOnlyConfigRefusesLegacySharedAllocation(t *testing.T) {
	home := isolatedOpenCodeHost(t)

	agentID := db.NewAgentID()
	inserted, err := db.InsertOpenCodeAgentStateAllocation(
		db.OpenCodeAgentStateAllocation{
			AgentID: agentID, Mode: db.OpenCodeStateLegacyShared,
		})
	require.NoError(t, err)
	require.True(t, inserted)

	stateRoot := filepath.Join(
		home, "data", "tclaude", "opencode-agents", agentID)
	configDir := filepath.Join(stateRoot, "config", "opencode")
	require.NoError(t, os.MkdirAll(configDir, 0o700))

	err = prepareOpenCodeReadOnlyConfig(
		openCodeConfigBootstrapSpec(stateRoot, configDir, configDir), "Linux")
	require.ErrorContains(t, err,
		"does not belong to the legacy-shared state allocation of agent "+agentID)
	assert.NoFileExists(t, filepath.Join(configDir, openCodeInstallBootstrapFile))
}

// An allocation this daemon owns, but at a state root outside the private state
// parent THIS daemon derives, is not proof of anything: the allocation store is
// the same durable database as the launch spec.
func TestPrepareOpenCodeReadOnlyConfigRefusesAllocationOutsidePrivateParent(t *testing.T) {
	home := isolatedOpenCodeHost(t)

	agentID := db.NewAgentID()
	stateRoot := filepath.Join(home, "elsewhere", agentID)
	configDir := filepath.Join(stateRoot, "config", "opencode")
	require.NoError(t, os.MkdirAll(configDir, 0o700))
	inserted, err := db.InsertOpenCodeAgentStateAllocation(
		db.OpenCodeAgentStateAllocation{
			AgentID: agentID, Mode: db.OpenCodeStatePrivate, StateRoot: stateRoot,
		})
	require.NoError(t, err)
	require.True(t, inserted)

	err = prepareOpenCodeReadOnlyConfig(
		openCodeConfigBootstrapSpec(stateRoot, configDir, configDir), "Linux")
	require.ErrorContains(t, err,
		"is outside this daemon's private state parent")
	assert.NoFileExists(t, filepath.Join(configDir, openCodeInstallBootstrapFile))
}

// Accepted, documented behavior rather than an oversight: a private allocation
// is bound to the private state parent it was created under, so changing
// XDG_DATA_HOME (or HOME, when XDG_DATA_HOME is unset) strands it and the seed
// refuses. openCodeControlSocketPath already fails the same way on the same
// allocation, which is why isolated and filtered postures were already broken
// by this operator action before the seed was anchored; this makes the
// host-open posture behave the same. The refusal has to name the environment
// change, because that is the only thing that tells an operator what they did.
func TestPrepareOpenCodeReadOnlyConfigRefusesAllocationStrandedByEnvChange(t *testing.T) {
	stateRoot, configDir := allocatedOpenCodeConfigDir(t)
	spec := openCodeConfigBootstrapSpec(stateRoot, configDir, configDir)
	require.NoError(t, prepareOpenCodeReadOnlyConfig(spec, "Linux"),
		"the allocation is seedable before the environment moves")
	require.NoError(t, os.Remove(
		filepath.Join(configDir, openCodeInstallBootstrapFile)))

	// The operator moves their XDG data base. The allocation row, the state
	// root on disk and the launch contract are all unchanged.
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "moved"))

	err := prepareOpenCodeReadOnlyConfig(spec, "Linux")
	require.ErrorContains(t, err,
		"is outside this daemon's private state parent")
	require.ErrorContains(t, err,
		"a changed XDG_DATA_HOME or HOME moves that parent away from an existing allocation")
	// The remedy on THIS path too, not only on the control socket below. A cold
	// review mutation removed it from here and no test failed — the docs assert
	// host-open gets a way out, and nothing was holding that claim up.
	require.ErrorContains(t, err, openCodeStrandedAllocationRemedy)
	require.ErrorContains(t, err, "recreate this agent")
	assert.NoFileExists(t, filepath.Join(configDir, openCodeInstallBootstrapFile))

	// The same allocation fails the same way on the control-socket path, which
	// is the pre-existing rule this one is now consistent with — asserted, so
	// "consistency gain" is not a claim the PR makes about untested code.
	_, controlErr := openCodeControlSocketPath(filepath.Base(stateRoot))
	require.ErrorContains(t, controlErr,
		"is outside this daemon's private state parent",
		"pinned to the reason: an unrelated failure here would keep this test green while the parity claim in the migration note quietly became false")
	require.ErrorContains(t, controlErr, openCodeStrandedAllocationRemedy,
		"this is the posture pair operators actually run, so it is the one that most needs the way out")
	// The retired spelling, kept as a NEGATIVE needle. It covered four distinct
	// causes and named none of them; if it comes back, this fails rather than
	// the wording silently regressing to the version TCL-909 removed.
	require.NotContains(t, controlErr.Error(), "validated direct agent child")
}

// The self-bind case, driven through the PRODUCTION layout builder rather than
// a hand-authored contract. TestPrepareOpenCodeReadOnlyConfigMatchesTheProduced-
// Layout covers the ambient projection; this covers the other half — a host with
// no ambient ~/.config/opencode, where the layout self-binds the per-agent
// config directory and the seed has to accept it on allocation authority. If
// the layout ever placed state roots somewhere the predicate does not accept,
// only a test on this path would notice.
func TestPrepareOpenCodeReadOnlyConfigAcceptsProducedSelfBoundLayout(t *testing.T) {
	stateRoot, configDir := allocatedOpenCodeConfigDir(t)
	require.NoDirExists(t, filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "opencode"),
		"this case is about a host with no ambient OpenCode config")

	layout, err := openCodeStateLayoutForAllocation(db.OpenCodeAgentStateAllocation{
		AgentID:   filepath.Base(stateRoot),
		Mode:      db.OpenCodeStatePrivate,
		StateRoot: stateRoot,
	})
	require.NoError(t, err)
	require.Len(t, layout.stateDirs, 4)
	require.Equal(t, configDir, layout.stateDirs[2])
	require.Equal(t, configDir,
		openCodeReadOnlyConfigBindSource(session.TclaudeLayerLaunchContract{
			StateDirs: layout.stateDirs, ReadOnlyBinds: layout.readOnlyBinds,
		}), "with no ambient config the layout serves the per-agent directory to itself")

	spec := &session.TclaudeLayerLaunchSpec{
		Contract: session.TclaudeLayerLaunchContract{
			HarnessName:   harness.OpenCodeName,
			StateRoot:     stateRoot,
			StateDirs:     layout.stateDirs,
			ReadOnlyBinds: layout.readOnlyBinds,
		},
	}
	require.NoError(t, prepareOpenCodeReadOnlyConfigForPlatform(spec))
	raw, err := os.ReadFile(filepath.Join(configDir, openCodeInstallBootstrapFile))
	require.NoError(t, err)
	assert.Equal(t, openCodeInstallGitignore, string(raw))
}

// A bind source is not covered by the launch contract's own validation, which
// checks bind targets, so a replayed or tampered spec must not be able to aim a
// daemon-side write at an arbitrary directory.
func TestPrepareOpenCodeReadOnlyConfigRefusesForeignBindSource(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(t.TempDir(), "opencode")
	require.NoError(t, os.Mkdir(configDir, 0o700))
	foreign := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "config"))

	err := prepareOpenCodeReadOnlyConfig(
		openCodeConfigBootstrapSpec(root, configDir, foreign), "Linux")
	require.ErrorContains(t, err, "opencode_read_only_config_bootstrap")
	require.ErrorContains(t, err,
		"is neither an allocated per-agent config directory nor this host's ambient OpenCode config")
	assert.NoFileExists(t, filepath.Join(foreign, openCodeInstallBootstrapFile))
}

// When more than one read-only bind names the config directory, the one the
// sandbox serves is the LAST. Seeding an earlier source would write a file
// nothing inside the sandbox can see.
func TestPrepareOpenCodeReadOnlyConfigSeedsTheServingBind(t *testing.T) {
	root, privateConfig := allocatedOpenCodeConfigDir(t)
	ambientConfig := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "opencode")
	require.NoError(t, os.MkdirAll(ambientConfig, 0o700))

	spec := openCodeConfigBootstrapSpec(root, privateConfig, ambientConfig)
	spec.Contract.ReadOnlyBinds = append(spec.Contract.ReadOnlyBinds,
		session.TclaudeLayerReadOnlyBind{
			Source: privateConfig, Target: privateConfig,
		})

	require.NoError(t, prepareOpenCodeReadOnlyConfig(spec, "Linux"))
	assert.FileExists(t, filepath.Join(privateConfig, openCodeInstallBootstrapFile))
	assert.NoFileExists(t,
		filepath.Join(ambientConfig, openCodeInstallBootstrapFile),
		"the losing bind's source is not what the sandbox reads")
}

// The unit cases above author their own contract, so nothing in them would
// notice if the production layout stopped emitting the bind shape the predicate
// looks for. This one builds the layout through the production path and feeds
// its own output to the predicate.
func TestPrepareOpenCodeReadOnlyConfigMatchesTheProducedLayout(t *testing.T) {
	home := t.TempDir()
	configBase := filepath.Join(home, "config")
	ambientConfig := filepath.Join(configBase, "opencode")
	require.NoError(t, os.MkdirAll(ambientConfig, 0o700))
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("XDG_CONFIG_HOME", configBase)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))

	const agentID = "agt_0123456789abcdef0123456789abcdef"
	stateRoot := filepath.Join(home, "opencode-state", agentID)
	require.NoError(t, os.MkdirAll(stateRoot, 0o700))
	resolvedRoot := resolvedTestPath(t, stateRoot)

	layout, err := openCodeStateLayoutForAllocation(db.OpenCodeAgentStateAllocation{
		AgentID:   agentID,
		Mode:      db.OpenCodeStatePrivate,
		StateRoot: resolvedRoot,
	})
	require.NoError(t, err)
	require.Len(t, layout.stateDirs, 4)

	spec := &session.TclaudeLayerLaunchSpec{
		Contract: session.TclaudeLayerLaunchContract{
			HarnessName:   harness.OpenCodeName,
			StateRoot:     resolvedRoot,
			StateDirs:     layout.stateDirs,
			ReadOnlyBinds: layout.readOnlyBinds,
		},
	}
	require.NoError(t, prepareOpenCodeReadOnlyConfigForPlatform(spec))

	resolvedAmbient := resolvedTestPath(t, ambientConfig)
	raw, err := os.ReadFile(
		filepath.Join(resolvedAmbient, openCodeInstallBootstrapFile))
	require.NoError(t, err,
		"the layout's config bind serves the ambient directory, so that is where the bootstrap has to land")
	assert.Equal(t, openCodeInstallGitignore, string(raw))
}

// The bind source and the host's ambient config can name the same directory
// through different paths — macOS reaches its temp root through /var, a
// symlink to /private/var, which is how this first failed in CI. The source
// check compares directories, not strings.
func TestPrepareOpenCodeReadOnlyConfigAcceptsSymlinkedAmbientConfig(t *testing.T) {
	root := t.TempDir()
	real := t.TempDir()
	configBase := filepath.Join(real, "config")
	ambientConfig := filepath.Join(configBase, "opencode")
	require.NoError(t, os.MkdirAll(ambientConfig, 0o700))

	link := filepath.Join(t.TempDir(), "link")
	require.NoError(t, os.Symlink(real, link))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(link, "config"))

	privateConfig := filepath.Join(root, "config", "opencode")
	require.NoError(t, os.MkdirAll(privateConfig, 0o700))
	spec := openCodeConfigBootstrapSpec(root, privateConfig, ambientConfig)

	require.NoError(t, prepareOpenCodeReadOnlyConfig(spec, "Darwin"))
	assert.FileExists(t, filepath.Join(ambientConfig, openCodeInstallBootstrapFile))
}

// The remedy must be UNREACHABLE from a posture that is not stranded. Telling
// an operator whose agent is fine to recreate it is worse than saying nothing,
// and it is the failure mode a shared remedy constant invites.
//
// Legacy-shared is that posture, and the reason is structural rather than
// incidental: openCodeControlSocketPath builds its control root as
// filepath.Join(parent, agentID) from the CURRENT parent, so the root follows
// an environment change instead of being stranded by it. Private allocations
// carry a RECORDED root, which is what a moved parent leaves behind.
//
// Asserted through the production path both before and after the move, so this
// is a property of the code rather than a note about it — a later change that
// made legacy-shared consult a recorded root would fail here rather than start
// telling unaffected operators to recreate their agents.
func TestOpenCodeControlSocketPathLeavesLegacySharedUnstrandedAndUnadvised(t *testing.T) {
	isolatedOpenCodeHost(t)
	agentID := db.NewAgentID()
	inserted, err := db.InsertOpenCodeAgentStateAllocation(
		db.OpenCodeAgentStateAllocation{
			AgentID: agentID, Mode: db.OpenCodeStateLegacyShared,
		})
	require.NoError(t, err)
	require.True(t, inserted, "the fixture only means anything on a real allocation")

	before, err := openCodeControlSocketPath(agentID)
	require.NoError(t, err, "control before the move — the accepting control")
	require.NotEmpty(t, before)

	// The same operator action that strands a private allocation.
	moved := filepath.Join(t.TempDir(), "moved")
	// The FIXTURE owns this directory's existence, not production. Without it
	// the assertion below still relies on production's own MkdirAll having run,
	// so a regression that moved the derived parent out from under
	// XDG_DATA_HOME would die inside resolvedTestPath with "lstat: no such
	// file" instead of saying the control root is not under the new parent.
	// Still red either way — but pointed at the wrong thing.
	require.NoError(t, os.MkdirAll(moved, 0o700))
	t.Setenv("XDG_DATA_HOME", moved)

	after, err := openCodeControlSocketPath(agentID)
	require.NoError(t, err,
		"legacy-shared derives its control root from the current parent, so it follows the move")
	assert.NotEqual(t, before, after,
		"and it really did follow it — equal paths would mean the move never took effect and this test proved nothing")
	// resolvedTestPath, not the raw `moved`. openCodeControlSocketPath
	// EvalSymlinks's the parent before building the control root, so it returns
	// the RESOLVED spelling — and on macOS the temp root is reached through
	// /var -> /private/var, so the two differ and a raw comparison fails there
	// while passing on Linux. This is the trap resolvedTestPath's own comment
	// in this file warns about, and this assertion walked straight into it: it
	// went red on macOS CI only.
	//
	// Resolved AFTER the call, when production has created the directory —
	// EvalSymlinks needs it to exist.
	assert.True(t, strings.HasPrefix(after, resolvedTestPath(t, moved)),
		"the new control root must sit under the new parent")
}

// The private counterpart, kept next to it: the same move on a private
// allocation DOES strand, and that refusal is the one carrying the remedy. Both
// halves in one place, because the property under test is the DIFFERENCE
// between them.
func TestOpenCodeControlSocketPathAdvisesOnlyTheStrandedPrivateAllocation(t *testing.T) {
	stateRoot, _ := allocatedOpenCodeConfigDir(t)
	agentID := filepath.Base(stateRoot)

	_, err := openCodeControlSocketPath(agentID)
	require.NoError(t, err, "control before the move")

	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "moved"))

	_, err = openCodeControlSocketPath(agentID)
	require.Error(t, err)
	require.ErrorContains(t, err, "is outside this daemon's private state parent")
	require.ErrorContains(t, err, openCodeStrandedAllocationRemedy)
	require.ErrorContains(t, err, "recreate this agent",
		"the remedy has to survive as WORDS an operator can act on, not just as a constant reference")
}
