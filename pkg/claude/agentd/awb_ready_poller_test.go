package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/usageapi"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

func TestAWBReadyPickupLogsSelectedDispatch(t *testing.T) {
	setupTestDB(t)
	t.Setenv("AWB_PASSWORD", "hunter2")
	_, err := db.CreateAgentGroup("builders", "")
	require.NoError(t, err)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		issue := awbIssue{ID: "tcl-a1", Workspace: "tcl", Status: "open"}
		if r.URL.Path == "/api/issues/tcl-a1" {
			issue.Status = "closed"
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/ready" {
			if err := json.NewEncoder(w).Encode([]awbIssue{issue}); err != nil {
				t.Errorf("encode ready response: %v", err)
			}
			return
		}
		if r.URL.Path != "/api/issues/tcl-a1" {
			t.Errorf("request path = %q, want /api/issues/tcl-a1", r.URL.Path)
		}
		if err := json.NewEncoder(w).Encode(issue); err != nil {
			t.Errorf("encode issue response: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	base, fault := validateAWBBaseURL(server.URL)
	require.Nil(t, fault)
	policy := config.AWBProxyConfig{URL: server.URL, Username: "worker", AllowWrite: true, AllowedWorkspaces: []string{"tcl"}}
	worker := awbReadyWorker{
		process:   "builders",
		workspace: "tcl",
		config:    config.AWBReadyPollingConfig{Workspace: "tcl", Group: "builders", Cwd: t.TempDir()},
		session:   &awbProxySession{policy: policy, base: base, workspaces: []string{"tcl"}},
	}
	require.NoError(t, worker.tick(context.Background()))

	got := logs.String()
	assert.Contains(t, got, `"level":"INFO"`)
	assert.Contains(t, got, `"msg":"awb ready polling: picked up issue"`)
	assert.Contains(t, got, `"process":"builders"`)
	assert.Contains(t, got, `"workspace":"tcl"`)
	assert.Contains(t, got, `"issue":"tcl-a1"`)
	assert.Contains(t, got, `"agent_id":"agt_`)
	assert.Equal(t, 1, strings.Count(got, `"msg":"awb ready polling: picked up issue"`))
	dispatch, err := db.GetAWBReadyDispatch("builders")
	require.NoError(t, err)
	assert.Nil(t, dispatch, "the closed issue releases the dispatch after logging its pickup")
}

func TestAWBReadyPickupWarnsWhenCommitMonitorHasNoOrigin(t *testing.T) {
	setupTestDB(t)
	t.Setenv("AWB_PASSWORD", "hunter2")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	repo := t.TempDir()
	out, err := exec.Command("git", "init", "-b", "main", repo).CombinedOutput()
	require.NoError(t, err, "%s", out)
	_, err = db.CreateAgentGroup("builders", "")
	require.NoError(t, err)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status := "open"
		if r.URL.Path != "/api/ready" {
			status = "closed"
		}
		issue := awbIssue{ID: "tcl-a1", Workspace: "tcl", Status: status}
		if r.URL.Path == "/api/ready" {
			require.NoError(t, json.NewEncoder(w).Encode([]awbIssue{issue}))
		} else {
			require.NoError(t, json.NewEncoder(w).Encode(issue))
		}
	}))
	t.Cleanup(server.Close)
	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	base, fault := validateAWBBaseURL(server.URL)
	require.Nil(t, fault)
	policy := config.AWBProxyConfig{URL: server.URL, Username: "worker", AllowWrite: true, AllowedWorkspaces: []string{"tcl"}}
	worker := awbReadyWorker{process: "builders", workspace: "tcl",
		config:  config.AWBReadyPollingConfig{Workspace: "tcl", Group: "builders", Cwd: repo, MonitorCommit: true},
		session: &awbProxySession{policy: policy, base: base, workspaces: []string{"tcl"}}}
	require.NoError(t, worker.tick(context.Background()))
	assert.Contains(t, logs.String(), "monitor_commit has no effect without an origin remote")
}

