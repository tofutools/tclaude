package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

const defaultAWBReadyPollInterval = time.Minute

var liveAWBReadyCommitOnMainFn = liveAWBReadyCommitOnMain

const awbReadyPRStateQuery = `
query PRState($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) { state }
  }
}`

type awbReadyWorker struct {
	process   string
	workspace string
	config    config.AWBReadyPollingConfig
	interval  time.Duration
	session   *awbProxySession
	// gate carries the rate-limit hold this worker has already reported. It
	// is a pointer so the value-receiver tick can update it; a nil gate
	// (tests constructing a worker directly) only loses the log/audit
	// de-duplication, never the hold itself.
	gate *awbReadyGateState
}

// awbReadyGateState remembers the harness and usage window a worker is
// currently held on, so a wait spanning hundreds of polls reports itself once
// instead of once per tick. The harness is part of the key because a fallback
// chain can move its hold from one vendor to another — a change worth one
// fresh log line. The zero value means "not holding".
type awbReadyGateState struct {
	harness  string
	window   string
	resetsAt time.Time
}

type reservedAgentIDContextKey struct{}

func reservedAgentIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(reservedAgentIDContextKey{}).(string)
	return id
}

func startAWBReadyPollers(stop <-chan struct{}, cfg *config.Config) error {
	policy := cfg.ResolvedAWBProxy()
	for process, polling := range policy.ReadyPolling {
		if _, err := validateAWBReadyPolling(policy, process, polling); err != nil {
			return fmt.Errorf("agent.awb_proxy.ready_polling[%q]: %w", process, err)
		}
	}
	for process, polling := range policy.ReadyPolling {
		interval, _ := validateAWBReadyPolling(policy, process, polling)
		base, _ := validateAWBBaseURL(policy.URL)
		w := awbReadyWorker{process: process, workspace: polling.Workspace, config: polling, interval: interval,
			session: &awbProxySession{policy: policy, base: base, workspaces: []string{polling.Workspace}},
			gate:    &awbReadyGateState{}}
		go w.run(stop)
	}
	return nil
}

func validateAWBReadyPolling(policy config.AWBProxyConfig, process string, p config.AWBReadyPollingConfig) (time.Duration, error) {
	if err := awbWorkspaceKeyShapeErr(process); err != nil {
		return 0, fmt.Errorf("invalid process name: %w", err)
	}
	if err := awbWorkspaceKeyShapeErr(p.Workspace); err != nil {
		return 0, fmt.Errorf("invalid workspace: %w", err)
	}
	for _, label := range p.Labels {
		if _, fault := validateAWBLabel(label); fault != nil {
			return 0, fmt.Errorf("invalid label: %s", fault.Msg)
		}
	}
	if !policy.AWBWorkspaceAllowed(p.Workspace) {
		return 0, fmt.Errorf("workspace is not in allowed_workspaces")
	}
	if _, fault := validateAWBBaseURL(policy.URL); fault != nil {
		return 0, fmt.Errorf("invalid url: %s", fault.Msg)
	}
	if policy.Username == "" || !policy.AllowWrite {
		return 0, fmt.Errorf("ready polling requires url, username, and allow_write=true")
	}
	if strings.TrimSpace(p.Group) == "" {
		return 0, fmt.Errorf("group is required")
	}
	if !filepath.IsAbs(strings.TrimSpace(p.Cwd)) {
		return 0, fmt.Errorf("cwd must be absolute")
	}
	if p.MonitorPR && p.MonitorCommit {
		return 0, fmt.Errorf("monitor_pr and monitor_commit are alternatives and cannot both be enabled")
	}
	if p.Interval == "" {
		return defaultAWBReadyPollInterval, nil
	}
	d, err := time.ParseDuration(strings.TrimSpace(p.Interval))
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("invalid interval %q", p.Interval)
	}
	return d, nil
}

func (w awbReadyWorker) run(stop <-chan struct{}) {
	for {
		if err := w.tick(context.Background()); err != nil {
			d, _ := db.GetAWBReadyDispatch(w.process)
			attrs := []any{"process", w.process, "workspace", w.workspace, "error", err}
			if d != nil {
				attrs = append(attrs, "issue", d.IssueID, "phase", d.Phase, "agent_id", d.AgentID)
				_, _ = db.UpdateAWBReadyDispatch(w.process, d.IssueID, d.Phase, err.Error())
			}
			slog.Error("awb ready polling: retry", attrs...)
		}
		t := time.NewTimer(w.interval)
		select {
		case <-stop:
			t.Stop()
			return
		case <-t.C:
		}
	}
}

