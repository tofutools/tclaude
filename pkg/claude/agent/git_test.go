package agent

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// git_test.go covers the CLI half of the Git proxy. The interesting behaviour
// on this side is `pull`, which is deliberately SPLIT across the trust
// boundary: the credentialed fetch goes to the daemon, and the fast-forward
// runs here, in the agent's own process, because it needs no credential and
// updating the work tree runs .gitattributes filter programs the agent already
// controls.

// stubLocalGit swaps the LOCAL half of `pull` and records the commands run.
// failOn, when non-empty, makes any command containing it fail the way git
// does when a fast-forward is not possible.
func stubLocalGit(t *testing.T, answers map[string]string, failOn string) *[][]string {
	t.Helper()
	prev := localGitRun
	t.Cleanup(func() { localGitRun = prev })
	var calls [][]string
	localGitRun = func(args ...string) (string, error) {
		calls = append(calls, args)
		key := strings.Join(args, " ")
		if failOn != "" && strings.Contains(key, failOn) {
			return "fatal: Not possible to fast-forward, aborting.", errors.New("exit status 128")
		}
		return answers[key], nil
	}
	return &calls
}

// gitOutcomeJSON is a proxied-git response body. Written as JSON rather than a
// struct literal so these tests exercise the same decode path the CLI uses in
// production.
const (
	gitPushOKJSON = `{"repo":"repo","remote":"origin","remote_ref":"github.com/tofutools/tclaude",
		"branch":"feat/thing","exit_code":0,
		"stderr":" * [new branch]      feat/thing -> feat/thing"}`
	gitPushRejectedJSON = `{"repo":"repo","remote":"origin","remote_ref":"github.com/tofutools/tclaude",
		"branch":"feat/thing","exit_code":1,
		"stderr":"! [rejected] feat/thing -> feat/thing (non-fast-forward)"}`
	gitPushTimedOutJSON = `{"repo":"repo","remote":"origin","exit_code":-1,"timed_out":true}`
	gitFetchOKJSON      = `{"repo":"repo","remote":"origin","branch":"feat/thing","exit_code":0}`
	gitFetchFailedJSON  = `{"repo":"repo","remote":"origin","branch":"feat/thing","exit_code":128,
		"stderr":"fatal: could not read from remote repository"}`
)

func TestRunGitPush_SendsSemanticRequest(t *testing.T) {
	var calls []capturedReq
	stubDaemon(t, &calls, ok(gitPushOKJSON))

	var stdout, stderr bytes.Buffer
	rc := runGitPush(&gitPushParams{Branch: "feat/thing", SetUpstream: true}, &stdout, &stderr)

	require.Equal(t, rcOK, rc, "stderr=%s", stderr.String())
	require.Len(t, calls, 1)
	assert.Equal(t, "POST", calls[0].method)
	assert.Equal(t, "/v1/git/push", calls[0].path)

	body, okBody := calls[0].body.(map[string]any)
	require.True(t, okBody, "body should be a map, got %T", calls[0].body)
	assert.Equal(t, "feat/thing", body["branch"])
	assert.Equal(t, true, body["set_upstream"])
	assert.Equal(t, false, body["force_with_lease"])
	// The CLI names no repository — the daemon derives it from recorded launch
	// state. A repo key here would mean the aiming primitive had leaked back in.
	assert.NotContains(t, body, "repo")

	assert.Positive(t, calls[0].opts.Timeout,
		"a push needs longer than the default 10s client timeout")
	assert.Contains(t, stderr.String(), "new branch", "git's own summary reaches the agent")
}

// TestRunGitPush_ExitCodeFollowsGit pins the response contract: the daemon
// returning 200 means it RAN git, and a non-zero git exit must still fail the
// command — otherwise an agent would read a rejected push as success.
func TestRunGitPush_ExitCodeFollowsGit(t *testing.T) {
	var calls []capturedReq
	stubDaemon(t, &calls, ok(gitPushRejectedJSON))

	var stdout, stderr bytes.Buffer
	rc := runGitPush(&gitPushParams{Branch: "feat/thing"}, &stdout, &stderr)

	assert.Equal(t, rcIOFailure, rc, "a rejected push must not report success")
	assert.Contains(t, stderr.String(), "non-fast-forward")
}

// TestRunGitPush_TimeoutIsReportedAsUnknown — a timed-out push may or may not
// have landed remotely, and saying so is more useful than a bare failure.
func TestRunGitPush_TimeoutIsReportedAsUnknown(t *testing.T) {
	var calls []capturedReq
	stubDaemon(t, &calls, ok(gitPushTimedOutJSON))

	var stdout, stderr bytes.Buffer
	rc := runGitPush(&gitPushParams{Branch: "feat/thing"}, &stdout, &stderr)

	assert.Equal(t, rcIOFailure, rc)
	assert.Contains(t, stderr.String(), "may or may not")
}

// TestRunGitPush_SurfacesTheDaemonRefusal — a 403 from a gate (protected
// branch, missing slug, off-list remote) must arrive as the daemon's own
// explanation, since that message names the config or grant to change.
func TestRunGitPush_SurfacesTheDaemonRefusal(t *testing.T) {
	var calls []capturedReq
	stubDaemon(t, &calls, func(string, string) (int, string, string) {
		return 403, "protected_ref", ""
	})

	var stdout, stderr bytes.Buffer
	rc := runGitPush(&gitPushParams{Branch: "main"}, &stdout, &stderr)

	assert.NotEqual(t, rcOK, rc)
	assert.Contains(t, stderr.String(), "Error:")
}