func TestAWBReadyPickupDoesNotLogResumedDispatch(t *testing.T) {
	setupTestDB(t)
	t.Setenv("AWB_PASSWORD", "hunter2")
	_, err := db.CreateAgentGroup("builders", "")
	require.NoError(t, err)
	selected, err := db.SelectAWBReadyDispatch("builders", "tcl", "tcl-a1", "agt_reserved")
	require.NoError(t, err)
	require.True(t, selected)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/issues/tcl-a1" {
			t.Errorf("request path = %q, want /api/issues/tcl-a1", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(awbIssue{ID: "tcl-a1", Workspace: "tcl", Status: "closed"}); err != nil {
			t.Errorf("encode issue response: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	base, fault := validateAWBBaseURL(server.URL)
	require.Nil(t, fault)
	policy := config.AWBProxyConfig{URL: server.URL, Username: "worker", AllowWrite: true, AllowedWorkspaces: []string{"tcl"}}
	worker := awbReadyWorker{
		process:   "builders",
		workspace: "tcl",
		config:    config.AWBReadyPollingConfig{Workspace: "tcl", Group: "builders", Cwd: t.TempDir()},
		session:   &awbProxySession{policy: policy, base: base, workspaces: []string{"tcl"}},
	}
	require.NoError(t, worker.tick(context.Background()))
	assert.NotContains(t, logs.String(), "awb ready polling: picked up issue")
}

func TestAWBReadyInitialMessageLeavesClosureToOperator(t *testing.T) {
	message := awbReadyInitialMessage("tcl-a1", false, false, false)
	assert.Contains(t, message, "tclaude proxy awb show tcl-a1")
	assert.Contains(t, message, "record progress")
	assert.Contains(t, message, "Leave closing the issue to the operator")
	assert.NotContains(t, strings.ToLower(message), "close it")
}

func TestAWBReadyInitialMessageExplainsPRMonitoring(t *testing.T) {
	message := awbReadyInitialMessage("tcl-a1", true, false, false)
	assert.Contains(t, message, "awb update --pull-request-url")
	assert.Contains(t, message, "daemon will close the issue")
}

func TestAWBReadyInitialMessageExplainsCommitMonitoring(t *testing.T) {
	message := awbReadyInitialMessage("tcl-a1", false, true, false)
	assert.Contains(t, message, "awb update --commit-hash")
	assert.Contains(t, message, "origin/main")
	assert.Contains(t, message, "daemon will close the issue")
}

func TestAWBReadyInitialMessageExplainsAgentClosure(t *testing.T) {
	message := awbReadyInitialMessage("tcl-a1", false, false, true)
	assert.Contains(t, message, "tclaude proxy awb close tcl-a1")
	assert.Contains(t, message, "daemon will clean up")
	assert.NotContains(t, message, "Leave closing the issue to the operator")
}

func TestAWBReadyMonitorCloseWaitsForSettledAgent(t *testing.T) {
	setupTestDB(t)
	t.Setenv("AWB_PASSWORD", "hunter2")
	agentID := testAWBReadyAgent(t, session.StatusWorking)
	selected, err := db.SelectAWBReadyDispatch("builders", "tcl", "tcl-a1", agentID)
	require.NoError(t, err)
	require.True(t, selected)
	_, err = db.UpdateAWBReadyDispatch("builders", "tcl-a1", "spawned", "")
	require.NoError(t, err)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		require.NoError(t, json.NewEncoder(w).Encode(awbIssue{ID: "tcl-a1", Workspace: "tcl", Status: "closed"}))
	}))
	t.Cleanup(server.Close)
	worker := testAWBReadyMonitorWorker(t, server.URL)
	worker.config.MonitorPR = false
	worker.config.MonitorClose = true
	require.NoError(t, worker.tick(context.Background()))
	dispatch, err := db.GetAWBReadyDispatch("builders")
	require.NoError(t, err)
	assert.NotNil(t, dispatch)
}

func TestAWBReadyMonitorCloseAdvancesAfterAgentSettles(t *testing.T) {
	setupTestDB(t)
	t.Setenv("AWB_PASSWORD", "hunter2")
	selected, err := db.SelectAWBReadyDispatch("builders", "tcl", "tcl-a1", "agt_missing")
	require.NoError(t, err)
	require.True(t, selected)
	_, err = db.UpdateAWBReadyDispatch("builders", "tcl-a1", "spawned", "")
	require.NoError(t, err)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		require.NoError(t, json.NewEncoder(w).Encode(awbIssue{ID: "tcl-a1", Workspace: "tcl", Status: "closed"}))
	}))
	t.Cleanup(server.Close)
	worker := testAWBReadyMonitorWorker(t, server.URL)
	worker.config.MonitorPR = false
	worker.config.MonitorClose = true
	require.NoError(t, worker.tick(context.Background()))
	dispatch, err := db.GetAWBReadyDispatch("builders")
	require.NoError(t, err)
	assert.Nil(t, dispatch)
}

func TestAWBReadyMonitorCloseRecoversSpawnedAgentFromClaimedPhase(t *testing.T) {
	setupTestDB(t)
	t.Setenv("AWB_PASSWORD", "hunter2")
	_, err := db.CreateAgentGroup("builders", "")
	require.NoError(t, err)
	agentID := testAWBReadyAgent(t, session.StatusIdle)
	selected, err := db.SelectAWBReadyDispatch("builders", "tcl", "tcl-a1", agentID)
	require.NoError(t, err)
	require.True(t, selected)
	_, err = db.UpdateAWBReadyDispatch("builders", "tcl-a1", "claimed", "")
	require.NoError(t, err)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		require.NoError(t, json.NewEncoder(w).Encode(awbIssue{ID: "tcl-a1", Workspace: "tcl", Status: "closed"}))
	}))
	t.Cleanup(server.Close)
	worker := testAWBReadyMonitorWorker(t, server.URL)
	worker.config.MonitorPR = false
	worker.config.MonitorClose = true
	require.NoError(t, worker.tick(context.Background()))
	dispatch, err := db.GetAWBReadyDispatch("builders")
	require.NoError(t, err)
	assert.Nil(t, dispatch)
	agent, err := db.GetAgent(agentID)
	require.NoError(t, err)
	require.NotNil(t, agent)
	assert.False(t, agent.Active())
}

func TestAWBReadyMonitorCloseRetriesCleanupWhenAgentResumes(t *testing.T) {
	setupTestDB(t)
	t.Setenv("AWB_PASSWORD", "hunter2")
	installEmptyTmuxForTest(t)
	agentID := testAWBReadyAgent(t, session.StatusIdle)
	a, err := db.GetAgent(agentID)
	require.NoError(t, err)
	_, err = db.PromoteAgent(a.CurrentConvID, "promote")
	require.NoError(t, err)
	selected, err := db.SelectAWBReadyDispatch("builders", "tcl", "tcl-a1", agentID)
	require.NoError(t, err)
	require.True(t, selected)
	_, err = db.UpdateAWBReadyDispatch("builders", "tcl-a1", "spawned", "")
	require.NoError(t, err)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		require.NoError(t, json.NewEncoder(w).Encode(awbIssue{ID: "tcl-a1", Workspace: "tcl", Status: "closed"}))
	}))
	t.Cleanup(server.Close)
	worker := testAWBReadyMonitorWorker(t, server.URL)
	worker.config.MonitorPR = false
	worker.config.MonitorClose = true
	previous := awbReadyStillSettledFn
	awbReadyStillSettledFn = func(string) error { return errAWBReadyAgentBusy }
	t.Cleanup(func() { awbReadyStillSettledFn = previous })
	require.NoError(t, worker.tick(context.Background()))
	dispatch, err := db.GetAWBReadyDispatch("builders")
	require.NoError(t, err)
	assert.NotNil(t, dispatch)
	awbReadyStillSettledFn = previous
	require.NoError(t, worker.tick(context.Background()))
	dispatch, err = db.GetAWBReadyDispatch("builders")
	require.NoError(t, err)
	assert.Nil(t, dispatch)
	state, err := db.AgentState(a.CurrentConvID)
	require.NoError(t, err)
	assert.Equal(t, db.AgentStateRetired, state)
}

