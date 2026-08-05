package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/common/executil"
)

const (
	maxSeanceRequestBytes    = 4 << 10
	maxSeanceRunRequestBytes = 64 << 10
	maxSeanceTargetBytes     = 256
	maxSeanceQuestionBytes   = 32 << 10
	maxSeanceModelBytes      = 256
	maxSeanceEffortBytes     = 64
	maxSeanceAnswerBytes     = 256 << 10
	maxSeanceStderrBytes     = 64 << 10
	maxSeanceBack            = 128
	defaultSeanceTimeout     = 5 * time.Minute
	maxSeanceTimeout         = 10 * time.Minute
	seanceConcurrency        = 2
)

var seanceSlots = make(chan struct{}, seanceConcurrency)

// seanceResolveReq is the non-billable planning request from the sandboxed
// agent CLI. Agent selectors identify an actor and walk back from its live
// head; exact conv-id selectors identify one historical generation.
type seanceResolveReq struct {
	Target string `json:"target"`
	Back   int    `json:"back"`
}

type seanceResolveResp struct {
	Predecessor     string   `json:"predecessor"`
	Harness         string   `json:"harness"`
	Cwd             string   `json:"cwd"`
	Hops            int      `json:"hops"`
	Requested       int      `json:"requested_back"`
	Exact           bool     `json:"exact"`
	Sandbox         string   `json:"sandbox"`
	Approval        string   `json:"approval"`
	AutoReview      bool     `json:"auto_review"`
	SandboxDenyDirs []string `json:"sandbox_deny_dirs,omitempty"`

	// launchPosture and effectiveSandbox never cross the API boundary. They are
	// the daemon-private, DB-backed launch contract consumed by /run.
	launchPosture    harness.SpawnSpec
	effectiveSandbox *sandboxpolicy.Snapshot
}

type seanceRunReq struct {
	Target    string `json:"target"`
	Back      int    `json:"back"`
	Question  string `json:"question"`
	Model     string `json:"model"`
	Effort    string `json:"effort"`
	TimeoutMS int64  `json:"timeout_ms"`
}

type seanceRunResp struct {
	Answer      string `json:"answer"`
	Predecessor string `json:"predecessor"`
	Harness     string `json:"harness"`
	Truncated   bool   `json:"truncated,omitempty"`
}

// SeanceExecPlan is the single external-process boundary for a daemon-owned
// séance turn. It is exported so production-path flow tests can replace only
// the harness subprocess while exercising the real authenticated HTTP mux.
type SeanceExecPlan struct {
	Argv        []string
	Cwd         string
	Environment map[string]string
}

// SeanceExecResult separates initialization failure (Started=false) from a
// harness that started and exited unsuccessfully. Stdout and stderr are always
// bounded by the live runner before they reach this result.
type SeanceExecResult struct {
	Stdout          string
	Stderr          string
	Started         bool
	StdoutTruncated bool
	Err             error
}

// RunSeanceHarness is the swappable external subprocess boundary. Production
// uses the real harness binary; flow tests replace it with a behavior-accurate
// fake and restore it with t.Cleanup. Such tests must not run in parallel.
var RunSeanceHarness = liveRunSeanceHarness

// EnsureSeanceCodexProfile is the profile-materialization half of the external
// Codex launch boundary. Production shares session-new's exact renderer; flow
// tests replace it so they never touch the operator's real CODEX_HOME.
var EnsureSeanceCodexProfile = session.EnsureCodexManagedOneShotProfile

// RevalidateSeanceCodexCapability is split separately so flow tests can prove
// the verified executable is carried into argv without depending on a real
// platform sandbox probe.
var RevalidateSeanceCodexCapability = harness.RevalidateCodexHomeSplitPolicyCapability