func (w awbReadyWorker) tick(ctx context.Context) error {
	dispatch, err := db.GetAWBReadyDispatch(w.process)
	if err != nil {
		return err
	}
	if dispatch == nil || dispatch.Phase != "spawned" {
		if err := w.validateRuntime(); err != nil {
			return err
		}
	}
	// The harness this tick would launch, decided once and reused by the
	// spawn below so the gate's verdict and the launch cannot disagree.
	var spawnHarness string
	if dispatch == nil {
		// Nothing is in flight, so this is the point where the process would
		// start burning the subscription on a new issue — and therefore the
		// point the configured usage ceilings govern. A dispatch already
		// selected is deliberately NOT gated: abandoning a claimed issue
		// half-way would leave it assigned to the operator's account with
		// nobody working it.
		chosen, hold := w.chooseSpawnHarness(time.Now())
		if w.reportRateLimitHold(hold) != nil {
			return nil
		}
		spawnHarness = chosen
		issue, err := w.ready(ctx)
		if err != nil || issue == nil {
			return err
		}
		selected, err := db.SelectAWBReadyDispatch(w.process, w.workspace, issue.ID, db.NewAgentID())
		if err != nil || !selected {
			return err
		}
		dispatch, err = db.GetAWBReadyDispatch(w.process)
		if err != nil {
			return err
		}
		if dispatch == nil {
			return nil
		}
		slog.Info("awb ready polling: picked up issue", "process", w.process,
			"workspace", w.workspace, "issue", dispatch.IssueID, "agent_id", dispatch.AgentID,
			"harness", spawnHarness)
	}
	issue, err := w.show(ctx, dispatch.IssueID)
	if err != nil {
		return err
	}
	// Closure is the only condition that releases a workspace, regardless of
	// how far dispatch progressed. In particular, do not claim or spawn an
	// issue that closed after it was selected.
	if issue.Status == "closed" {
		_, err = db.ClearAWBReadyDispatch(w.process, dispatch.IssueID)
		return err
	}
	if dispatch.Phase == "spawned" {
		if (w.config.MonitorPR && issue.PullRequestURL != "") || (w.config.MonitorCommit && issue.CommitHash != "") {
			settled, settleErr := liveAWBReadyAgentSettled(dispatch.AgentID)
			if settleErr != nil {
				return settleErr
			}
			if !settled {
				return nil
			}
			readyToClose := false
			localMain := false
			if w.config.MonitorCommit {
				readyToClose, localMain, err = liveAWBReadyCommitOnMainFn(ctx, w.config.Cwd, issue.CommitHash)
				if err != nil {
					return err
				}
			} else {
				var reachable bool
				readyToClose, reachable, err = liveAWBReadyPRMerged(ctx, issue.PullRequestURL)
				if reachable {
					status := http.StatusOK
					if err != nil {
						status = http.StatusBadGateway
					}
					w.audit("github.pr.view", dispatch.IssueID, status)
				}
				if err != nil {
					return err
				}
				if !reachable {
					slog.Warn("awb ready polling: pull request is not reachable through GitHub proxy",
						"process", w.process, "workspace", w.workspace, "issue", dispatch.IssueID)
				}
			}
			if readyToClose {
				reason := "GitHub pull request merged and spawned agent settled"
				if w.config.MonitorCommit {
					reason = "Recorded commit reached origin/main and spawned agent settled"
					if localMain {
						reason = "Recorded commit reached local main and spawned agent settled"
					}
				}
				if closeErr := w.close(ctx, dispatch.IssueID, reason); closeErr != nil {
					return closeErr
				}
				slog.Info("awb ready polling: closed issue after monitored change reached main", "process", w.process,
					"workspace", w.workspace, "issue", dispatch.IssueID, "agent_id", dispatch.AgentID)
				_, err = db.ClearAWBReadyDispatch(w.process, dispatch.IssueID)
				return err
			}
		}
		return nil
	}
	// A crash after the spawn committed but before the phase update is
	// recovered through the reserved stable identity. Never launch a duplicate.
	if existing, getErr := db.GetAgent(dispatch.AgentID); getErr != nil {
		return getErr
	} else if existing != nil && existing.CurrentConvID != "" {
		_, err = db.UpdateAWBReadyDispatch(w.process, dispatch.IssueID, "spawned", "")
		return err
	}
	if pending, pendingErr := db.GetPendingSpawnByAgentID(dispatch.AgentID); pendingErr != nil {
		return pendingErr
	} else if pending != nil {
		_, err = db.UpdateAWBReadyDispatch(w.process, dispatch.IssueID, "spawned", "")
		return err
	}
	if !containsFold(issue.Assignees, w.session.policy.Username) {
		claimed, claimErr := w.claim(ctx, dispatch.IssueID)
		if claimErr != nil {
			return claimErr
		}
		if !containsFold(claimed.Assignees, w.session.policy.Username) {
			return fmt.Errorf("AWB claim response did not assign issue to configured account")
		}
	}
	if _, err = db.UpdateAWBReadyDispatch(w.process, dispatch.IssueID, "claimed", ""); err != nil {
		return err
	}
	if spawnHarness == "" {
		// A dispatch selected by an earlier tick (or recovered after a crash)
		// reaches the spawn without passing the gate. Choose from the same
		// chain on the freshest reading available, but never hold: this issue
		// is already claimed on the operator's account.
		spawnHarness, _ = w.chooseSpawnHarness(time.Now())
	}
	agentID, err := w.spawn(dispatch.IssueID, dispatch.AgentID, spawnHarness)
	if err != nil {
		return err
	}
	if err = db.SetAWBReadyDispatchAgent(w.process, dispatch.IssueID, agentID); err != nil {
		return err
	}
	_, err = db.UpdateAWBReadyDispatch(w.process, dispatch.IssueID, "spawned", "")
	return err
}

