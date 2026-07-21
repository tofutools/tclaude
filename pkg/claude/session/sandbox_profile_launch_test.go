package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func TestCodexManagedProfileCarriesSnapshotNetworkPolicy(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	effective, err := sandboxpolicy.Resolve(sandboxpolicy.Scopes{Global: &sandboxpolicy.Profile{
		Name: "internet", NetworkAccess: sandboxpolicy.NetworkAccessInternet,
	}})
	require.NoError(t, err)
	snapshot := sandboxpolicy.NewSnapshot(effective, nil)
	params := &NewParams{
		PermissionProfile:          harness.CodexAgentProfile,
		GitWorktreeWriteDirsPinned: true,
	}
	_, path, _, err := ensureCodexManagedProfileWithSnapshot(params, t.TempDir(), "1234567890abcdef", &snapshot)
	require.NoError(t, err)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	content := string(raw)
	assert.Contains(t, content, "[features]")
	assert.Contains(t, content, "network_proxy = false")
	assert.NotContains(t, content, "[features.network_proxy]")
	assert.Contains(t, content, "enabled = true")
	assert.Contains(t, content, ".network.unix_sockets]")
}

func TestSandboxSnapshotEnvironmentCarriesMaterializedAgentDirectory(t *testing.T) {
	snapshot := &sandboxpolicy.Snapshot{Effective: sandboxpolicy.EffectiveProfile{Environment: []sandboxpolicy.EnvironmentEntry{
		{Name: "GOBIN", Value: "/private/GOBIN"},
		{Name: "PROFILE_VERSION", Value: "v2"},
	}}}

	assert.Equal(t, map[string]string{
		"GOBIN":           "/private/GOBIN",
		"PROFILE_VERSION": "v2",
	}, sandboxSnapshotEnvironment(snapshot))
	assert.Nil(t, sandboxSnapshotEnvironment(nil))
}

func TestSandboxSnapshotDirsOmitsMissingRuleUntilLaterLaunch(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	missing := filepath.Join(root, "future", "cache")
	snapshot := &sandboxpolicy.Snapshot{
		Version: sandboxpolicy.SnapshotVersion,
		Effective: sandboxpolicy.EffectiveProfile{Filesystem: []sandboxpolicy.FilesystemGrant{{
			Path: missing, Access: sandboxpolicy.AccessWrite,
		}}},
	}

	launch, err := sandboxSnapshotForLaunch(snapshot)
	require.NoError(t, err)
	assert.Empty(t, sandboxSnapshotDirs(launch, sandboxpolicy.AccessWrite),
		"a missing rule must not reach the harness on this launch")
	require.NoError(t, os.MkdirAll(missing, 0o755))
	launch, err = sandboxSnapshotForLaunch(snapshot)
	require.NoError(t, err)
	assert.Equal(t, []string{missing}, sandboxSnapshotDirs(launch, sandboxpolicy.AccessWrite),
		"the same frozen rule becomes active on a later launch")
}