// TestRunGitPull_FetchesThroughTheDaemonThenFastForwardsLocally is the split
// this whole verb exists to express.
func TestRunGitPull_FetchesThroughTheDaemonThenFastForwardsLocally(t *testing.T) {
	var calls []capturedReq
	stubDaemon(t, &calls, ok(gitFetchOKJSON))
	local := stubLocalGit(t, map[string]string{
		"rev-parse --abbrev-ref HEAD":                    "feat/thing\n",
		"merge --ff-only refs/remotes/origin/feat/thing": "Fast-forward\n",
	}, "")

	var stdout, stderr bytes.Buffer
	rc := runGitPull(&gitPullParams{}, &stdout, &stderr)

	require.Equal(t, rcOK, rc, "stderr=%s", stderr.String())
	require.Len(t, calls, 1)
	assert.Equal(t, "/v1/git/fetch", calls[0].path, "only the credentialed half crosses the boundary")
	body := calls[0].body.(map[string]any)
	assert.Equal(t, "feat/thing", body["branch"])

	require.Len(t, *local, 2)
	assert.Equal(t, []string{"rev-parse", "--abbrev-ref", "HEAD"}, (*local)[0])
	assert.Equal(t, []string{"merge", "--ff-only", "refs/remotes/origin/feat/thing"}, (*local)[1],
		"the merge runs locally, as the agent, with no credential involved")
	assert.Contains(t, stdout.String(), "Fast-forward")
}

// TestRunGitPull_DivergedBranchIsTheAgentsProblem — the daemon does not merge,
// so a non-fast-forward has to come back as something the agent resolves.
func TestRunGitPull_DivergedBranchIsTheAgentsProblem(t *testing.T) {
	var calls []capturedReq
	stubDaemon(t, &calls, ok(gitFetchOKJSON))
	stubLocalGit(t, map[string]string{
		"rev-parse --abbrev-ref HEAD": "feat/thing\n",
	}, "merge --ff-only")

	var stdout, stderr bytes.Buffer
	rc := runGitPull(&gitPullParams{}, &stdout, &stderr)

	assert.Equal(t, rcIOFailure, rc)
	assert.Contains(t, stderr.String(), "diverged")
	assert.Contains(t, stderr.String(), "does not merge for you")
}

// TestRunGitPull_RefusesDetachedHead — with no branch to name there is nothing
// to fetch or fast-forward, and guessing would be worse than asking.
func TestRunGitPull_RefusesDetachedHead(t *testing.T) {
	var calls []capturedReq
	stubDaemon(t, &calls, ok(gitFetchOKJSON))
	stubLocalGit(t, map[string]string{"rev-parse --abbrev-ref HEAD": "HEAD\n"}, "")

	var stdout, stderr bytes.Buffer
	rc := runGitPull(&gitPullParams{}, &stdout, &stderr)

	assert.Equal(t, rcInvalidArg, rc)
	assert.Contains(t, stderr.String(), "--branch")
	assert.Empty(t, calls, "nothing should reach the daemon")
}

// TestRunGitPull_DoesNotFastForwardAfterAFailedFetch — a fetch that failed
// leaves the remote-tracking ref stale, and fast-forwarding onto it would
// silently move the branch to the wrong commit.
func TestRunGitPull_DoesNotFastForwardAfterAFailedFetch(t *testing.T) {
	var calls []capturedReq
	stubDaemon(t, &calls, ok(gitFetchFailedJSON))
	local := stubLocalGit(t, map[string]string{
		"rev-parse --abbrev-ref HEAD": "feat/thing\n",
	}, "")

	var stdout, stderr bytes.Buffer
	rc := runGitPull(&gitPullParams{}, &stdout, &stderr)

	assert.Equal(t, rcIOFailure, rc)
	require.Len(t, *local, 1, "only the branch probe should have run")
	assert.Equal(t, []string{"rev-parse", "--abbrev-ref", "HEAD"}, (*local)[0])
}

// TestRunGitLsRemote_ValidatesAskHumanBeforeReachingTheDaemon keeps a typo in
// --ask-human from turning into a request with no approval window.
func TestRunGitLsRemote_ValidatesAskHumanBeforeReachingTheDaemon(t *testing.T) {
	var calls []capturedReq
	stubDaemon(t, &calls, ok(`{}`))

	var stdout, stderr bytes.Buffer
	rc := runGitLsRemote(&gitLsRemoteParams{AskHuman: "banana"}, &stdout, &stderr)

	assert.Equal(t, rcInvalidArg, rc)
	assert.Empty(t, calls, "an invalid flag must not produce a request")
}

// TestRunGitRemotes_RendersRefusalReasons — the discovery command's whole job
// is telling the agent what to ask the operator for.
func TestRunGitRemotes_RendersRefusalReasons(t *testing.T) {
	var calls []capturedReq
	stubDaemon(t, &calls, ok(`{
		"repo":"/home/me/repo","branch":"feat/thing",
		"allowed_remotes":["github.com/tofutools"],
		"protected_refs":["main","master"],
		"allow_force_push":false,
		"remotes":[
			{"name":"origin","fetch_url":"git@github.com:tofutools/tclaude.git","allowed":true},
			{"name":"fork","fetch_url":"git@github.com:me/fork.git","allowed":false,
			 "refused_for":"not on the operator's allow-list"}]}`))

	var stdout, stderr bytes.Buffer
	rc := runGitRemotes(&gitRemotesParams{}, &stdout, &stderr)

	require.Equal(t, rcOK, rc, "stderr=%s", stderr.String())
	out := stdout.String()
	assert.Equal(t, "GET", calls[0].method)
	assert.Contains(t, out, "✓ origin")
	assert.Contains(t, out, "✗ fork")
	assert.Contains(t, out, "not on the operator's allow-list")
	assert.Contains(t, out, "main, master", "the agent should see what it may not push to")
	assert.Contains(t, out, "force-push disabled")
}
