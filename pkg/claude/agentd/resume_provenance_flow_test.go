package agentd_test

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

type observingResumeSpawner struct {
	inner  agentd.Spawner
	mu     sync.Mutex
	args   clcommon.SpawnArgs
	mutate func(clcommon.SpawnArgs) error
}

func canonicalResumeTestPath(t *testing.T, path string) string {
	t.Helper()
	physical, err := filepath.EvalSymlinks(path)
	require.NoError(t, err)
	return physical
}

func (s *observingResumeSpawner) SpawnNew(args clcommon.SpawnArgs) error {
	return s.inner.SpawnNew(args)
}

func (s *observingResumeSpawner) SpawnResume(args clcommon.SpawnArgs) error {
	s.mu.Lock()
	s.args = args
	s.mu.Unlock()
	if s.mutate != nil {
		if err := s.mutate(args); err != nil {
			return err
		}
	}
	return s.inner.SpawnResume(args)
}

func installObservingResumeSpawner(t *testing.T, mutate func(clcommon.SpawnArgs) error) *observingResumeSpawner {
	t.Helper()
	previous := agentd.Spawn
	observer := &observingResumeSpawner{inner: previous, mutate: mutate}
	agentd.Spawn = observer
	t.Cleanup(func() { agentd.Spawn = previous })
	return observer
}

func ownerResumeFixture(t *testing.T, f *testFlow, target, caller string) *db.AgentGroup {
	t.Helper()
	group := f.HaveGroup("resume-provenance-" + target[:8])
	f.HaveMember(group.Name, target)
	require.NoError(t, db.AddAgentGroupOwner(group.ID, caller, "test"))
	return group
}

// testFlow is only an alias to keep the fixture signature readable without
// exporting the concrete testharness type from newFlow's local helper.
type testFlow = testharness.Flow

func TestResumeProvenance_CwdSymlinkRetargetUsesOriginalPhysicalTarget(t *testing.T) {
	f := newFlow(t)
	const (
		target = "resume-symlink-target-aaaa-bbbb-111111111111"
		caller = "resume-symlink-owner-aaaa-bbbb-111111111111"
	)
	root := t.TempDir()
	original := filepath.Join(root, "original")
	unrelated := filepath.Join(root, "unrelated")
	require.NoError(t, os.Mkdir(original, 0o755))
	require.NoError(t, os.Mkdir(unrelated, 0o755))
	physicalOriginal := canonicalResumeTestPath(t, original)
	launchPath := filepath.Join(root, "launch")
	require.NoError(t, os.Symlink(original, launchPath))
	f.HaveConvWithTitle(target, "resume-symlink-target")
	f.HaveAliveSession(target, "resume-symlink-session", "resume-symlink-tmux", launchPath)
	f.MarkOffline("resume-symlink-tmux")
	ownerResumeFixture(t, f, target, caller)
	require.NoError(t, os.Remove(launchPath))
	require.NoError(t, os.Symlink(unrelated, launchPath))
	observer := installObservingResumeSpawner(t, nil)

	rec := agentReq(t, f, caller, http.MethodPost, "/v1/agent/"+target+"/resume", nil)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	observer.mu.Lock()
	launchedCwd := observer.args.Cwd
	observer.mu.Unlock()
	assert.Equal(t, physicalOriginal, launchedCwd,
		"offline resume must use durable physical cwd, not follow the retargeted launch spelling")
}

func TestResumeProvenance_RepositoryReplacementFailsClosedForOwner(t *testing.T) {
	f := newFlow(t)
	const (
		target = "resume-repo-target-aaaa-bbbb-111111111111"
		caller = "resume-repo-owner-aaaa-bbbb-111111111111"
	)
	repo := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, os.Mkdir(repo, 0o755))
	require.NoError(t, exec.Command("git", "init", "-q", repo).Run())
	f.HaveConvWithTitle(target, "resume-repo-target")
	f.HaveAliveSession(target, "resume-repo-session", "resume-repo-tmux", repo)
	f.MarkOffline("resume-repo-tmux")
	ownerResumeFixture(t, f, target, caller)
	require.NoError(t, os.Rename(filepath.Join(repo, ".git"), filepath.Join(repo, ".git-old")))
	require.NoError(t, exec.Command("git", "init", "-q", repo).Run())

	rec := agentReq(t, f, caller, http.MethodPost, "/v1/agent/"+target+"/resume", nil)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var response struct {
		Action string `json:"action"`
		Detail string `json:"detail"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
	assert.Equal(t, "error:resume_provenance", response.Action)
	assert.Contains(t, response.Detail, "Git")
	_, launched := f.World.SpawnCwdWriteProof(target)
	assert.False(t, launched)
	assertNoDirWriteProofMarkers(t, repo)
	assertNoDirWriteProofMarkers(t, filepath.Join(repo, ".git"))
	assertNoDirWriteProofMarkers(t, filepath.Join(repo, ".git-old"))
}

func TestResumeProvenance_MalformedMetadataFailsClosedForOwner(t *testing.T) {
	f := newFlow(t)
	const (
		target = "resume-malformed-target-aaaa-bbbb-111111111111"
		caller = "resume-malformed-owner-aaaa-bbbb-111111111111"
	)
	f.HaveConvWithTitle(target, "resume-malformed-target")
	f.HaveAliveSession(target, "resume-malformed-session", "resume-malformed-tmux", t.TempDir())
	f.MarkOffline("resume-malformed-tmux")
	ownerResumeFixture(t, f, target, caller)
	source, err := db.FindSessionByConvID(target)
	require.NoError(t, err)
	require.NoError(t, db.SetSessionResumeProvenance(source.ID, `{"version":1,"unknown":true}`))

	rec := agentReq(t, f, caller, http.MethodPost, "/v1/agent/"+target+"/resume", nil)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "error:resume_provenance")
	_, launched := f.World.SpawnCwdWriteProof(target)
	assert.False(t, launched)
}

func TestResumeProvenance_ProductionHandoffRejectsCwdReplacementAndCleansPin(t *testing.T) {
	f := newFlow(t)
	const (
		target = "resume-race-target-aaaa-bbbb-111111111111"
		caller = "resume-race-owner-aaaa-bbbb-111111111111"
	)
	root := t.TempDir()
	cwd := filepath.Join(root, "cwd")
	moved := filepath.Join(root, "cwd-old")
	require.NoError(t, os.Mkdir(cwd, 0o755))
	physicalCwd := canonicalResumeTestPath(t, cwd)
	f.HaveConvWithTitle(target, "resume-race-target")
	f.HaveAliveSession(target, "resume-race-session", "resume-race-tmux", cwd)
	f.MarkOffline("resume-race-tmux")
	ownerResumeFixture(t, f, target, caller)
	installObservingResumeSpawner(t, func(args clcommon.SpawnArgs) error {
		require.Equal(t, physicalCwd, args.Cwd)
		require.NoError(t, os.Rename(cwd, moved))
		return os.Mkdir(cwd, 0o755)
	})

	rec := agentReq(t, f, caller, http.MethodPost, "/v1/agent/"+target+"/resume", nil)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "spawn dir proof marker",
		"the production SpawnArgs handoff must reach the child-side cwd marker guard")
	assertNoDirWriteProofMarkers(t, cwd)
	assertNoDirWriteProofMarkers(t, moved)
	_, launched := f.World.SpawnCwdWriteProof(target)
	assert.False(t, launched)
}