func TestSandboxSnapshotProofDirsExcludesMaterializedAgentDirectory(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	cwd := filepath.Join(root, "cwd")
	customWriteDir := filepath.Join(root, "custom")
	agentWriteDir := filepath.Join(root, "agent-dirs", "spwn-test", "GOCACHE")
	for _, dir := range []string{cwd, customWriteDir, agentWriteDir} {
		require.NoError(t, os.MkdirAll(dir, 0o700))
	}
	snapshot := &sandboxpolicy.Snapshot{
		Version: sandboxpolicy.SnapshotVersion,
		Effective: sandboxpolicy.EffectiveProfile{
			AgentDirectories: []string{"GOCACHE"},
			Environment:      []sandboxpolicy.EnvironmentEntry{{Name: "GOCACHE", Value: agentWriteDir}},
			Filesystem: []sandboxpolicy.FilesystemGrant{
				{Path: customWriteDir, Access: sandboxpolicy.AccessWrite},
				{Path: agentWriteDir, Access: sandboxpolicy.AccessWrite},
			},
		},
	}

	assert.Equal(t, []string{customWriteDir, agentWriteDir},
		sandboxSnapshotDirs(snapshot, sandboxpolicy.AccessWrite),
		"the generated directory must remain writable by the child")
	proofDirs, generatedDirs := sandboxSnapshotProofDirs(snapshot, sandboxpolicy.AccessWrite)
	assert.Equal(t, []string{customWriteDir}, proofDirs,
		"only caller-controlled roots should require the caller's marker")
	assert.Equal(t, []string{agentWriteDir}, generatedDirs,
		"the generated root should retain a path-substitution check")

	proof := "proof-agent-directory"
	marker := clcommon.SpawnDirWriteProofPrefix + proof
	for _, dir := range []string{cwd, customWriteDir} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, marker), nil, 0o600))
	}
	ready := filepath.Join(root, "ready")
	require.NoError(t, os.WriteFile(ready, []byte("pending"), 0o600))

	cmd := exec.Command("sh", "-c", guardHarnessCommandWithDirProof(
		"true", proof, ready, true, proofDirs, generatedDirs))
	cmd.Dir = cwd
	require.NoError(t, cmd.Run(),
		"a daemon-materialized directory created after the challenge must not need a caller marker")
	status, err := os.ReadFile(ready)
	require.NoError(t, err)
	assert.Equal(t, "ok", string(status))

	// The generated directory needs no caller marker, but it must not be
	// replaceable with a symlink between daemon materialization and launch.
	forbidden := filepath.Join(root, "forbidden")
	require.NoError(t, os.Mkdir(forbidden, 0o700))
	require.NoError(t, os.Rename(agentWriteDir, agentWriteDir+"-old"))
	require.NoError(t, os.Symlink(forbidden, agentWriteDir))
	require.NoError(t, os.WriteFile(ready, []byte("pending"), 0o600))
	cmd = exec.Command("sh", "-c", guardHarnessCommandWithDirProof(
		"true", proof, ready, true, proofDirs, generatedDirs))
	cmd.Dir = cwd
	require.Error(t, cmd.Run())
	status, err = os.ReadFile(ready)
	require.NoError(t, err)
	assert.Equal(t, "error:repository-proof", string(status))
}

// With features.agent_dirs_mount_parent, the write grant is the shared parent
// root rather than each per-name subdir. The classifier must still recognize
// that root as daemon-generated (parent of the agent-dir env values) so it
// skips the caller-marker proof — otherwise the launch would demand a marker
// inside a directory the caller never created.
func TestSandboxSnapshotProofDirsExcludesMountedParentRoot(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	customWriteDir := filepath.Join(root, "custom")
	agentRoot := filepath.Join(root, "agent-dirs", "spwn-test")
	goCache := filepath.Join(agentRoot, "GOCACHE")
	goTmp := filepath.Join(agentRoot, "GOTMPDIR")
	snapshot := &sandboxpolicy.Snapshot{
		Version: sandboxpolicy.SnapshotVersion,
		Effective: sandboxpolicy.EffectiveProfile{
			AgentDirectories: []string{"GOCACHE", "GOTMPDIR"},
			Environment: []sandboxpolicy.EnvironmentEntry{
				{Name: "GOCACHE", Value: goCache},
				{Name: "GOTMPDIR", Value: goTmp},
			},
			// Mount-parent mode grants the parent root once; the subdirs
			// themselves get no individual grant.
			Filesystem: []sandboxpolicy.FilesystemGrant{
				{Path: agentRoot, Access: sandboxpolicy.AccessWrite},
				{Path: customWriteDir, Access: sandboxpolicy.AccessWrite},
			},
		},
	}

	proofDirs, generatedDirs := sandboxSnapshotProofDirs(snapshot, sandboxpolicy.AccessWrite)
	assert.Equal(t, []string{customWriteDir}, proofDirs,
		"only the caller-controlled root should require the caller's marker")
	assert.Equal(t, []string{agentRoot}, generatedDirs,
		"the mounted parent root is daemon-generated and must skip the caller marker")
}