func (w awbReadyWorker) validateRuntime() error {
	if _, fault := validateAWBBaseURL(w.session.policy.URL); fault != nil {
		return fmt.Errorf("%s", fault.Msg)
	}
	g, err := db.GetAgentGroupByName(w.config.Group)
	if err != nil {
		return err
	}
	if g == nil {
		return fmt.Errorf("group %q does not exist", w.config.Group)
	}
	st, err := os.Stat(w.config.Cwd)
	if err != nil || !st.IsDir() {
		return fmt.Errorf("cwd %q is not a directory", w.config.Cwd)
	}
	if w.config.Profile != "" {
		p, err := db.GetSpawnProfile(w.config.Profile)
		if err != nil || p == nil {
			return fmt.Errorf("spawn profile %q does not exist", w.config.Profile)
		}
	}
	if w.config.SandboxProfile != "" {
		p, err := db.GetSandboxProfile(w.config.SandboxProfile)
		if err != nil || p == nil {
			return fmt.Errorf("sandbox profile %q does not exist", w.config.SandboxProfile)
		}
	}
	for _, name := range w.config.Harness {
		if _, ok := harness.Get(name); !ok {
			return fmt.Errorf("harness %q does not exist", name)
		}
	}
	if w.config.Worktree || w.config.MonitorCommit {
		if _, _, err := spawnWorktreeRepoRoot(w.config.Cwd); err != nil {
			return fmt.Errorf("cwd %q is not in a git repository: %v", w.config.Cwd, err)
		}
	}
	if w.config.MonitorCommit {
		checkCtx, cancel := context.WithTimeout(context.Background(), gitProxyNetworkTimeout)
		defer cancel()
		_, remote, err := openAWBReadyCommitRemote(checkCtx, w.config.Cwd)
		if err != nil {
			return err
		}
		if remote.FetchURL == "" {
			slog.Info("awb ready polling: monitoring local main because no origin is configured",
				"process", w.process, "workspace", w.workspace, "cwd", w.config.Cwd)
		}
	}
	return nil
}