func TestAWBReadyMonitorClosesMergedPRAfterAgentSettles(t *testing.T) {
	setupTestDB(t)
	t.Setenv("AWB_PASSWORD", "hunter2")
	agentID := testAWBReadyAgent(t, session.StatusIdle)
	selected, err := db.SelectAWBReadyDispatch("builders", "tcl", "tcl-a1", agentID)
	require.NoError(t, err)
	require.True(t, selected)
	_, err = db.UpdateAWBReadyDispatch("builders", "tcl-a1", "spawned", "")
	require.NoError(t, err)

	var closed bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		issue := awbIssue{ID: "tcl-a1", Workspace: "tcl", Status: "in_progress", PullRequestURL: "https://github.com/acme/repo/pull/42"}
		if r.Method == http.MethodPost && r.URL.Path == "/api/issues/tcl-a1/close" {
			closed = true
			issue.Status = "closed"
		}
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(issue))
	}))
	t.Cleanup(server.Close)

	testAWBReadyGitHub(t, "MERGED")

	worker := testAWBReadyMonitorWorker(t, server.URL)
	require.NoError(t, worker.tick(context.Background()))
	assert.True(t, closed)
	dispatch, err := db.GetAWBReadyDispatch("builders")
	require.NoError(t, err)
	assert.Nil(t, dispatch, "successful automatic closure releases the serial polling process")
}

func TestAWBReadyMonitorWaitsWhileAgentWorks(t *testing.T) {
	setupTestDB(t)
	t.Setenv("AWB_PASSWORD", "hunter2")
	agentID := testAWBReadyAgent(t, session.StatusWorking)
	selected, err := db.SelectAWBReadyDispatch("builders", "tcl", "tcl-a1", agentID)
	require.NoError(t, err)
	require.True(t, selected)
	_, err = db.UpdateAWBReadyDispatch("builders", "tcl-a1", "spawned", "")
	require.NoError(t, err)

	var closeRequests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			closeRequests++
		}
		_ = json.NewEncoder(w).Encode(awbIssue{ID: "tcl-a1", Workspace: "tcl", Status: "in_progress", PullRequestURL: "https://github.com/acme/repo/pull/42"})
	}))
	t.Cleanup(server.Close)

	testAWBReadyGitHub(t, "MERGED")

	require.NoError(t, testAWBReadyMonitorWorker(t, server.URL).tick(context.Background()))
	assert.Zero(t, closeRequests)
	dispatch, err := db.GetAWBReadyDispatch("builders")
	require.NoError(t, err)
	assert.NotNil(t, dispatch)
}

func TestAWBReadyMonitorDisabledDoesNotInspectPR(t *testing.T) {
	setupTestDB(t)
	t.Setenv("AWB_PASSWORD", "hunter2")
	agentID := testAWBReadyAgent(t, session.StatusIdle)
	selected, err := db.SelectAWBReadyDispatch("builders", "tcl", "tcl-a1", agentID)
	require.NoError(t, err)
	require.True(t, selected)
	_, err = db.UpdateAWBReadyDispatch("builders", "tcl-a1", "spawned", "")
	require.NoError(t, err)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(awbIssue{ID: "tcl-a1", Workspace: "tcl", Status: "in_progress", PullRequestURL: "https://github.com/acme/repo/pull/42"})
	}))
	t.Cleanup(server.Close)
	testAWBReadyGitHub(t, "MERGED")

	worker := testAWBReadyMonitorWorker(t, server.URL)
	worker.config.MonitorPR = false
	require.NoError(t, worker.tick(context.Background()))
}

func TestAWBReadyMonitorCommitClosesAfterAgentSettles(t *testing.T) {
	setupTestDB(t)
	t.Setenv("AWB_PASSWORD", "hunter2")
	agentID := testAWBReadyAgent(t, session.StatusIdle)
	selected, err := db.SelectAWBReadyDispatch("builders", "tcl", "tcl-a1", agentID)
	require.NoError(t, err)
	require.True(t, selected)
	_, err = db.UpdateAWBReadyDispatch("builders", "tcl-a1", "spawned", "")
	require.NoError(t, err)

	const hash = "0123456789abcdef0123456789abcdef01234567"
	var closeReason string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		issue := awbIssue{ID: "tcl-a1", Workspace: "tcl", Status: "in_progress", CommitHash: hash}
		if r.Method == http.MethodPost && r.URL.Path == "/api/issues/tcl-a1/close" {
			var body awbCloseBody
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.NotNil(t, body.Reason)
			closeReason = *body.Reason
			issue.Status = "closed"
		}
		require.NoError(t, json.NewEncoder(w).Encode(issue))
	}))
	t.Cleanup(server.Close)

	previous := liveAWBReadyCommitOnMainFn
	liveAWBReadyCommitOnMainFn = func(_ context.Context, cwd, commit string) (bool, bool, error) {
		assert.Equal(t, "/repo", cwd)
		assert.Equal(t, hash, commit)
		return true, true, nil
	}
	t.Cleanup(func() { liveAWBReadyCommitOnMainFn = previous })

	worker := testAWBReadyMonitorWorker(t, server.URL)
	worker.config.MonitorPR = false
	worker.config.MonitorCommit = true
	worker.config.Cwd = "/repo"
	require.NoError(t, worker.tick(context.Background()))
	assert.Equal(t, "Recorded commit reached origin/main and spawned agent settled", closeReason)
	dispatch, err := db.GetAWBReadyDispatch("builders")
	require.NoError(t, err)
	assert.Nil(t, dispatch)
}

