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
)

const defaultAWBReadyPollInterval = time.Minute

type awbReadyWorker struct {
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
	for workspace, polling := range policy.ReadyPolling {
		if _, err := validateAWBReadyPolling(policy, workspace, polling); err != nil {
			return fmt.Errorf("agent.awb_proxy.ready_polling[%q]: %w", workspace, err)
		}
	}
	for workspace, polling := range policy.ReadyPolling {
		interval, _ := validateAWBReadyPolling(policy, workspace, polling)
		w := awbReadyWorker{workspace: workspace, config: polling, interval: interval,
			session: &awbProxySession{policy: policy, base: strings.TrimRight(policy.URL, "/"), workspaces: []string{workspace}}}
		goBackground(func() { w.run(stop) })
	}
	return nil
}

func validateAWBReadyPolling(policy config.AWBProxyConfig, workspace string, p config.AWBReadyPollingConfig) (time.Duration, error) {
	if awbWorkspaceKeyShapeErr(workspace) != nil {
		return 0, awbWorkspaceKeyShapeErr(workspace)
	}
	if policy.URL == "" || policy.Username == "" || !policy.AllowWrite {
		return 0, fmt.Errorf("ready polling requires url, username, and allow_write=true")
	}
	if !policy.AWBWorkspaceAllowed(workspace) {
		return 0, fmt.Errorf("workspace is not in allowed_workspaces")
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
			d, _ := db.GetAWBReadyDispatch(w.workspace)
			attrs := []any{"workspace", w.workspace, "error", err}
			if d != nil {
				attrs = append(attrs, "issue", d.IssueID, "phase", d.Phase, "agent_id", d.AgentID)
				_, _ = db.UpdateAWBReadyDispatch(w.workspace, d.IssueID, d.Phase, err.Error())
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
	dispatch, err := db.GetAWBReadyDispatch(w.workspace)
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
		selected, err := db.SelectAWBReadyDispatch(w.workspace, issue.ID, db.NewAgentID())
		if err != nil || !selected {
			return err
		}
		dispatch, err = db.GetAWBReadyDispatch(w.workspace)
		if err != nil {
			return err
		}
	}
	issue, err := w.show(ctx, dispatch.IssueID)
	if err != nil {
		return err
	}
	if dispatch.Phase == "spawned" {
		if issue.Status == "closed" {
			_, err = db.ClearAWBReadyDispatch(w.workspace, dispatch.IssueID)
		}
		return err
	}
	// A crash after the spawn committed but before the phase update is
	// recovered through the reserved stable identity. Never launch a duplicate.
	if existing, getErr := db.GetAgent(dispatch.AgentID); getErr != nil {
		return getErr
	} else if existing != nil && existing.CurrentConvID != "" {
		_, err = db.UpdateAWBReadyDispatch(w.workspace, dispatch.IssueID, "spawned", "")
		return err
	}
	if !containsFold(issue.Assignees, w.session.policy.Username) {
		if issue, err = w.claim(ctx, dispatch.IssueID); err != nil {
			return err
		}
	}
	if _, err = db.UpdateAWBReadyDispatch(w.workspace, dispatch.IssueID, "claimed", ""); err != nil {
		return err
	}
	agentID, err := w.spawn(dispatch.IssueID, dispatch.AgentID)
	if err != nil {
		return err
	}
	if err = db.SetAWBReadyDispatchAgent(w.workspace, dispatch.IssueID, agentID); err != nil {
		return err
	}
	_, err = db.UpdateAWBReadyDispatch(w.workspace, dispatch.IssueID, "spawned", "")
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
	q := url.Values{"workspace": {w.workspace}, "limit": {"1"}}
	_, f := w.session.exec(ctx, awbCall{Method: http.MethodGet, Path: "/api/ready", Query: q}, &issues)
	if f != nil {
		return nil, fmt.Errorf("%s", f.Msg)
	}
	if len(issues) == 0 {
		return nil, nil
	}
	if f := w.session.enforceIssueWorkspace(&issues[0]); f != nil {
		return nil, fmt.Errorf("%s", f.Msg)
	}
	return &issues[0], nil
}
func (w awbReadyWorker) show(ctx context.Context, id string) (*awbIssue, error) {
	var i awbIssue
	_, f := w.session.exec(ctx, awbCall{Method: http.MethodGet, Path: "/api/issues/" + awbSegment(id)}, &i)
	if f != nil {
		return nil, fmt.Errorf("%s", f.Msg)
	}
	if f = w.session.enforceIssueWorkspace(&i); f != nil {
		return nil, fmt.Errorf("%s", f.Msg)
	}
	return &i, nil
}
func (w awbReadyWorker) claim(ctx context.Context, id string) (*awbIssue, error) {
	if f := w.session.requireWrite(); f != nil {
		return nil, fmt.Errorf("%s", f.Msg)
	}
	body, _ := json.Marshal(awbClaimBody{Assignee: w.session.policy.Username, Force: true})
	var i awbIssue
	_, f := w.session.exec(ctx, awbCall{Method: http.MethodPost, Path: "/api/issues/" + awbSegment(id) + "/claim", Body: body, ContentType: "application/json"}, &i)
	if f != nil {
		return nil, fmt.Errorf("%s", f.Msg)
	}
	if f = w.session.enforceIssueWorkspace(&i); f != nil {
		return nil, fmt.Errorf("%s", f.Msg)
	}
	return &i, nil
}

func (w awbReadyWorker) spawn(issueID, reservedAgentID string) (string, error) {
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
	body := agent.SpawnRequest{Name: issueID, Cwd: cwd, WorktreePath: wtPath, WorktreeBranch: wtBranch, Profile: w.config.Profile, SandboxProfile: w.config.SandboxProfile, Harness: w.config.Harness, TaskURL: strings.TrimRight(w.session.base, "/") + "/#/issues/" + issueID, TaskLabel: issueID, InitialMessage: fmt.Sprintf("Fetch %s with `tclaude proxy awb show %s`, work it to completion, record progress through the AWB proxy, and close it when complete.", issueID, issueID), PermissionOverrides: map[string]db.PermissionOverride{PermAWBRead: db.ScopedOverride(db.PermEffectGrant, scope), PermAWBWrite: db.ScopedOverride(db.PermEffectGrant, scope)}}
	g, _ := db.GetAgentGroupByName(w.config.Group)
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
		return "", fmt.Errorf("spawn failed: HTTP %d: %s", rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	var out agent.SpawnResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		return "", err
	}
	return out.AgentID, nil
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