func (w awbReadyWorker) ready(ctx context.Context) (*awbIssue, error) {
	var issues []awbIssue
	q := awbReadyQuery(w.workspace, w.config.Labels)
	_, f := w.session.exec(ctx, awbCall{Method: http.MethodGet, Path: "/api/ready", Query: q}, &issues)
	if f != nil {
		w.audit("awb.ready", "", f.Status)
		return nil, fmt.Errorf("%s", f.Msg)
	}
	w.audit("awb.ready", "", http.StatusOK)
	if len(issues) == 0 {
		return nil, nil
	}
	if f := w.session.enforceIssueWorkspace(&issues[0]); f != nil {
		return nil, fmt.Errorf("%s", f.Msg)
	}
	return &issues[0], nil
}

func awbReadyQuery(workspace string, labels []string) url.Values {
	q := url.Values{"workspace": {workspace}, "limit": {"1"}}
	for _, label := range labels {
		q.Add("label", label)
	}
	return q
}
func (w awbReadyWorker) show(ctx context.Context, id string) (*awbIssue, error) {
	var i awbIssue
	_, f := w.session.exec(ctx, awbCall{Method: http.MethodGet, Path: "/api/issues/" + awbSegment(id)}, &i)
	if f != nil {
		w.audit("awb.show", id, f.Status)
		return nil, fmt.Errorf("%s", f.Msg)
	}
	w.audit("awb.show", id, http.StatusOK)
	if f = w.session.enforceIssueWorkspace(&i); f != nil {
		return nil, fmt.Errorf("%s", f.Msg)
	}
	return &i, nil
}
func (w awbReadyWorker) claim(ctx context.Context, id string) (*awbIssue, error) {
	if f := w.session.requireWrite(); f != nil {
		return nil, fmt.Errorf("%s", f.Msg)
	}
	assignee, fault := validateAWBAssignee(w.session.policy.Username)
	if fault != nil {
		return nil, fmt.Errorf("%s", fault.Msg)
	}
	body, _ := json.Marshal(awbClaimBody{Assignee: assignee, Force: true})
	var i awbIssue
	_, f := w.session.exec(ctx, awbCall{Method: http.MethodPost, Path: "/api/issues/" + awbSegment(id) + "/claim", Body: body, ContentType: "application/json"}, &i)
	if f != nil {
		w.audit("awb.claim", id, f.Status)
		return nil, fmt.Errorf("%s", f.Msg)
	}
	w.audit("awb.claim", id, http.StatusOK)
	if f = w.session.enforceIssueWorkspace(&i); f != nil {
		return nil, fmt.Errorf("%s", f.Msg)
	}
	return &i, nil
}

func (w awbReadyWorker) close(ctx context.Context, id, reason string) error {
	if f := w.session.requireWrite(); f != nil {
		return fmt.Errorf("%s", f.Msg)
	}
	body, _ := json.Marshal(awbCloseBody{Reason: &reason})
	var i awbIssue
	_, f := w.session.exec(ctx, awbCall{Method: http.MethodPost, Path: "/api/issues/" + awbSegment(id) + "/close", Body: body, ContentType: "application/json"}, &i)
	if f != nil {
		w.audit("awb.close", id, f.Status)
		return fmt.Errorf("%s", f.Msg)
	}
	w.audit("awb.close", id, http.StatusOK)
	if f = w.session.enforceIssueWorkspace(&i); f != nil {
		return fmt.Errorf("%s", f.Msg)
	}
	if i.Status != "closed" {
		return fmt.Errorf("AWB close response did not close issue")
	}
	return nil
}