func TestAWBReadyCommitOnOriginMain(t *testing.T) {
	setupTestDB(t)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	require.NoError(t, config.Save(&config.Config{Agent: &config.AgentConfig{GitProxy: &config.GitProxyConfig{
		AllowedRemotes: []string{"github.com/acme/repo"},
	}}}))
	repo := filepath.Join(t.TempDir(), "repo")
	git := func(dir string, args ...string) string {
		t.Helper()
		cmdArgs := args
		if dir != "" {
			cmdArgs = append([]string{"-C", dir}, args...)
		}
		out, err := exec.Command("git", cmdArgs...).CombinedOutput()
		require.NoError(t, err, "%s", out)
		return strings.TrimSpace(string(out))
	}
	git("", "init", "-b", "main", repo)
	git(repo, "config", "user.email", "test@example.invalid")
	git(repo, "config", "user.name", "tclaude test")
	git(repo, "config", "commit.gpgsign", "false")
	git(repo, "remote", "add", "origin", "https://github.com/acme/repo.git")
	git(repo, "commit", "--allow-empty", "-m", "pushed")
	pushed := git(repo, "rev-parse", "HEAD")

	realExec := proxyExec
	var fetchCommand ProxyCommand
	var fetchCalls int
	t.Cleanup(SetProxyExecForTest(func(ctx context.Context, cmd ProxyCommand) (ProxyResult, error) {
		isFetch := false
		for _, arg := range cmd.Args {
			isFetch = isFetch || arg == "fetch"
		}
		if isFetch {
			fetchCalls++
			fetchCommand = cmd
			require.NoError(t, os.WriteFile(filepath.Join(cmd.Dir, "FETCH_HEAD"), []byte(pushed+"\n"), 0o600))
			return ProxyResult{}, nil
		}
		return realExec(ctx, cmd)
	}))

	reached, local, err := liveAWBReadyCommitOnMain(context.Background(), repo, pushed)
	require.NoError(t, err)
	assert.True(t, reached)
	assert.False(t, local)
	assert.Contains(t, strings.Join(fetchCommand.Args, " "), "-c core.hooksPath=")
	assert.Contains(t, fetchCommand.Args, gitProxyUploadPack)
	assert.Contains(t, fetchCommand.Args, "https://github.com/acme/repo.git")

	git(repo, "commit", "--allow-empty", "-m", "local only")
	localOnly := git(repo, "rev-parse", "HEAD")
	reached, local, err = liveAWBReadyCommitOnMain(context.Background(), repo, localOnly)
	require.NoError(t, err)
	assert.False(t, reached)
	assert.False(t, local)

	reached, _, err = liveAWBReadyCommitOnMain(context.Background(), repo, strings.Repeat("f", 40))
	require.NoError(t, err)
	assert.False(t, reached, "an object not fetched yet is a normal waiting state")

	before := fetchCalls
	require.NoError(t, config.Save(&config.Config{}))
	_, _, err = liveAWBReadyCommitOnMain(context.Background(), repo, pushed)
	assert.ErrorContains(t, err, gitProxyDisabledMessage)
	assert.Equal(t, before, fetchCalls, "an origin without an allow-list must be refused before fetch")

	require.NoError(t, config.Save(&config.Config{Agent: &config.AgentConfig{GitProxy: &config.GitProxyConfig{
		AllowedRemotes: []string{"github.com/acme/repo"},
	}}}))
	git(repo, "remote", "set-url", "origin", "https://attacker.invalid/acme/repo.git")
	_, _, err = liveAWBReadyCommitOnMain(context.Background(), repo, pushed)
	assert.ErrorContains(t, err, "not on the operator's allow-list")
	assert.Equal(t, before, fetchCalls, "an unauthorized origin must be refused before fetch")
}

func TestAWBReadyCommitDoesNotMonitorLocalMainWithoutOrigin(t *testing.T) {
	setupTestDB(t)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	repo := filepath.Join(t.TempDir(), "repo")
	git := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput()
		require.NoError(t, err, "%s", out)
		return strings.TrimSpace(string(out))
	}
	require.NoError(t, exec.Command("git", "init", "-b", "main", repo).Run())
	git("config", "user.email", "test@example.invalid")
	git("config", "user.name", "tclaude test")
	git("config", "commit.gpgsign", "false")
	git("commit", "--allow-empty", "-m", "on main")
	onMain := git("rev-parse", "HEAD")

	reached, local, err := liveAWBReadyCommitOnMain(context.Background(), repo, onMain)
	require.NoError(t, err)
	assert.False(t, reached)
	assert.False(t, local)

	git("switch", "-c", "feature")
	git("commit", "--allow-empty", "-m", "feature only")
	featureOnly := git("rev-parse", "HEAD")
	reached, local, err = liveAWBReadyCommitOnMain(context.Background(), repo, featureOnly)
	require.NoError(t, err)
	assert.False(t, reached)
	assert.False(t, local)

	git("switch", "main")
	git("merge", "--ff-only", "feature")
	reached, local, err = liveAWBReadyCommitOnMain(context.Background(), repo, featureOnly)
	require.NoError(t, err)
	assert.False(t, reached)
	assert.False(t, local)
}

func TestAWBReadyCommitWithoutOriginRejectsNonRepository(t *testing.T) {
	setupTestDB(t)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	_, _, err := liveAWBReadyCommitOnMain(context.Background(), t.TempDir(), strings.Repeat("a", 40))
	assert.ErrorContains(t, err, "validate git repository")
}

func TestAWBReadyAgentSettledForMissingAgent(t *testing.T) {
	setupTestDB(t)
	settled, err := liveAWBReadyAgentSettled("agt_missing")
	require.NoError(t, err)
	assert.True(t, settled, "an absent actor cannot still be working")
}