// handleWhoamiSeance resolves the private-state half of a séance plan.
//
// Planning is deliberately free: the actual harness subprocess is only run by
// POST /v1/whoami/seance/run after the caller has had a chance to inspect the
// exact generation/cwd/argv with --print-cmd.
//
// Agents may consult only generations of their own stable actor. The human
// operator may target any actor/generation. Cross-agent séance can grow an
// explicit permission-gated endpoint later without silently granting that
// capability as part of this self-scoped repair.
func handleWhoamiSeance(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}

	caller, isHuman, ok := authedCaller(w, r)
	if !ok {
		return
	}
	var req seanceResolveReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSeanceRequestBytes)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid JSON body")
		return
	}
	resolved, ok := resolveSeancePlan(w, req, caller, isHuman)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, resolved)
}

// handleWhoamiSeanceRun executes one billable, non-interactive harness turn
// outside the managed caller's filesystem sandbox, replaying the predecessor's
// recorded launch posture. The daemon owns this narrow capability because
// Codex and Claude must write their own harness state to initialize; granting
// an agent write access to the whole harness home would expose unrelated
// conversations and credentials.
func handleWhoamiSeanceRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	caller, isHuman, ok := authedCaller(w, r)
	if !ok {
		return
	}
	var req seanceRunReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSeanceRunRequestBytes)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid JSON body")
		return
	}
	req.Question = strings.TrimSpace(req.Question)
	if req.Question == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "a séance question is required")
		return
	}
	if len(req.Question) > maxSeanceQuestionBytes {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			fmt.Sprintf("question is too long (maximum %d bytes)", maxSeanceQuestionBytes))
		return
	}
	if len(req.Model) > maxSeanceModelBytes || len(req.Effort) > maxSeanceEffortBytes {
		writeError(w, http.StatusBadRequest, "invalid_arg", "model or effort value is too long")
		return
	}

	timeout := defaultSeanceTimeout
	if req.TimeoutMS != 0 {
		if req.TimeoutMS < 0 || req.TimeoutMS > maxSeanceTimeout.Milliseconds() {
			writeError(w, http.StatusBadRequest, "invalid_arg",
				fmt.Sprintf("timeout must be greater than zero and no more than %s", maxSeanceTimeout))
			return
		}
		timeout = time.Duration(req.TimeoutMS) * time.Millisecond
	}
	if timeout <= 0 || timeout > maxSeanceTimeout {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			fmt.Sprintf("timeout must be greater than zero and no more than %s", maxSeanceTimeout))
		return
	}

	resolved, ok := resolveSeancePlan(w, seanceResolveReq{
		Target: req.Target,
		Back:   req.Back,
	}, caller, isHuman)
	if !ok {
		return
	}
	h, err := harness.Resolve(resolved.Harness)
	if err != nil || !h.SupportsAsk() {
		// resolveSeancePlan already checked this; keep the execution boundary
		// fail-closed if a future refactor ever changes that invariant.
		writeError(w, http.StatusConflict, "unsupported_harness",
			"the resolved harness cannot execute a séance")
		return
	}
	if !h.CanReplayOneShotLaunchPosture() {
		detail := fmt.Sprintf("harness %q cannot yet reproduce a predecessor's launch posture in a one-shot headless resume", h.Name)
		if h.ServerAuthoritative {
			detail = fmt.Sprintf("harness %q cannot yet reproduce a predecessor's managed-server permission posture in a one-shot headless resume", h.Name)
		}
		writeError(w, http.StatusConflict, "unsupported_harness",
			detail)
		return
	}
	model, err := h.Models.ValidateModel(req.Model)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid --model: "+err.Error())
		return
	}
	effort, err := h.Models.ValidateEffort(req.Effort)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid --effort: "+err.Error())
		return
	}
	select {
	case seanceSlots <- struct{}{}:
		defer func() { <-seanceSlots }()
	default:
		writeError(w, http.StatusTooManyRequests, "seance_busy",
			"the daemon is already holding the maximum number of concurrent séances; try again shortly")
		return
	}

	posture := resolved.launchPosture
	profilePath := ""
	var splitCapability *harness.CodexSplitPolicyCapability
	if h.UsesCodexOneShotReplay() {
		guardMode := posture.HarnessBuiltinMode
		if guardMode == harness.SandboxManagedProfile {
			guardMode = harness.SandboxWorkspaceWrite
		}
		if guardMode == harness.SandboxWorkspaceWrite {
			home, homeErr := os.UserHomeDir()
			if homeErr != nil {
				writeError(w, http.StatusBadGateway, "seance_init",
					"resolve home for the recorded sandbox: "+homeErr.Error())
				return
			}
			if harness.CodexSandboxCwdConflict(guardMode, resolved.Cwd, home) {
				writeError(w, http.StatusConflict, "sandbox_cwd_conflict",
					fmt.Sprintf("cannot reproduce the predecessor's sandbox in %q: "+
						"the workspace would make private harness state writable", resolved.Cwd))
				return
			}
		}
	}
	if h.NeedsManagedProfileForOneShot(posture.HarnessBuiltinMode) {
		profileName, path, capability, profileErr := EnsureSeanceCodexProfile(
			resolved.Cwd, session.GenerateSessionID(), resolved.effectiveSandbox)
		if profileErr != nil {
			writeError(w, http.StatusBadGateway, "seance_init",
				"the harness sandbox could not initialize: "+profileErr.Error())
			return
		}
		posture.HarnessBuiltinMode = ""
		posture.PermissionProfile = profileName
		profilePath = path
		splitCapability = capability
		defer func() { _ = os.Remove(profilePath) }()
	}
	if splitCapability != nil {
		if err := RevalidateSeanceCodexCapability(*splitCapability); err != nil {
			writeError(w, http.StatusBadGateway, "seance_init",
				"the harness sandbox could not initialize: "+err.Error())
			return
		}
	}
	argv := h.Ask.BuildAskArgv(harness.AskSpec{
		ResumeID:      resolved.Predecessor,
		Prompt:        req.Question,
		Print:         true,
		Ephemeral:     true,
		LaunchPosture: &posture,
		Model:         model,
		Effort:        effort,
	})
	if len(argv) == 0 {
		writeError(w, http.StatusConflict, "unsupported_harness", "the resolved harness returned an empty command")
		return
	}
	if splitCapability != nil {
		argv[0] = splitCapability.ExecutablePath
	}

	setAuditTargetConv(r, resolved.Predecessor)
	setAuditTargetLabel(r, short8(resolved.Predecessor))
	setAuditDetail(r, fmt.Sprintf("predecessor %s; harness %s", short8(resolved.Predecessor), h.Name))
	runCtx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	// Claim the directory before the subprocess starts, above the swappable
	// exec seam so the production path always registers. The séance runs in a
	// PREDECESSOR generation's cwd — an offline superseded conversation that
	// captureAgentWorktreeClaims deliberately stops counting as a claimant — so
	// without this a live harness sits for minutes in a directory the cleanup
	// snapshot affirmatively reports as unclaimed (TCL-1026).
	releaseSeanceWorktree := holdSeanceWorktree(resolved.Cwd)
	defer releaseSeanceWorktree()
	result := RunSeanceHarness(runCtx, SeanceExecPlan{
		Argv:        argv,
		Cwd:         resolved.Cwd,
		Environment: posture.ShellEnvironment,
	})
	if result.StdoutTruncated {
		writeError(w, http.StatusBadGateway, "seance_output_limit",
			fmt.Sprintf("the séance answer exceeded the %d-byte output limit", maxSeanceAnswerBytes))
		return
	}
	if result.Err != nil {
		switch {
		case errors.Is(runCtx.Err(), context.DeadlineExceeded):
			writeError(w, http.StatusGatewayTimeout, "seance_timeout",
				fmt.Sprintf("the séance timed out after %s", timeout))
		case errors.Is(runCtx.Err(), context.Canceled):
			writeError(w, 499, "seance_canceled", "the séance was canceled")
		case !result.Started:
			writeError(w, http.StatusBadGateway, "seance_init",
				"the harness could not initialize: "+result.Err.Error())
		default:
			detail := strings.TrimSpace(result.Stderr)
			if detail == "" {
				detail = result.Err.Error()
			}
			writeError(w, http.StatusBadGateway, "seance_failed", "the séance failed: "+detail)
		}
		return
	}

	writeJSON(w, http.StatusOK, seanceRunResp{
		Answer:      result.Stdout,
		Predecessor: resolved.Predecessor,
		Harness:     h.Name,
	})
}

