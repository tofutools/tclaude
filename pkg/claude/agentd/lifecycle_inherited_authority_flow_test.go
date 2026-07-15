package agentd_test

import (
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

type blockingFirstSpawn struct {
	inner   agentd.Spawner
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *blockingFirstSpawn) SpawnNew(args clcommon.SpawnArgs) error {
	blocked := false
	s.once.Do(func() {
		blocked = true
		close(s.entered)
	})
	if blocked {
		<-s.release
	}
	return s.inner.SpawnNew(args)
}

func (s *blockingFirstSpawn) SpawnResume(args clcommon.SpawnArgs) error {
	return s.inner.SpawnResume(args)
}

// TCL-510: a lifecycle caller authorizes an operation on the target; it does
// not delegate its own filesystem authority. In particular, a manager cannot
// write marker files in another Codex agent's private daemon-owned directory,
// and must not need to do so to reincarnate or clone that target in place.
func TestLifecycleInheritedAuthority_AgentOwnedDirectoriesNeedNoCallerProof(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cross bool
		clone bool
	}{
		{name: "self-reincarnate"},
		{name: "manager-reincarnate", cross: true},
		{name: "self-clone", clone: true},
		{name: "manager-clone", cross: true, clone: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_CACHE_HOME", t.TempDir())
			if tc.clone {
				previous := agentd.CloneCooldown
				agentd.CloneCooldown = 0
				t.Cleanup(func() { agentd.CloneCooldown = previous })
			}
			f := newFlow(t)
			group := f.HaveGroup("crew")
			_, err := db.CreateSandboxProfile(&db.SandboxProfile{
				Name: "private-go-cache", AgentDirectories: []string{"GOCACHE"},
			})
			require.NoError(t, err)
			_, err = db.SetAgentGroupSandboxProfile(group.Name, "private-go-cache")
			require.NoError(t, err)

			target := f.AsHuman().SpawnWith(group.Name, map[string]any{
				"name": "worker", "harness": "codex", "sandbox": "tclaude-agent",
				"cwd": t.TempDir(),
			})
			require.Equalf(t, http.StatusOK, target.Code, "spawn target body=%s", target.Raw)
			before, err := db.AgentEffectiveSandboxConfigForConv(target.ConvID)
			require.NoError(t, err)
			require.NotNil(t, before)
			require.Equal(t, []string{"GOCACHE"}, before.Effective.AgentDirectories)
			require.Len(t, before.Effective.Environment, 1)
			privateDir := before.Effective.Environment[0].Value
			assert.Contains(t, privateDir, filepath.Join("agent-dirs", target.Label))

			caller := target.ConvID
			path := "/v1/whoami/reincarnate"
			body := map[string]any{"follow_up": "continue the task"}
			if tc.clone {
				require.NoError(t, db.GrantAgentPermission(target.ConvID, agentd.PermSelfClone, "test"))
				path = "/v1/whoami/clone"
				body = map[string]any{"no_copy_conv": true}
			}
			if tc.cross {
				caller = "manager-aaaa-bbbb-cccc-111111111111"
				f.HaveAliveSession(caller, "manager-session", "manager-tmux", t.TempDir())
				f.HaveMember(group.Name, caller)
				require.NoError(t, db.AddAgentGroupOwner(group.ID, caller, "test"))
				if tc.clone {
					path = "/v1/agent/" + target.ConvID + "/clone"
				} else {
					path = "/v1/agent/" + target.ConvID + "/reincarnate"
				}
			}

			rec := agentReq(t, f, caller, http.MethodPost, path, body)
			require.Equalf(t, http.StatusOK, rec.Code,
				"inherited lifecycle authority must not challenge for target-owned directory %s; body=%s",
				privateDir, rec.Body.String())
		})
	}
}