func TestAWBReadyMonitorCloseWaitsForPendingEnrollment(t *testing.T) {
	setupTestDB(t)
	t.Setenv("AWB_PASSWORD", "hunter2")
	const agentID = "agt_pending"
	require.NoError(t, db.InsertPendingSpawn(&db.PendingSpawn{
		Label: "spwn-pending", AgentID: agentID, GroupID: 1}))
	selected, err := db.SelectAWBReadyDispatch("builders", "tcl", "tcl-a1", agentID)
	require.NoError(t, err)
	require.True(t, selected)
	_, err = db.UpdateAWBReadyDispatch("builders", "tcl-a1", "spawned", "")
	require.NoError(t, err)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		require.NoError(t, json.NewEncoder(w).Encode(awbIssue{ID: "tcl-a1", Workspace: "tcl", Status: "closed"}))
	}))
	t.Cleanup(server.Close)
	worker := testAWBReadyMonitorWorker(t, server.URL)
	worker.config.MonitorPR = false
	worker.config.MonitorClose = true
	require.NoError(t, worker.tick(context.Background()))
	dispatch, err := db.GetAWBReadyDispatch("builders")
	require.NoError(t, err)
	assert.NotNil(t, dispatch, "a pending spawn can still enroll after the issue closes")
}

func TestAWBReadyAgentSettledForMissingSession(t *testing.T) {
	setupTestDB(t)
	agentID, err := db.AllocateAgent("conv-pruned", "spawn")
	require.NoError(t, err)
	settled, err := liveAWBReadyAgentSettled(agentID)
	require.NoError(t, err)
	assert.True(t, settled, "a pruned session row means the actor cannot still be working")
}

func testAWBReadyAgent(t *testing.T, status string) string {
	t.Helper()
	convID := "conv-" + strings.ReplaceAll(status, "_", "-")
	agentID, err := db.AllocateAgent(convID, "spawn")
	require.NoError(t, err)
	require.NoError(t, db.SaveSession(&db.SessionRow{ID: "session-" + convID, ConvID: convID, Status: status}))
	return agentID
}

func testAWBReadyGitHub(t *testing.T, state string) {
	t.Helper()
	require.NoError(t, config.Save(&config.Config{Agent: &config.AgentConfig{GitProxy: &config.GitProxyConfig{
		AllowedRemotes: []string{"github.com/acme/repo"},
	}}}))
	t.Cleanup(SetGHTokenCommandForTest(func(context.Context) (string, error) { return "ghp_testtoken", nil }))
	t.Cleanup(SetGitHubTransportForTest(func(_ context.Context, token string, req ghAPIRequest) (ghAPIResult, error) {
		assert.Equal(t, "ghp_testtoken", token)
		assert.Equal(t, "graphql", req.Path)
		body := []byte(`{"data":{"repository":{"pullRequest":{"state":"` + state + `"}}}}`)
		return ghAPIResult{Status: http.StatusOK, Body: body, Header: http.Header{}}, nil
	}))
}

func testAWBReadyMonitorWorker(t *testing.T, serverURL string) awbReadyWorker {
	t.Helper()
	base, fault := validateAWBBaseURL(serverURL)
	require.Nil(t, fault)
	policy := config.AWBProxyConfig{URL: serverURL, Username: "worker", AllowWrite: true, AllowedWorkspaces: []string{"tcl"}}
	return awbReadyWorker{process: "builders", workspace: "tcl",
		config:  config.AWBReadyPollingConfig{Workspace: "tcl", Group: "builders", Cwd: t.TempDir(), MonitorPR: true},
		session: &awbProxySession{policy: policy, base: base, workspaces: []string{"tcl"}}}
}

func TestAWBReadyQueryIncludesWorkspaceLabelsAndLimit(t *testing.T) {
	query := awbReadyQuery("tcl", []string{"backend", "urgent"})
	assert.Equal(t, "tcl", query.Get("workspace"))
	assert.Equal(t, "1", query.Get("limit"))
	assert.Equal(t, []string{"backend", "urgent"}, query["label"])
}

func TestAWBReadySkipEpicsSelectsFirstNonEpic(t *testing.T) {
	t.Setenv("AWB_PASSWORD", "hunter2")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "1", r.URL.Query().Get("limit"))
		assert.Equal(t, []string{"feature", "bug", "task", "chore"}, r.URL.Query()["type"])
		require.NoError(t, json.NewEncoder(w).Encode([]awbIssue{
			{ID: "tcl-task", Workspace: "tcl", Type: "task"},
		}))
	}))
	t.Cleanup(server.Close)
	base, fault := validateAWBBaseURL(server.URL)
	require.Nil(t, fault)
	worker := awbReadyWorker{workspace: "tcl", config: config.AWBReadyPollingConfig{SkipEpics: true},
		session: &awbProxySession{base: base, policy: config.AWBProxyConfig{URL: server.URL, Username: "worker", AllowedWorkspaces: []string{"tcl"}}, workspaces: []string{"tcl"}}}
	issue, err := worker.ready(context.Background())
	require.NoError(t, err)
	require.NotNil(t, issue)
	assert.Equal(t, "tcl-task", issue.ID)
}