type seanceRecordedLaunch struct {
	cwd        string
	harness    string
	sandbox    string
	approval   string
	autoReview bool
	row        *db.SessionRow
}

// recordedSeanceLaunchForConv reconstructs the exact historical generation's
// containment posture. Conversation-owned resume facts and that generation's
// session row win; the stable agent's current relaunch profile deliberately
// does not, because it may now describe a later successor.
func recordedSeanceLaunchForConv(convID string) (seanceRecordedLaunch, error) {
	row, err := db.FindSessionByConvID(convID)
	if err != nil {
		return seanceRecordedLaunch{}, fmt.Errorf("load predecessor launch record: %w", err)
	}
	conversation, err := db.ConversationResumeProfileForConv(convID)
	if err != nil {
		return seanceRecordedLaunch{}, fmt.Errorf("load conversation resume profile: %w", err)
	}
	if conversation == nil {
		if err := db.BackfillDurableRelaunchProfilesFromLatestSession(convID); err != nil {
			return seanceRecordedLaunch{}, fmt.Errorf("backfill conversation resume profile: %w", err)
		}
		conversation, err = db.ConversationResumeProfileForConv(convID)
		if err != nil {
			return seanceRecordedLaunch{}, fmt.Errorf("reload conversation resume profile: %w", err)
		}
	}

	var fallback *db.AgentRelaunchProfile
	out := seanceRecordedLaunch{row: row}
	if conversation != nil {
		out.cwd = strings.TrimSpace(conversation.Cwd)
		out.harness = strings.TrimSpace(conversation.Harness)
		fallback = conversation.FallbackRelaunch
	}
	if row != nil {
		if out.cwd == "" {
			out.cwd = strings.TrimSpace(row.Cwd)
		}
		if strings.TrimSpace(row.Harness) != "" {
			out.harness = strings.TrimSpace(row.Harness)
		}
	}
	if out.harness == "" {
		return seanceRecordedLaunch{}, fmt.Errorf("conversation harness is missing")
	}
	h, err := harness.Resolve(out.harness)
	if err != nil {
		return seanceRecordedLaunch{}, err
	}

	recordedSandbox := ""
	if fallback != nil && fallback.HarnessBuiltinMode != nil {
		recordedSandbox = *fallback.HarnessBuiltinMode
	}
	if row != nil && strings.TrimSpace(row.HarnessBuiltinMode) != "" {
		recordedSandbox = row.HarnessBuiltinMode
	}
	out.sandbox, err = relaunchSandboxForSession(&db.SessionRow{
		Harness: out.harness, HarnessBuiltinMode: recordedSandbox,
	})
	if err != nil {
		return seanceRecordedLaunch{}, err
	}

	recordedApproval := ""
	autoReview := false
	if fallback != nil {
		if fallback.ApprovalPolicy != nil {
			recordedApproval = *fallback.ApprovalPolicy
		}
		if fallback.ApprovalAutoReview != nil {
			autoReview = *fallback.ApprovalAutoReview
		}
	}
	if row != nil && strings.TrimSpace(row.ApprovalPolicy) != "" {
		recordedApproval = row.ApprovalPolicy
		autoReview = row.ApprovalAutoReview
	}
	out.approval, err = reconstructApproval(h.Name, recordedApproval)
	if err != nil {
		return seanceRecordedLaunch{}, fmt.Errorf("invalid recorded approval policy: %w", err)
	}
	out.autoReview, err = harness.ResolveAutoReview(h, autoReview)
	if err != nil {
		return seanceRecordedLaunch{}, fmt.Errorf("invalid recorded auto-review posture: %w", err)
	}
	return out, nil
}