func TestReincarnateRefreshesTargetSandboxProfile(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	f := newFlow(t)
	group := f.HaveGroup("crew")
	profileID, err := db.CreateSandboxProfile(&db.SandboxProfile{
		Name:             "refreshable",
		Environment:      []db.SandboxEnvironmentEntry{{Name: "POLICY_VERSION", Value: "v1"}},
		AgentDirectories: []string{"GOCACHE"},
	})
	require.NoError(t, err)
	_, err = db.SetAgentGroupSandboxProfile(group.Name, "refreshable")
	require.NoError(t, err)

	target := f.AsHuman().SpawnWith(group.Name, map[string]any{
		"name": "worker", "harness": "codex", "sandbox": "tclaude-agent",
		"cwd": t.TempDir(),
	})
	require.Equalf(t, http.StatusOK, target.Code, "spawn target body=%s", target.Raw)
	profile, err := db.GetSandboxProfileByID(profileID)
	require.NoError(t, err)
	profile.Environment = []db.SandboxEnvironmentEntry{{Name: "POLICY_VERSION", Value: "v2"}}
	require.NoError(t, db.UpdateSandboxProfile(profile))

	rec := agentReq(t, f, target.ConvID, http.MethodPost, "/v1/whoami/reincarnate",
		map[string]any{"follow_up": "continue under the updated profile"})
	require.Equalf(t, http.StatusOK, rec.Code, "reincarnate body=%s", rec.Body.String())
	var response struct {
		NewConv string `json:"new_conv"`
	}
	testharness.DecodeJSON(t, rec, &response)
	launched, ok := f.World.SpawnSandboxPolicy(response.NewConv)
	require.True(t, ok)
	require.NotNil(t, launched)
	assert.Contains(t, launched.Effective.Environment,
		db.SandboxEnvironmentEntry{Name: "POLICY_VERSION", Value: "v2"})
	persisted, err := db.AgentEffectiveSandboxConfigForConv(response.NewConv)
	require.NoError(t, err)
	require.NotNil(t, persisted)
	assert.Equal(t, launched.Effective, persisted.Effective)
}

func TestReincarnateTimeoutRestoresPreviousSandboxProfile(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Cleanup(agentd.SetReincarnateSpawnTimeoutForTest(20 * time.Millisecond))
	f := newFlow(t)
	group := f.HaveGroup("crew")
	profileID, err := db.CreateSandboxProfile(&db.SandboxProfile{
		Name:        "timeout-refresh",
		Environment: []db.SandboxEnvironmentEntry{{Name: "POLICY_VERSION", Value: "v1"}},
	})
	require.NoError(t, err)
	_, err = db.SetAgentGroupSandboxProfile(group.Name, "timeout-refresh")
	require.NoError(t, err)
	target := f.AsHuman().SpawnWith(group.Name, map[string]any{
		"name": "worker", "cwd": t.TempDir(),
	})
	require.Equal(t, http.StatusOK, target.Code)
	previous, err := db.AgentEffectiveSandboxConfigForConv(target.ConvID)
	require.NoError(t, err)
	require.NotNil(t, previous)
	profile, err := db.GetSandboxProfileByID(profileID)
	require.NoError(t, err)
	profile.Environment = []db.SandboxEnvironmentEntry{{Name: "POLICY_VERSION", Value: "v2"}}
	require.NoError(t, db.UpdateSandboxProfile(profile))
	f.World.SkipSpawnRow = true

	rec := agentReq(t, f, target.ConvID, http.MethodPost, "/v1/whoami/reincarnate",
		map[string]any{"follow_up": "continue"})
	require.Equal(t, http.StatusGatewayTimeout, rec.Code, "body=%s", rec.Body.String())
	persisted, err := db.AgentEffectiveSandboxConfigForConv(target.ConvID)
	require.NoError(t, err)
	require.NotNil(t, persisted)
	assert.Equal(t, previous.Effective, persisted.Effective,
		"a failed successor must not change the live predecessor's durable policy")
}