// A minimal profile drops `extends = ":workspace"`, which is what used to make
// the launch directory writable for free. GitWorktreeWriteDirs yields nothing
// outside a Git repository, so without an explicit grant a minimal agent in a
// plain directory would have no workspace at all.
func TestMinimalBaselineGrantsWorkspaceOutsideGitRepository(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Deliberately NOT a Git repository.
	workspace := filepath.Join(home, "plain-workspace")
	require.NoError(t, os.MkdirAll(workspace, 0o755))
	canonicalWorkspace, err := filepath.EvalSymlinks(workspace)
	require.NoError(t, err)
	outside := filepath.Join(home, "elsewhere")
	require.NoError(t, os.MkdirAll(outside, 0o755))

	effective, err := sandboxpolicy.Resolve(sandboxpolicy.Scopes{Global: &sandboxpolicy.Profile{
		Name: "strict", ReadBaseline: sandboxpolicy.ReadBaselineMinimal,
	}})
	require.NoError(t, err)
	snapshot := sandboxpolicy.NewSnapshot(effective, nil)

	params := &NewParams{PermissionProfile: harness.CodexAgentProfile, GitWorktreeWriteDirsPinned: true}
	_, path, _, err := ensureCodexManagedProfileWithSnapshot(params, workspace, "1234567890abcdef", &snapshot)
	require.NoError(t, err)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	content := string(raw)

	// The workspace is readable and writable...
	assert.Contains(t, content, `"`+canonicalWorkspace+`" = "write"`,
		"a minimal agent must still be able to work in its launch directory")
	// ...the runtime baseline is present so tools can actually run...
	assert.Contains(t, content, `":minimal" = "read"`)
	// ...the broad read baseline is gone...
	assert.NotContains(t, content, `extends = ":workspace"`)
	// ...and nothing granted the sibling directory.
	assert.NotContains(t, content, outside)
}

// The default baseline must keep relying on :workspace, unchanged.
func TestDefaultBaselineDoesNotAddExplicitWorkspaceGrant(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := filepath.Join(home, "plain-workspace")
	require.NoError(t, os.MkdirAll(workspace, 0o755))

	effective, err := sandboxpolicy.Resolve(sandboxpolicy.Scopes{})
	require.NoError(t, err)
	snapshot := sandboxpolicy.NewSnapshot(effective, nil)
	params := &NewParams{PermissionProfile: harness.CodexAgentProfile, GitWorktreeWriteDirsPinned: true}
	_, path, _, err := ensureCodexManagedProfileWithSnapshot(params, workspace, "1234567890abcdef", &snapshot)
	require.NoError(t, err)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `extends = ":workspace"`)
	assert.NotContains(t, string(raw), `"`+workspace+`" = "write"`,
		"today's behavior is unchanged: :workspace already covers the cwd")
}