func liveAWBReadyCommitOnMain(ctx context.Context, cwd, commit string) (reached, local bool, err error) {
	if _, fault := validateAWBCommitHash(commit); fault != nil {
		return false, false, fmt.Errorf("invalid AWB commit hash: %s", fault.Msg)
	}
	checkCtx, cancel := context.WithTimeout(ctx, gitProxyNetworkTimeout)
	defer cancel()
	s, remote, err := openAWBReadyCommitRemote(checkCtx, cwd)
	if err != nil {
		return false, false, err
	}
	if remote.FetchURL == "" {
		reached, err := awbReadyCommitOnLocalMain(checkCtx, s, commit)
		return reached, true, err
	}
	xfer, fault := newGitProxyXfer(checkCtx, s, xferBorrowObjects)
	if fault != nil {
		return false, false, fmt.Errorf("prepare isolated fetch: %s", fault.Msg)
	}
	defer xfer.cleanup()
	res, err := xfer.git(checkCtx, s, "fetch", gitProxyUploadPack, "--no-recurse-submodules", "--quiet", "--", remote.FetchURL, "main")
	if err != nil || res.ExitCode != 0 {
		return false, false, fmt.Errorf("fetch origin main: %s", proxyResultDetail(res, err))
	}
	verified, err := xfer.git(checkCtx, s, "rev-parse", "--verify", "--quiet", commit+"^{commit}")
	if err != nil {
		return false, false, fmt.Errorf("verify recorded commit: %w", err)
	}
	if verified.ExitCode != 0 || strings.TrimSpace(verified.Stdout) == "" {
		return false, false, nil
	}
	res, err = xfer.git(checkCtx, s, "merge-base", "--is-ancestor", strings.TrimSpace(verified.Stdout), "FETCH_HEAD")
	if err != nil {
		return false, false, fmt.Errorf("check commit on origin/main: %w", err)
	}
	if res.ExitCode == 1 {
		return false, false, nil
	}
	if res.ExitCode != 0 {
		return false, false, fmt.Errorf("check commit on origin/main: %s", proxyResultDetail(res, nil))
	}
	return true, false, nil
}

func awbReadyCommitOnLocalMain(ctx context.Context, s *gitProxySession, commit string) (bool, error) {
	verified, err := s.git(ctx, "rev-parse", "--verify", "--quiet", commit+"^{commit}")
	if err != nil {
		return false, fmt.Errorf("verify recorded commit: %w", err)
	}
	if verified.ExitCode != 0 || strings.TrimSpace(verified.Stdout) == "" {
		return false, nil
	}
	res, err := s.git(ctx, "merge-base", "--is-ancestor", strings.TrimSpace(verified.Stdout), "refs/heads/main")
	if err != nil {
		return false, fmt.Errorf("check commit on local main: %w", err)
	}
	if res.ExitCode == 1 || res.ExitCode == 128 {
		return false, nil
	}
	if res.ExitCode != 0 {
		return false, fmt.Errorf("check commit on local main: %s", proxyResultDetail(res, nil))
	}
	return true, nil
}

func openAWBReadyCommitRemote(ctx context.Context, cwd string) (*gitProxySession, resolvedRemote, error) {
	s, fault := newGitProxySessionBase(ctx, true)
	if fault != nil {
		return nil, resolvedRemote{}, fmt.Errorf("prepare hardened git session: %s", fault.Msg)
	}
	s.repoRoot = cwd
	remote, fault := resolveProxyRemote(ctx, s, "origin")
	if fault != nil {
		if fault.Code == "unknown_remote" {
			res, err := s.git(ctx, "rev-parse", "--is-inside-work-tree")
			if err != nil || res.ExitCode != 0 {
				return nil, resolvedRemote{}, fmt.Errorf("validate git repository: %s", proxyResultDetail(res, err))
			}
			if strings.TrimSpace(res.Stdout) != "true" {
				return nil, resolvedRemote{}, fmt.Errorf("validate git repository: cwd is not a Git work tree")
			}
			return s, resolvedRemote{}, nil
		}
		return nil, resolvedRemote{}, fmt.Errorf("validate origin remote: %s", fault.Msg)
	}
	if len(s.policy.AllowedRemotes) == 0 {
		return nil, resolvedRemote{}, fmt.Errorf("prepare hardened git session: %s", gitProxyDisabledMessage)
	}
	return s, remote, nil
}

func proxyResultDetail(res ProxyResult, err error) string {
	if err != nil {
		return err.Error()
	}
	if res.TimedOut {
		return "git operation timed out"
	}
	if detail := strings.TrimSpace(res.Stderr); detail != "" {
		return detail
	}
	return fmt.Sprintf("git exited with status %d", res.ExitCode)
}