func TestValidateAWBReadyPolling(t *testing.T) {
	policy := config.AWBProxyConfig{URL: "https://awb.example", Username: "worker", AllowWrite: true, AllowedWorkspaces: []string{"tcl"}}
	base := config.AWBReadyPollingConfig{Workspace: "tcl", Group: "builders", Cwd: "/repo"}
	d, err := validateAWBReadyPolling(policy, "builders", base)
	assert.NoError(t, err)
	assert.Equal(t, time.Minute, d)
	base.Interval = "15s"
	d, err = validateAWBReadyPolling(policy, "builders", base)
	assert.NoError(t, err)
	assert.Equal(t, 15*time.Second, d)
	base.Interval = ""
	badGroup := base
	badGroup.Group = ""
	_, err = validateAWBReadyPolling(policy, "builders", badGroup)
	assert.ErrorContains(t, err, "group")
	_, err = validateAWBReadyPolling(policy, "Builders", base)
	assert.ErrorContains(t, err, "lowercase")
	bad := base
	bad.Cwd = "relative"
	_, err = validateAWBReadyPolling(policy, "builders", bad)
	assert.ErrorContains(t, err, "absolute")
	bad = base
	bad.Interval = "tomorrow"
	_, err = validateAWBReadyPolling(policy, "builders", bad)
	assert.ErrorContains(t, err, "invalid interval")
	bad = base
	bad.Workspace = "other"
	_, err = validateAWBReadyPolling(policy, "builders", bad)
	assert.ErrorContains(t, err, "allowed_workspaces")
	policy.AllowWrite = false
	_, err = validateAWBReadyPolling(policy, "builders", base)
	assert.ErrorContains(t, err, "allow_write")
	policy.AllowWrite = true
	policy.Username = ""
	_, err = validateAWBReadyPolling(policy, "builders", base)
	assert.ErrorContains(t, err, "username")
	policy.Username = "worker"
	bad = base
	bad.Labels = []string{" bad label "}
	_, err = validateAWBReadyPolling(policy, "builders", bad)
	assert.ErrorContains(t, err, "invalid label")
	bad = base
	bad.MonitorPR = true
	bad.MonitorCommit = true
	_, err = validateAWBReadyPolling(policy, "builders", bad)
	assert.ErrorContains(t, err, "alternatives")
	bad = base
	bad.MonitorClose = true
	bad.MonitorPR = true
	_, err = validateAWBReadyPolling(policy, "builders", bad)
	assert.ErrorContains(t, err, "alternatives")
	bad.MonitorPR = false
	bad.MonitorCommit = true
	_, err = validateAWBReadyPolling(policy, "builders", bad)
	assert.ErrorContains(t, err, "alternatives")
	policy.URL = "file:///tmp/awb"
	_, err = validateAWBReadyPolling(policy, "builders", base)
	assert.ErrorContains(t, err, "invalid url")
}