func TestCodexManagedProfileRendersSemanticReadExclusion(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	home := t.TempDir()
	canonicalHome, err := filepath.EvalSymlinks(home)
	require.NoError(t, err)
	home = canonicalHome
	t.Setenv("HOME", home)
	ssh := filepath.Join(home, ".ssh")
	require.NoError(t, os.MkdirAll(ssh, 0o700))
	effective, err := sandboxpolicy.Resolve(sandboxpolicy.Scopes{Explicit: &sandboxpolicy.Profile{
		Name: "no-ssh", ReadBaselineExclusions: []string{sandboxpolicy.ReadExclusionSSH},
	}})
	require.NoError(t, err)
	snapshot := sandboxpolicy.NewSnapshot(effective, nil)
	params := &NewParams{PermissionProfile: harness.CodexAgentProfile, GitWorktreeWriteDirsPinned: true}
	_, path, _, err := ensureCodexManagedProfileWithSnapshot(params, t.TempDir(), "1234567890abcdef", &snapshot)
	require.NoError(t, err)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"`+ssh+`" = "none"`)
	assert.Contains(t, string(raw), `extends = ":workspace"`)
}

func TestStrictHomeGitWriteDirsDropsRepositoryContainerAndSiblings(t *testing.T) {
	container := t.TempDir()
	workspace := filepath.Join(container, "active")
	common := filepath.Join(workspace, ".git")
	admin := filepath.Join(common, "worktrees", "active")
	siblingRepo := filepath.Join(container, "sibling-repo")
	for _, dir := range []string{workspace, common, admin, siblingRepo} {
		require.NoError(t, os.MkdirAll(dir, 0o755))
	}
	canonicalWorkspace, err := filepath.EvalSymlinks(workspace)
	require.NoError(t, err)

	got := strictHomeGitWriteDirs(workspace, common, []string{container, common, admin, siblingRepo})
	assert.Equal(t, []string{canonicalWorkspace, common, admin}, got)
	assert.NotContains(t, got, container, "strict Home must not reopen the historical sibling-worktree container")
	assert.NotContains(t, got, siblingRepo)
}

func TestCodexStrictHomeSessionRendererHostSmoke(t *testing.T) {
	if os.Getenv("TCLAUDE_CODEX_SPLIT_SMOKE") != "1" {
		t.Skip("set TCLAUDE_CODEX_SPLIT_SMOKE=1 on an unsandboxed Linux host with Codex+bubblewrap")
	}
	if runtime.GOOS != "linux" {
		t.Skip("Linux only")
	}
	home := t.TempDir()
	canonicalHome, err := filepath.EvalSymlinks(home)
	require.NoError(t, err)
	home = canonicalHome
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", t.TempDir())
	container := filepath.Join(home, "git")
	workspace := filepath.Join(container, "active")
	common := filepath.Join(workspace, ".git")
	admin := filepath.Join(common, "worktrees", "active")
	sibling := filepath.Join(container, "sibling")
	private := filepath.Join(home, ".tclaude", "data")
	breakGlass := filepath.Join(private, "acknowledged")
	explicitRead := filepath.Join(home, "explicit-read")
	explicitWrite := filepath.Join(home, "explicit-write")
	agentCache := filepath.Join(home, "agent-cache")
	for _, dir := range []string{workspace, common, admin, sibling, breakGlass, explicitRead, explicitWrite, agentCache} {
		require.NoError(t, os.MkdirAll(dir, 0o700))
	}
	effective, err := sandboxpolicy.Resolve(sandboxpolicy.Scopes{Explicit: &sandboxpolicy.Profile{
		Name:                   "strict-home",
		ReadBaselineExclusions: []string{sandboxpolicy.ReadExclusionHome},
		Filesystem: []sandboxpolicy.FilesystemGrant{
			{Path: explicitRead, Access: sandboxpolicy.AccessRead},
			{Path: explicitWrite, Access: sandboxpolicy.AccessWrite},
		},
		BreakGlassFilesystem: []sandboxpolicy.BreakGlassGrant{{Path: breakGlass, Access: sandboxpolicy.AccessRead}},
	}})
	require.NoError(t, err)
	effective.Filesystem = append(effective.Filesystem, sandboxpolicy.FilesystemGrant{Path: agentCache, Access: sandboxpolicy.AccessWrite})
	effective.AgentDirectories = []string{"GOCACHE"}
	effective.Environment = append(effective.Environment, sandboxpolicy.EnvironmentEntry{Name: "GOCACHE", Value: agentCache})
	snapshot := sandboxpolicy.NewSnapshot(effective, nil)
	params := &NewParams{
		PermissionProfile:          harness.CodexAgentProfile,
		CodexGitCommonDir:          common,
		CodexGitCommonDirPinned:    true,
		GitWorktreeWriteDirs:       []string{container, common, admin, sibling},
		GitWorktreeWriteDirsPinned: true,
	}
	_, profilePath, capability, err := ensureCodexManagedProfileWithSnapshot(params, workspace, "1234567890abcdef", &snapshot)
	require.NoError(t, err)
	require.NotNil(t, capability)
	require.NoError(t, harness.RevalidateCodexHomeSplitPolicyCapability(*capability))
	raw, err := os.ReadFile(profilePath)
	require.NoError(t, err)
	content := string(raw)
	for _, allowed := range []string{workspace, common, admin, explicitRead, explicitWrite, agentCache, breakGlass} {
		assert.Contains(t, content, `"`+allowed+`"`)
	}
	assert.NotContains(t, content, `"`+container+`" = "write"`)
	assert.NotContains(t, content, sibling)
}