func liveAWBReadyPRMerged(ctx context.Context, rawURL string) (merged, reachable bool, err error) {
	ref, ok := githubPRRefFromURL(rawURL)
	if !ok {
		return false, false, nil
	}
	cfg, err := config.Load()
	if err != nil {
		return false, false, fmt.Errorf("load GitHub proxy policy: %w", err)
	}
	if !cfg.GitProxyEnabled() || !presentedPRRemoteAllowed(ref, cfg.ResolvedGitProxy().AllowedRemotes) {
		return false, false, nil
	}
	policy := cfg.ResolvedGitProxy()
	token, _, fault := githubToken(ctx, policy)
	if fault != nil {
		return false, true, fmt.Errorf("GitHub proxy: %s", fault.Msg)
	}
	owner, repo, _ := strings.Cut(ref.repo, "/")
	g := &ghProxySession{owner: owner, repo: repo, ownerRepo: ref.repo, remoteKey: "github.com/" + ref.repo, token: token}
	pr, failure, err := g.pullRequest(ctx, awbReadyPRStateQuery, ref.number)
	if err != nil {
		return false, true, err
	}
	if failure != nil {
		return false, true, fmt.Errorf("GitHub proxy: %s", strings.TrimSpace(failure.Stderr))
	}
	return strings.EqualFold(pr.State, "merged"), true, nil
}

func liveAWBReadyAgentSettled(agentID string) (bool, error) {
	a, err := db.GetAgent(agentID)
	if err != nil {
		return false, err
	}
	if a == nil {
		return true, nil
	}
	if !a.Active() {
		return true, nil
	}
	if a.CurrentConvID == "" {
		return false, nil
	}
	row, err := db.FindSessionByConvID(a.CurrentConvID)
	if err != nil {
		return false, err
	}
	return row == nil || row.Status == session.StatusIdle || row.Status == session.StatusExited, nil
}

// spawnRequest builds the launch this process would submit for one issue.
// harnessName is the harness the usage gate settled on, which is why it is a
// parameter rather than read back off the configuration: a fallback chain's
// second entry must reach the launch, not the operator's first choice.
func (w awbReadyWorker) spawnRequest(issueID, cwd, wtPath, wtBranch, harnessName string) agent.SpawnRequest {
	scope := fmt.Sprintf(`{"awb_workspace":[%q]}`, w.workspace)
	return agent.SpawnRequest{Name: issueID, Cwd: cwd, WorktreePath: wtPath, WorktreeBranch: wtBranch,
		Profile: w.config.Profile, SandboxProfile: w.config.SandboxProfile, Harness: harnessName,
		TaskURL: strings.TrimRight(w.session.base, "/") + "/#/issues/" + issueID, TaskLabel: issueID,
		InitialMessage: awbReadyInitialMessage(issueID, w.config.MonitorPR, w.config.MonitorCommit),
		PermissionOverrides: map[string]db.PermissionOverride{
			PermAWBRead:  db.ScopedOverride(db.PermEffectGrant, scope),
			PermAWBWrite: db.ScopedOverride(db.PermEffectGrant, scope)}}
}