func resolveSeancePlan(
	w http.ResponseWriter,
	req seanceResolveReq,
	caller string,
	isHuman bool,
) (seanceResolveResp, bool) {
	req.Target = strings.TrimSpace(req.Target)
	if len(req.Target) > maxSeanceTargetBytes {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			fmt.Sprintf("--target is too long (maximum %d bytes)", maxSeanceTargetBytes))
		return seanceResolveResp{}, false
	}
	if req.Back < 1 {
		req.Back = 1
	}
	if req.Back > maxSeanceBack {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			fmt.Sprintf("--back must be between 1 and %d", maxSeanceBack))
		return seanceResolveResp{}, false
	}

	target, hops, exact, ok := resolveSeanceGeneration(w, req, caller, isHuman)
	if !ok {
		return seanceResolveResp{}, false
	}
	if !isHuman && !sameActor(caller, target) {
		writeError(w, http.StatusForbidden, "permission",
			"an agent may hold a séance only with one of its own predecessor generations")
		return seanceResolveResp{}, false
	}

	recorded, err := recordedSeanceLaunchForConv(target)
	if err != nil {
		writeError(w, http.StatusConflict, "resume_profile",
			"cannot load the predecessor's recorded launch posture: "+err.Error())
		return seanceResolveResp{}, false
	}
	sourceRow := recorded.row
	cwd := recorded.cwd
	if cwd == "" {
		writeError(w, http.StatusNotFound, "not_found",
			fmt.Sprintf("cannot locate predecessor %s's working directory; its grave is unreachable", short8(target)))
		return seanceResolveResp{}, false
	}
	info, err := os.Stat(cwd)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusNotFound, "not_found",
				fmt.Sprintf("predecessor %s's working directory no longer exists; its grave is unreachable: %s",
					short8(target), cwd))
			return seanceResolveResp{}, false
		}
		writeError(w, http.StatusConflict, "unreachable_grave",
			fmt.Sprintf("cannot access predecessor %s's working directory; its grave is unreachable: %v",
				short8(target), err))
		return seanceResolveResp{}, false
	}
	if !info.IsDir() {
		writeError(w, http.StatusConflict, "unreachable_grave",
			fmt.Sprintf("predecessor %s's recorded working directory is not a directory; its grave is unreachable: %s",
				short8(target), cwd))
		return seanceResolveResp{}, false
	}

	h, err := harness.Resolve(recorded.harness)
	if err != nil {
		writeError(w, http.StatusConflict, "unsupported_harness", err.Error())
		return seanceResolveResp{}, false
	}
	if !h.SupportsAsk() {
		writeError(w, http.StatusConflict, "unsupported_harness",
			fmt.Sprintf("harness %q cannot hold a séance (no headless resume/ask support)", h.Name))
		return seanceResolveResp{}, false
	}

	harnessBuiltinMode := recorded.sandbox
	approvalPolicy := recorded.approval
	autoReview := recorded.autoReview

	// The session row is the exact generation's immutable launch snapshot. It
	// is preferable to the stable actor copy here because a séance addresses a
	// historical generation, not whichever successor currently owns the actor.
	// A legacy/pruned row must fail closed: the stable actor copy belongs to the
	// successor and may have changed since this generation launched.
	var effectiveSandbox *sandboxpolicy.Snapshot
	if sourceRow == nil || sourceRow.EffectiveSandbox == nil {
		writeError(w, http.StatusConflict, "resume_profile",
			"the predecessor's historical sandbox snapshot is unavailable; refusing to substitute the successor's current authority")
		return seanceResolveResp{}, false
	}
	effectiveSandbox = sourceRow.EffectiveSandbox
	if effectiveSandbox != nil {
		validated, validateErr := sandboxpolicy.RevalidateSnapshot(*effectiveSandbox)
		if validateErr != nil {
			writeError(w, http.StatusConflict, "sandbox_profile_changed",
				"the predecessor's recorded sandbox is no longer valid: "+validateErr.Error())
			return seanceResolveResp{}, false
		}
		effectiveSandbox = &validated
	}
	effectiveSandbox, err = session.SandboxSnapshotForOneShotLaunch(effectiveSandbox)
	if err != nil {
		writeError(w, http.StatusConflict, "sandbox_profile_changed",
			"the predecessor's recorded sandbox is no longer valid: "+err.Error())
		return seanceResolveResp{}, false
	}
	if fail := sandboxProfileCapabilityFailure(h.Name, harnessBuiltinMode, effectiveSandbox); fail != nil {
		writeError(w, http.StatusConflict, "sandbox_profile_changed",
			"cannot reproduce the predecessor's recorded sandbox: "+fail.Msg)
		return seanceResolveResp{}, false
	}
	posture, err := session.OneShotLaunchPosture(
		cwd, h, harnessBuiltinMode, approvalPolicy, autoReview, effectiveSandbox)
	if err != nil {
		writeError(w, http.StatusConflict, "sandbox_profile_changed",
			"cannot reproduce the predecessor's recorded sandbox: "+err.Error())
		return seanceResolveResp{}, false
	}
	return seanceResolveResp{
		Predecessor:      target,
		Harness:          h.Name,
		Cwd:              cwd,
		Hops:             hops,
		Requested:        req.Back,
		Exact:            exact,
		Sandbox:          harnessBuiltinMode,
		Approval:         approvalPolicy,
		AutoReview:       autoReview,
		SandboxDenyDirs:  append([]string(nil), posture.SandboxDenyDirs...),
		launchPosture:    posture,
		effectiveSandbox: effectiveSandbox,
	}, true
}