func TestConcurrentReincarnationsCannotRollbackWinnerPolicy(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	f := newFlow(t)
	group := f.HaveGroup("crew")
	profileID, err := db.CreateSandboxProfile(&db.SandboxProfile{
		Name:        "serialized-refresh",
		Environment: []db.SandboxEnvironmentEntry{{Name: "POLICY_VERSION", Value: "v1"}},
	})
	require.NoError(t, err)
	_, err = db.SetAgentGroupSandboxProfile(group.Name, "serialized-refresh")
	require.NoError(t, err)
	target := f.AsHuman().SpawnWith(group.Name, map[string]any{
		"name": "worker", "harness": "codex", "sandbox": "tclaude-agent", "cwd": t.TempDir(),
	})
	require.Equal(t, http.StatusOK, target.Code)
	profile, err := db.GetSandboxProfileByID(profileID)
	require.NoError(t, err)
	profile.Environment = []db.SandboxEnvironmentEntry{{Name: "POLICY_VERSION", Value: "v2"}}
	require.NoError(t, db.UpdateSandboxProfile(profile))

	previousSpawner := agentd.Spawn
	blocking := &blockingFirstSpawn{inner: previousSpawner, entered: make(chan struct{}), release: make(chan struct{})}
	agentd.Spawn = blocking
	t.Cleanup(func() { agentd.Spawn = previousSpawner })
	req1 := agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/whoami/reincarnate",
		map[string]any{"follow_up": "first"}), target.ConvID)
	req2 := agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/whoami/reincarnate",
		map[string]any{"follow_up": "second"}), target.ConvID)
	results := make(chan int, 2)
	go func() { results <- testharness.Serve(f.Mux, req1).Code }()
	<-blocking.entered
	go func() { results <- testharness.Serve(f.Mux, req2).Code }()
	close(blocking.release)
	statuses := []int{<-results, <-results}
	assert.ElementsMatch(t, []int{http.StatusOK, http.StatusConflict}, statuses)
	actor, err := db.GetAgentByConv(target.ConvID)
	require.NoError(t, err)
	require.NotNil(t, actor)
	persisted, err := db.AgentEffectiveSandboxConfigForConv(actor.CurrentConvID)
	require.NoError(t, err)
	require.NotNil(t, persisted)
	assert.Contains(t, persisted.Effective.Environment,
		db.SandboxEnvironmentEntry{Name: "POLICY_VERSION", Value: "v2"})
}

func TestReincarnateDefersSupersededDirectoryCleanupUntilPredecessorExits(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	f := newFlow(t)
	group := f.HaveGroup("crew")
	profileID, err := db.CreateSandboxProfile(&db.SandboxProfile{
		Name: "directory-refresh", AgentDirectories: []string{"GOTMPDIR"},
	})
	require.NoError(t, err)
	_, err = db.SetAgentGroupSandboxProfile(group.Name, "directory-refresh")
	require.NoError(t, err)
	target := f.AsHuman().SpawnWith(group.Name, map[string]any{
		"name": "worker", "harness": "codex", "sandbox": "tclaude-agent", "cwd": t.TempDir(),
	})
	require.Equal(t, http.StatusOK, target.Code)
	before, err := db.AgentEffectiveSandboxConfigForConv(target.ConvID)
	require.NoError(t, err)
	var oldDir string
	for _, entry := range before.Effective.Environment {
		if entry.Name == "GOTMPDIR" {
			oldDir = entry.Value
		}
	}
	require.NotEmpty(t, oldDir)
	profile, err := db.GetSandboxProfileByID(profileID)
	require.NoError(t, err)
	profile.AgentDirectories = []string{"GOCACHE"}
	require.NoError(t, db.UpdateSandboxProfile(profile))
	codex := f.World.Codexes.GetByConvID(target.ConvID)
	require.NotNil(t, codex)
	codex.SetCommandDelay("/quit", 200*time.Millisecond)

	rec := agentReq(t, f, target.ConvID, http.MethodPost, "/v1/whoami/reincarnate",
		map[string]any{"follow_up": "continue"})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	_, statErr := os.Stat(oldDir)
	require.NoError(t, statErr, "old directory must remain while the predecessor is still exiting")
	agentd.WaitForBackgroundForTest()
	_, statErr = os.Stat(oldDir)
	assert.True(t, os.IsNotExist(statErr), "old directory should be removed after predecessor exit; err=%v", statErr)
}