func (w awbReadyWorker) spawn(issueID, reservedAgentID, harnessName string) (string, error) {
	g, err := db.GetAgentGroupByName(w.config.Group)
	if err != nil {
		return "", fmt.Errorf("load group %q: %w", w.config.Group, err)
	}
	if g == nil {
		return "", fmt.Errorf("group %q no longer exists", w.config.Group)
	}
	cwd, wtPath, wtBranch, discard := w.config.Cwd, "", "", ""
	if w.config.Worktree {
		body := agent.WorktreePrepareRequest{Repo: w.config.Cwd, Group: w.config.Group, Branch: issueID}
		var out agent.WorktreePrepareResponse
		if err := internalHumanJSON(handleWorktreePrepare, "/v1/worktrees/prepare", body, &out); err != nil {
			return "", err
		}
		cwd, wtPath, wtBranch, discard = out.Path, out.Path, issueID, out.DiscardToken
	}
	body := w.spawnRequest(issueID, cwd, wtPath, wtBranch, harnessName)
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/groups/"+url.PathEscape(w.config.Group)+"/spawn", bytes.NewReader(raw))
	req.SetPathValue("name", w.config.Group)
	req = req.WithContext(context.WithValue(req.Context(), peerKey{}, &peer{PID: os.Getpid(), HumanTokenValid: true}))
	req = req.WithContext(context.WithValue(req.Context(), reservedAgentIDContextKey{}, reservedAgentID))
	rec := httptest.NewRecorder()
	handleGroupSpawn(rec, req, g)
	if rec.Code < 200 || rec.Code > 299 {
		if discard != "" {
			_ = internalHumanJSON(handleWorktreeDiscard, "/v1/worktrees/discard", agent.WorktreeDiscardRequest{Token: discard}, nil)
		}
		w.audit("spawn", issueID, rec.Code)
		return "", fmt.Errorf("spawn failed: HTTP %d: %s", rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	w.audit("spawn", issueID, rec.Code)
	var out agent.SpawnResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		return "", err
	}
	if strings.TrimSpace(out.AgentID) == "" {
		return "", fmt.Errorf("spawn response contained no agent id")
	}
	return out.AgentID, nil
}

func awbReadyInitialMessage(issueID string, monitorPR, monitorCommit bool) string {
	message := fmt.Sprintf("Fetch %s with `tclaude proxy awb show %s`, work it to completion, and record progress through the AWB proxy. Leave closing the issue to the operator.", issueID, issueID)
	if monitorPR {
		message += fmt.Sprintf(" When you open a pull request, record it with `tclaude proxy awb update --pull-request-url <url> %s`; the daemon will close the issue after that pull request merges and you become idle or exit.", issueID)
	}
	if monitorCommit {
		message += fmt.Sprintf(" When your change is on main, record its commit with `tclaude proxy awb update --commit-hash <hash> %s`; the daemon will close the issue after that commit reaches the monitored main branch and you become idle or exit.", issueID)
	}
	return message
}

func internalHumanJSON(handler http.HandlerFunc, path string, in, out any) error {
	raw, _ := json.Marshal(in)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req = req.WithContext(context.WithValue(req.Context(), peerKey{}, &peer{PID: os.Getpid(), HumanTokenValid: true}))
	rec := httptest.NewRecorder()
	handler(rec, req)
	if rec.Code < 200 || rec.Code > 299 {
		return fmt.Errorf("HTTP %d: %s", rec.Code, rec.Body.String())
	}
	if out != nil {
		return json.Unmarshal(rec.Body.Bytes(), out)
	}
	return nil
}
func containsFold(values []string, want string) bool {
	for _, v := range values {
		if strings.EqualFold(v, want) {
			return true
		}
	}
	return false
}

func (w awbReadyWorker) audit(verb, issue string, status int) {
	w.auditDetail(verb, issue, status, "")
}

// auditDetail is audit with extra key=value context appended to the recorded
// detail, for a row whose workspace alone would not explain it.
func (w awbReadyWorker) auditDetail(verb, issue string, status int, extra string) {
	detail := "workspace=" + w.workspace
	if extra != "" {
		detail += " " + extra
	}
	if _, err := db.InsertAuditLog(db.AuditLogEntry{ActorKind: db.AuditActorSystem,
		ActorLabel: "agentd AWB ready poller", Verb: verb, TargetLabel: issue,
		GroupName: w.config.Group, Detail: detail,
		Method: http.MethodPost, Path: "internal://awb-ready-poller", Status: status,
		Source: db.AuditSourceReconcile}); err != nil {
		slog.Warn("awb ready polling: failed to record audit", "workspace", w.workspace, "verb", verb, "error", err)
	}
}

// chooseSpawnHarness picks which harness this process should spawn next, and
// reports the hold when none of its candidates may be used.
//
// A process may configure `harness` as an ordered fallback chain rather than a
// single vendor. The first candidate still under the operator's ceilings wins,
// so a process keeps working on a second vendor while the first one's window
// recovers, and only a chain where EVERY candidate is over its ceiling holds
// the process back.
//
// The hold returned in that case is the candidate resetting SOONEST — the
// moment the chain frees up again. (Within one harness the rule is the
// opposite: harnessRateLimitHold reports its LATEST exceeded window, because
// that harness is unusable until all of them have reset.)
//
// The name returned alongside a hold is the first candidate: a caller that is
// past the gate — a dispatch already claimed, which is never abandoned to a
// ceiling — still needs a harness to launch, and the operator's first choice
// is the honest answer.
func (w awbReadyWorker) chooseSpawnHarness(now time.Time) (string, *rateLimitHold) {
	candidates := w.spawnHarnessCandidates()
	policy := loadRateLimitPolicy()
	if policy == nil {
		return candidates[0], nil
	}
	var soonest *rateLimitHold
	for _, name := range candidates {
		hold := harnessRateLimitHold(policy, name, now)
		if hold == nil {
			return name, nil
		}
		if soonest == nil || hold.ResetsAt.Before(soonest.ResetsAt) {
			soonest = hold
		}
	}
	return candidates[0], soonest
}

// reportRateLimitHold logs and audits a hold once per distinct harness/window/
// reset triple, so a wait spanning hundreds of polls leaves one explanation in
// the log and one row in the audit trail instead of one per tick — and an
// operator can see why a process went quiet without correlating it against a
// usage graph. It returns the hold unchanged so callers can gate on it.
//
// The wait itself is served by the ordinary poll interval rather than by
// sleeping the worker: every tick re-reads the cached usage, so the process
// resumes within one interval of the window resetting, and an operator who
// edits a ceiling or whose limit lifts early is picked up just as quickly.
// Each check is local, so polling through a five-hour hold costs less than the
// AWB call it replaces.
func (w awbReadyWorker) reportRateLimitHold(hold *rateLimitHold) *rateLimitHold {
	if hold == nil {
		if w.gate != nil && w.gate.window != "" {
			slog.Info("awb ready polling: usage back under the configured limit, resuming pickups",
				"process", w.process, "workspace", w.workspace, "harnesses", w.harnessChain())
			*w.gate = awbReadyGateState{}
		}
		return nil
	}
	if w.gate == nil || w.gate.harness != hold.Harness ||
		w.gate.window != hold.Window || !w.gate.resetsAt.Equal(hold.ResetsAt) {
		slog.Info("awb ready polling: holding pickups until the rate limit resets",
			append([]any{"process", w.process, "workspace", w.workspace,
				"harnesses", w.harnessChain()}, hold.LogAttrs()...)...)
		w.auditDetail("awb.ready.ratelimited", "", http.StatusTooManyRequests,
			fmt.Sprintf("harnesses=%s harness=%s window=%s pct=%.1f max_pct=%.1f resets_at=%s",
				w.harnessChain(), hold.Harness, hold.Window, hold.Pct, hold.Threshold,
				hold.ResetsAt.UTC().Format(time.RFC3339)))
		if w.gate != nil {
			*w.gate = awbReadyGateState{harness: hold.Harness, window: hold.Window, resetsAt: hold.ResetsAt}
		}
	}
	return hold
}

// harnessChain renders the candidate chain for one log line or audit detail.
// Resolved on demand rather than carried into every tick: only a hold, or the
// line that lifts one, ever needs to name it.
func (w awbReadyWorker) harnessChain() string {
	return strings.Join(w.spawnHarnessCandidates(), ",")
}

// spawnHarnessCandidates returns the ordered harnesses this process may spawn,
// always at least one entry.
//
// A configured `harness` — one name or a fallback list — is taken verbatim.
// Otherwise the single answer is resolved exactly as handleGroupSpawn's
// independent harness chain resolves it: the named spawn profile, then the
// group default profile, then the global default profile, then Claude Code.
// The usage gate has to ask the harness that will actually spend the
// subscription, and a process that omits `harness` can still be pinned to
// Codex by any of those profile tiers.
func (w awbReadyWorker) spawnHarnessCandidates() []string {
	if len(w.config.Harness) > 0 {
		return append([]string(nil), w.config.Harness...)
	}
	var named *db.SpawnProfile
	if name := strings.TrimSpace(w.config.Profile); name != "" {
		prof, err := db.ResolveSpawnProfile(name)
		if err != nil {
			slog.Warn("awb ready polling: failed to load spawn profile for the usage gate",
				"process", w.process, "profile", name, "error", err)
		}
		named = prof
	}
	g, err := db.GetAgentGroupByName(w.config.Group)
	if err != nil {
		slog.Warn("awb ready polling: failed to load group for the usage gate",
			"process", w.process, "group", w.config.Group, "error", err)
	}
	for _, prof := range []*db.SpawnProfile{named, groupDefaultProfile(g), globalDefaultProfile()} {
		if prof != nil {
			return []string{harnessOrDefault(prof.Harness)}
		}
	}
	return []string{harness.DefaultName}
}