// boundedHeadBuffer retains the first max bytes and silently consumes the
// remainder. The runner cancels the process on the first overflow so a noisy
// child cannot keep producing unbounded output after the response is doomed.
type boundedHeadBuffer struct {
	max       int
	buf       []byte
	truncated bool
	onLimit   func()
}

func (b *boundedHeadBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := b.max - len(b.buf)
	if remaining > 0 {
		b.buf = append(b.buf, p[:min(remaining, len(p))]...)
	}
	if len(p) > remaining && !b.truncated {
		b.truncated = true
		if b.onLimit != nil {
			b.onLimit()
		}
	}
	return n, nil
}

func (b *boundedHeadBuffer) String() string {
	return strings.ToValidUTF8(string(b.buf), "?")
}

func liveRunSeanceHarness(ctx context.Context, plan SeanceExecPlan) SeanceExecResult {
	if len(plan.Argv) == 0 {
		return SeanceExecResult{Err: errors.New("empty harness command")}
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stdout := &boundedHeadBuffer{max: maxSeanceAnswerBytes, onLimit: cancel}
	stderr := &tailBuffer{max: maxSeanceStderrBytes}
	cmd := executil.CommandContext(runCtx, plan.Argv[0], plan.Argv[1:]...)
	cmd.Dir = plan.Cwd
	cmd.Env = append(seanceProcessEnv(plan.Environment),
		"TCLAUDE_IGNORE_HOOKS=true",
		// Any tclaude command a resumed harness tries to invoke must fail
		// closed as an agent caller; it must never inherit daemon/human status.
		"TCLAUDE_AGENT_HINT=1",
	)
	cmd.Stdin = nil
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return SeanceExecResult{
			Stdout: stdout.String(),
			Stderr: strings.ToValidUTF8(stderr.String(), "?"),
			Err:    err,
		}
	}
	err := cmd.Wait()
	return SeanceExecResult{
		Stdout:          stdout.String(),
		Stderr:          strings.ToValidUTF8(stderr.String(), "?"),
		Started:         true,
		StdoutTruncated: stdout.truncated,
		Err:             err,
	}
}

