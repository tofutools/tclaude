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

const awbReadyPRStateQuery = `
query PRState($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) { state }
  }
}`

var (
	awbReadyPRMerged     = liveAWBReadyPRMerged
	awbReadyAgentSettled = liveAWBReadyAgentSettled
)

type awbReadyWorker struct {
	process   string
	workspace string
	config    config.AWBReadyPollingConfig
	interval  time.Duration
	session   *awbProxySession
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
			session: &awbProxySession{policy: policy, base: base, workspaces: []string{polling.Workspace}}}
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
	if dispatch == nil {
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
			"workspace", w.workspace, "issue", dispatch.IssueID, "agent_id", dispatch.AgentID)
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
		if w.config.MonitorPR && issue.PullRequestURL != "" {
			settled, settleErr := awbReadyAgentSettled(dispatch.AgentID)
			if settleErr != nil {
				return settleErr
			}
			if !settled {
				return nil
			}
			merged, reachable, mergeErr := awbReadyPRMerged(ctx, issue.PullRequestURL)
			if reachable {
				status := http.StatusOK
				if mergeErr != nil {
					status = http.StatusBadGateway
				}
				w.audit("github.pr.view", dispatch.IssueID, status)
			}
			if mergeErr != nil {
				return mergeErr
			}
			if !reachable {
				slog.Warn("awb ready polling: pull request is not reachable through GitHub proxy",
					"process", w.process, "workspace", w.workspace, "issue", dispatch.IssueID)
			}
			if merged {
				if closeErr := w.close(ctx, dispatch.IssueID); closeErr != nil {
					return closeErr
				}
				slog.Info("awb ready polling: closed issue for merged pull request", "process", w.process,
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
	agentID, err := w.spawn(dispatch.IssueID, dispatch.AgentID)
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
	if w.config.Harness != "" {
		if _, ok := harness.Get(w.config.Harness); !ok {
			return fmt.Errorf("harness %q does not exist", w.config.Harness)
		}
	}
	if w.config.Worktree {
		if _, _, err := spawnWorktreeRepoRoot(w.config.Cwd); err != nil {
			return fmt.Errorf("cwd %q is not in a git repository: %v", w.config.Cwd, err)
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

func (w awbReadyWorker) close(ctx context.Context, id string) error {
	if f := w.session.requireWrite(); f != nil {
		return fmt.Errorf("%s", f.Msg)
	}
	reason := "GitHub pull request merged and spawned agent settled"
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
	return row != nil && (row.Status == session.StatusIdle || row.Status == session.StatusExited), nil
}

func (w awbReadyWorker) spawn(issueID, reservedAgentID string) (string, error) {
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
	scope := fmt.Sprintf(`{"awb_workspace":[%q]}`, w.workspace)
	body := agent.SpawnRequest{Name: issueID, Cwd: cwd, WorktreePath: wtPath, WorktreeBranch: wtBranch, Profile: w.config.Profile, SandboxProfile: w.config.SandboxProfile, Harness: w.config.Harness, TaskURL: strings.TrimRight(w.session.base, "/") + "/#/issues/" + issueID, TaskLabel: issueID, InitialMessage: awbReadyInitialMessage(issueID, w.config.MonitorPR), PermissionOverrides: map[string]db.PermissionOverride{PermAWBRead: db.ScopedOverride(db.PermEffectGrant, scope), PermAWBWrite: db.ScopedOverride(db.PermEffectGrant, scope)}}
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

func awbReadyInitialMessage(issueID string, monitorPR bool) string {
	message := fmt.Sprintf("Fetch %s with `tclaude proxy awb show %s`, work it to completion, and record progress through the AWB proxy. Leave closing the issue to the operator.", issueID, issueID)
	if monitorPR {
		message += fmt.Sprintf(" When you open a pull request, record it with `tclaude proxy awb update --pull-request-url <url> %s`; the daemon will close the issue after that pull request merges and you become idle or exit.", issueID)
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
	if _, err := db.InsertAuditLog(db.AuditLogEntry{ActorKind: db.AuditActorSystem,
		ActorLabel: "agentd AWB ready poller", Verb: verb, TargetLabel: issue,
		GroupName: w.config.Group, Detail: "workspace=" + w.workspace,
		Method: http.MethodPost, Path: "internal://awb-ready-poller", Status: status,
		Source: db.AuditSourceReconcile}); err != nil {
		slog.Warn("awb ready polling: failed to record audit", "workspace", w.workspace, "verb", verb, "error", err)
	}
}