func TestAWBReadyPickupHeldWhileHarnessIsOverItsUsageCeiling(t *testing.T) {
	setupTestDB(t)
	t.Setenv("AWB_PASSWORD", "hunter2")
	_, err := db.CreateAgentGroup("builders", "")
	require.NoError(t, err)
	writeRateLimitConfig(t, 80, 95)
	now := time.Now()
	reset := now.Add(2 * time.Hour)
	seedCodexUsage(t, harness.CodexUsage{
		Observed: now.Add(-time.Minute),
		FiveHour: &harness.CodexRateLimitWindow{UsedPercent: 93, ResetsAt: reset},
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("AWB was called at %q while the harness was over its usage ceiling", r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	base, fault := validateAWBBaseURL(server.URL)
	require.Nil(t, fault)
	policy := config.AWBProxyConfig{URL: server.URL, Username: "worker", AllowWrite: true, AllowedWorkspaces: []string{"tcl"}}
	worker := awbReadyWorker{
		process:   "builders",
		workspace: "tcl",
		config: config.AWBReadyPollingConfig{Workspace: "tcl", Group: "builders",
			Cwd: t.TempDir(), Harness: config.HarnessList{harness.CodexName}},
		session: &awbProxySession{policy: policy, base: base, workspaces: []string{"tcl"}},
		gate:    &awbReadyGateState{},
	}
	require.NoError(t, worker.tick(context.Background()))
	require.NoError(t, worker.tick(context.Background()))

	dispatch, err := db.GetAWBReadyDispatch("builders")
	require.NoError(t, err)
	assert.Nil(t, dispatch, "nothing may be picked up while the ceiling holds")

	got := logs.String()
	assert.Contains(t, got, `"level":"INFO"`, "an operator scanning the daemon log must see the hold without debug logging")
	assert.Contains(t, got, `"msg":"awb ready polling: holding pickups until the rate limit resets"`)
	assert.Contains(t, got, `"harness":"codex"`)
	assert.Contains(t, got, `"window":"five_hour"`)
	assert.Equal(t, 1, strings.Count(got, "holding pickups until the rate limit resets"),
		"a hold spanning many polls explains itself once, not once per tick")

	rows, err := db.ListAuditLog(db.AuditLogFilter{Verb: "awb.ready.ratelimited"})
	require.NoError(t, err)
	require.Len(t, rows, 1, "the operator can see why the process went quiet")
	assert.Equal(t, http.StatusTooManyRequests, rows[0].Status)
	assert.Contains(t, rows[0].Detail, "harness=codex")
	assert.Contains(t, rows[0].Detail, "window=five_hour")
	assert.Contains(t, rows[0].Detail, "max_pct=80.0")
}

func TestAWBReadyPickupResumesAfterTheRateLimitResets(t *testing.T) {
	setupTestDB(t)
	t.Setenv("AWB_PASSWORD", "hunter2")
	_, err := db.CreateAgentGroup("builders", "")
	require.NoError(t, err)
	writeRateLimitConfig(t, 80, 95)
	now := time.Now()
	// A five-hour window whose reset has already elapsed: the percentage
	// describes a window that has since rolled over, so it cannot hold.
	seedCodexUsage(t, harness.CodexUsage{
		Observed: now.Add(-6 * time.Hour),
		FiveHour: &harness.CodexRateLimitWindow{UsedPercent: 93, ResetsAt: now.Add(-time.Minute)},
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		issue := awbIssue{ID: "tcl-a1", Workspace: "tcl", Status: "open"}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/ready" {
			if err := json.NewEncoder(w).Encode([]awbIssue{issue}); err != nil {
				t.Errorf("encode ready response: %v", err)
			}
			return
		}
		issue.Status = "closed"
		if err := json.NewEncoder(w).Encode(issue); err != nil {
			t.Errorf("encode issue response: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	base, fault := validateAWBBaseURL(server.URL)
	require.Nil(t, fault)
	policy := config.AWBProxyConfig{URL: server.URL, Username: "worker", AllowWrite: true, AllowedWorkspaces: []string{"tcl"}}
	worker := awbReadyWorker{
		process:   "builders",
		workspace: "tcl",
		config: config.AWBReadyPollingConfig{Workspace: "tcl", Group: "builders",
			Cwd: t.TempDir(), Harness: config.HarnessList{harness.CodexName}},
		session: &awbProxySession{policy: policy, base: base, workspaces: []string{"tcl"}},
		gate:    &awbReadyGateState{},
	}
	require.NoError(t, worker.tick(context.Background()))

	rows, err := db.ListAuditLog(db.AuditLogFilter{Verb: "awb.ready"})
	require.NoError(t, err)
	assert.Len(t, rows, 1, "the reset window leaves the process free to ask AWB for work")
}

func TestAWBReadySpawnHarnessCandidatesResolveThroughProfileTiers(t *testing.T) {
	setupTestDB(t)
	_, err := db.CreateAgentGroup("builders", "")
	require.NoError(t, err)
	_, err = db.CreateSpawnProfile(&db.SpawnProfile{Name: "codex-worker", Harness: harness.CodexName})
	require.NoError(t, err)
	_, err = db.CreateSpawnProfile(&db.SpawnProfile{Name: "plain-worker"})
	require.NoError(t, err)

	worker := awbReadyWorker{process: "builders", workspace: "tcl",
		config: config.AWBReadyPollingConfig{Workspace: "tcl", Group: "builders"}}
	assert.Equal(t, []string{harness.DefaultName}, worker.spawnHarnessCandidates(),
		"a process that pins nothing spawns the default harness")

	worker.config.Profile = "codex-worker"
	assert.Equal(t, []string{harness.CodexName}, worker.spawnHarnessCandidates(),
		"a named profile can pin the harness the gate must ask about")

	worker.config.Harness = config.HarnessList{harness.CopilotName}
	assert.Equal(t, []string{harness.CopilotName}, worker.spawnHarnessCandidates(),
		"explicit configuration outranks every profile tier")

	worker.config.Harness = config.HarnessList{harness.CodexName, harness.DefaultName}
	assert.Equal(t, []string{harness.CodexName, harness.DefaultName}, worker.spawnHarnessCandidates(),
		"a configured fallback chain is taken verbatim, in the operator's order")

	worker.config.Harness = nil
	worker.config.Profile = "plain-worker"
	assert.Equal(t, []string{harness.DefaultName}, worker.spawnHarnessCandidates(),
		"the first profile tier that exists answers, even when it pins no harness")

	worker.config.Profile = ""
	_, err = db.SetAgentGroupDefaultProfile("builders", "codex-worker")
	require.NoError(t, err)
	group, err := db.GetAgentGroupByName("builders")
	require.NoError(t, err)
	require.Equal(t, "codex-worker", group.DefaultProfile)
	assert.Equal(t, []string{harness.CodexName}, worker.spawnHarnessCandidates(),
		"a group default profile flips the harness just as handleGroupSpawn's chain does")
}

func TestAWBReadyChooseSpawnHarnessFallsThroughAHeldChain(t *testing.T) {
	setupTestDB(t)
	writeRateLimitConfig(t, 80, 95)
	now := time.Now()
	// Codex is spent for the next two hours; Claude has room.
	seedCodexUsage(t, harness.CodexUsage{
		Observed: now.Add(-time.Minute),
		FiveHour: &harness.CodexRateLimitWindow{UsedPercent: 93, ResetsAt: now.Add(2 * time.Hour)},
	})
	seedClaudeUsage(t, now, usageapi.CachedUsage{
		FiveHour: &usageapi.CachedBucket{Pct: 20, ResetsAt: now.Add(3 * time.Hour)},
	})

	worker := awbReadyWorker{process: "builders", workspace: "tcl",
		config: config.AWBReadyPollingConfig{Workspace: "tcl", Group: "builders",
			Harness: config.HarnessList{harness.CodexName, harness.DefaultName}}}
	chosen, hold := worker.chooseSpawnHarness(now)
	assert.Nil(t, hold, "a chain with one usable harness left must not hold the process")
	assert.Equal(t, harness.DefaultName, chosen, "the first candidate under its ceiling wins")
}

func TestAWBReadyChooseSpawnHarnessHoldsOnTheSoonestResetWhenEveryHarnessIsSpent(t *testing.T) {
	setupTestDB(t)
	writeRateLimitConfig(t, 80, 95)
	now := time.Now()
	codexReset := now.Add(4 * time.Hour)
	claudeReset := now.Add(90 * time.Minute)
	seedCodexUsage(t, harness.CodexUsage{
		Observed: now.Add(-time.Minute),
		FiveHour: &harness.CodexRateLimitWindow{UsedPercent: 93, ResetsAt: codexReset},
	})
	seedClaudeUsage(t, now, usageapi.CachedUsage{
		FiveHour: &usageapi.CachedBucket{Pct: 99, ResetsAt: claudeReset},
	})

	worker := awbReadyWorker{process: "builders", workspace: "tcl",
		config: config.AWBReadyPollingConfig{Workspace: "tcl", Group: "builders",
			Harness: config.HarnessList{harness.CodexName, harness.DefaultName}}}
	chosen, hold := worker.chooseSpawnHarness(now)
	require.NotNil(t, hold, "a chain whose every candidate is spent holds the process")
	assert.Equal(t, harness.DefaultName, hold.Harness,
		"the chain frees up when its soonest-resetting candidate resets")
	assert.WithinDuration(t, claudeReset, hold.ResetsAt, time.Second)
	assert.Equal(t, harness.CodexName, chosen,
		"a caller past the gate still launches the operator's first choice")
}

func TestAWBReadyPickupUsesTheFallbackHarnessWhileTheFirstIsSpent(t *testing.T) {
	setupTestDB(t)
	t.Setenv("AWB_PASSWORD", "hunter2")
	_, err := db.CreateAgentGroup("builders", "")
	require.NoError(t, err)
	writeRateLimitConfig(t, 80, 95)
	now := time.Now()
	seedCodexUsage(t, harness.CodexUsage{
		Observed: now.Add(-time.Minute),
		FiveHour: &harness.CodexRateLimitWindow{UsedPercent: 93, ResetsAt: now.Add(2 * time.Hour)},
	})
	seedClaudeUsage(t, now, usageapi.CachedUsage{
		FiveHour: &usageapi.CachedBucket{Pct: 20, ResetsAt: now.Add(3 * time.Hour)},
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		issue := awbIssue{ID: "tcl-a1", Workspace: "tcl", Status: "open"}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/ready" {
			if err := json.NewEncoder(w).Encode([]awbIssue{issue}); err != nil {
				t.Errorf("encode ready response: %v", err)
			}
			return
		}
		issue.Status = "closed"
		if err := json.NewEncoder(w).Encode(issue); err != nil {
			t.Errorf("encode issue response: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	base, fault := validateAWBBaseURL(server.URL)
	require.Nil(t, fault)
	policy := config.AWBProxyConfig{URL: server.URL, Username: "worker", AllowWrite: true, AllowedWorkspaces: []string{"tcl"}}
	worker := awbReadyWorker{
		process:   "builders",
		workspace: "tcl",
		config: config.AWBReadyPollingConfig{Workspace: "tcl", Group: "builders", Cwd: t.TempDir(),
			Harness: config.HarnessList{harness.CodexName, harness.DefaultName}},
		session: &awbProxySession{policy: policy, base: base, workspaces: []string{"tcl"}},
		gate:    &awbReadyGateState{},
	}
	require.NoError(t, worker.tick(context.Background()))

	rows, err := db.ListAuditLog(db.AuditLogFilter{Verb: "awb.ready"})
	require.NoError(t, err)
	assert.Len(t, rows, 1, "one spent harness in the chain must not stop the process from taking work")
	assert.Contains(t, logs.String(), `"harness":"claude"`,
		"the pickup line names the vendor the fallback landed on")
	held, err := db.ListAuditLog(db.AuditLogFilter{Verb: "awb.ready.ratelimited"})
	require.NoError(t, err)
	assert.Empty(t, held)
}

func TestAWBReadyPickupHeldWhenEveryHarnessInTheChainIsSpent(t *testing.T) {
	setupTestDB(t)
	t.Setenv("AWB_PASSWORD", "hunter2")
	_, err := db.CreateAgentGroup("builders", "")
	require.NoError(t, err)
	writeRateLimitConfig(t, 80, 95)
	now := time.Now()
	seedCodexUsage(t, harness.CodexUsage{
		Observed: now.Add(-time.Minute),
		FiveHour: &harness.CodexRateLimitWindow{UsedPercent: 93, ResetsAt: now.Add(4 * time.Hour)},
	})
	seedClaudeUsage(t, now, usageapi.CachedUsage{
		FiveHour: &usageapi.CachedBucket{Pct: 99, ResetsAt: now.Add(90 * time.Minute)},
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("AWB was called at %q while every harness in the chain was spent", r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	base, fault := validateAWBBaseURL(server.URL)
	require.Nil(t, fault)
	policy := config.AWBProxyConfig{URL: server.URL, Username: "worker", AllowWrite: true, AllowedWorkspaces: []string{"tcl"}}
	worker := awbReadyWorker{
		process:   "builders",
		workspace: "tcl",
		config: config.AWBReadyPollingConfig{Workspace: "tcl", Group: "builders", Cwd: t.TempDir(),
			Harness: config.HarnessList{harness.CodexName, harness.DefaultName}},
		session: &awbProxySession{policy: policy, base: base, workspaces: []string{"tcl"}},
		gate:    &awbReadyGateState{},
	}
	require.NoError(t, worker.tick(context.Background()))

	dispatch, err := db.GetAWBReadyDispatch("builders")
	require.NoError(t, err)
	assert.Nil(t, dispatch)

	got := logs.String()
	assert.Contains(t, got, `"level":"INFO"`)
	assert.Contains(t, got, `"harnesses":"codex,claude"`,
		"the log names the whole chain that was tried, not just the one it reports")
	assert.Contains(t, got, `"harness":"claude"`)

	rows, err := db.ListAuditLog(db.AuditLogFilter{Verb: "awb.ready.ratelimited"})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Contains(t, rows[0].Detail, "harnesses=codex,claude")
}

func TestAWBReadySpawnRequestCarriesTheChosenHarness(t *testing.T) {
	setupTestDB(t)
	worker := awbReadyWorker{
		process:   "builders",
		workspace: "tcl",
		config: config.AWBReadyPollingConfig{Workspace: "tcl", Group: "builders", Cwd: "/repo",
			Harness: config.HarnessList{harness.CodexName, harness.DefaultName}},
		session: &awbProxySession{base: "https://awb.example/"},
	}

	body := worker.spawnRequest("tcl-a1", "/repo", "", "", harness.DefaultName)
	assert.Equal(t, harness.DefaultName, body.Harness,
		"the gate's choice must reach the launch, not the chain's first entry")
	assert.Equal(t, "tcl-a1", body.Name)
	assert.Equal(t, "https://awb.example/#/issues/tcl-a1", body.TaskURL)
}