func seanceProcessEnv(overrides map[string]string) []string {
	base := spawnEnvWithoutOperatorToken()
	blocked := map[string]bool{
		humanTokenEnvVar:       true,
		"TCLAUDE_IGNORE_HOOKS": true,
		"TCLAUDE_AGENT_HINT":   true,
	}
	for name := range overrides {
		blocked[name] = true
	}
	out := make([]string, 0, len(base)+len(overrides))
	for _, kv := range base {
		name, _, _ := strings.Cut(kv, "=")
		if blocked[name] {
			continue
		}
		out = append(out, kv)
	}
	names := make([]string, 0, len(overrides))
	for name := range overrides {
		if !blockedSeanceEnvironmentName(name) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		out = append(out, name+"="+overrides[name])
	}
	return out
}

func blockedSeanceEnvironmentName(name string) bool {
	switch name {
	case humanTokenEnvVar, "TCLAUDE_IGNORE_HOOKS", "TCLAUDE_AGENT_HINT":
		return true
	default:
		return false
	}
}

func resolveSeanceGeneration(
	w http.ResponseWriter,
	req seanceResolveReq,
	caller string,
	isHuman bool,
) (target string, hops int, exact bool, ok bool) {
	if req.Target == "" {
		if isHuman {
			writeError(w, http.StatusBadRequest, "invalid_arg",
				"the human operator must pass --target; there is no calling agent predecessor")
			return "", 0, false, false
		}
		target, hops, ok = walkSeancePredecessor(w, caller, req.Back)
		return target, hops, false, ok
	}

	// Conversation IDs are generation selectors, deliberately resolved before
	// the general agent selector so a predecessor never redirects to its live
	// head. The durable lookup includes succession rows, so a historical
	// generation stays exact even after its conv_index cache row is pruned.
	exactID, matchCount, shortPrefix, err := exactSeanceGeneration(req.Target)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return "", 0, false, false
	}
	if shortPrefix {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			fmt.Sprintf("conversation prefix %q is too short; pass at least 8 characters", req.Target))
		return "", 0, false, false
	}
	if matchCount > 1 {
		writeError(w, http.StatusConflict, "ambiguous",
			fmt.Sprintf("conversation prefix %q matches multiple generations; pass at least 8 unique characters", req.Target))
		return "", 0, false, false
	}
	if exactID != "" {
		successor, serr := db.GetConvSuccessor(exactID)
		if serr != nil {
			writeError(w, http.StatusInternalServerError, "io", serr.Error())
			return "", 0, false, false
		}
		if successor == "" {
			writeError(w, http.StatusConflict, "invalid_arg",
				fmt.Sprintf("conversation %s is a live generation or was never superseded; a séance requires a dead predecessor",
					short8(exactID)))
			return "", 0, false, false
		}
		return exactID, 0, true, true
	}

	res, matches, rerr := agent.ResolveSelectorCached(req.Target)
	if errors.Is(rerr, agent.ErrAmbiguous) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":      "selector matches multiple agents",
			"code":       "ambiguous",
			"candidates": peerEntriesFromResolved(matches),
		})
		return "", 0, false, false
	}
	if rerr != nil {
		writeError(w, http.StatusNotFound, "not_found", rerr.Error())
		return "", 0, false, false
	}
	if res.AgentID == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			fmt.Sprintf("selector %q resolves to a conversation, not an agent", req.Target))
		return "", 0, false, false
	}
	if !isHuman && !sameActor(caller, res.ConvID) {
		writeError(w, http.StatusForbidden, "permission",
			"an agent may hold a séance only with one of its own predecessor generations")
		return "", 0, false, false
	}
	target, hops, ok = walkSeancePredecessor(w, res.ConvID, req.Back)
	return target, hops, false, ok
}

// exactSeanceGeneration returns (id, matchCount, shortPrefix, error). A
// matchCount greater than one is an ambiguous prefix; zero means the selector
// should fall through to stable agent-id/name resolution. Conversation IDs are
// deliberately not assumed to be UUIDs: OpenCode uses ses_... IDs.
func exactSeanceGeneration(selector string) (string, int, bool, error) {
	if strings.HasPrefix(selector, db.AgentIDPrefix) {
		return "", 0, false, nil
	}
	ids, err := db.FindKnownConvIDsByPrefix(selector, 2)
	if err != nil {
		return "", 0, false, err
	}
	if len(selector) < 8 && len(ids) > 0 {
		return "", len(ids), true, nil
	}
	if len(ids) == 1 {
		return ids[0], 1, false, nil
	}
	return "", len(ids), false, nil
}

func walkSeancePredecessor(w http.ResponseWriter, head string, back int) (string, int, bool) {
	target, hops, err := db.ResolvePredecessorN(head, back)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return "", 0, false
	}
	if target == "" {
		writeError(w, http.StatusNotFound, "not_found",
			"you have no predecessor to consult — this conversation was not reincarnated from another agent")
		return "", 0, false
	}
	return target, hops, true
}
