package agentd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/harness/transcript"
)

// Morph relaunches an agent in place with a different launch form: model,
// effort, approval posture and a few other launch fields. Within a harness it
// is a durable relaunch-profile edit followed by the ordinary restart path
// (stop, then resume the same native conversation), so identity, inbox,
// groups, permissions and history are untouched and no harness-specific
// resume code is involved. A morph into another harness translates the
// history into a new conversation of that harness first (agent_morph_harness.go).
// Sandbox fields, cwd and drive are never morphable.

// morphPendingTTL bounds how long a requested morph waits for the agent to go
// idle before it is dropped.
var morphPendingTTL = 30 * time.Minute

// morphRequest is the POST body of /v1/agent/{sel}/morph and /v1/whoami/morph.
// Unknown fields are rejected, and the fields morph refuses by design are
// named in morphRefusedFields so a caller gets a reason, not a silent drop.
type morphRequest struct {
	ConfirmHistoryLoss    bool    `json:"confirm_history_loss,omitempty"`
	ProceedWithoutSummary bool    `json:"proceed_without_summary,omitempty"`
	Profile               string  `json:"profile,omitempty"`
	Harness               string  `json:"harness,omitempty"`
	Model                 *string `json:"model,omitempty"`
	Effort                *string `json:"effort,omitempty"`
	Approval              *string `json:"approval,omitempty"`
	Tools                 *string `json:"tools,omitempty"`
	AskTimeout            *string `json:"ask_timeout,omitempty"`
	AutoCompactWindow     *string `json:"auto_compact_window,omitempty"`
	TrustDir              *bool   `json:"trust_dir,omitempty"`
	// History picks the delivery rung of a cross-harness morph: "" or
	// "auto" (native, else a context file, else a handoff), "context_file",
	// or "handoff" (the agent writes a handoff for its successor first).
	History             string `json:"history,omitempty"`
	HistoryWindowTokens *int64 `json:"history_window_tokens,omitempty"`
	Now                 bool   `json:"now,omitempty"`
	Back                bool   `json:"back,omitempty"`
	Cancel              bool   `json:"cancel,omitempty"`
	DryRun              bool   `json:"dry_run,omitempty"`
}

var morphRefusedFields = map[string]string{
	"cwd":                    "the working directory is part of the conversation and cannot be morphed",
	"sandbox":                "sandbox settings cannot be changed by a morph",
	"sandbox_mode":           "sandbox settings cannot be changed by a morph",
	"sandbox_profile":        "sandbox settings cannot be changed by a morph",
	"sandbox_implementation": "sandbox settings cannot be changed by a morph",
	"network":                "sandbox settings cannot be changed by a morph",
	"model_proxy":            "the model proxy route cannot be changed by a morph",
	"copilot_api":            "the drive cannot be changed by a morph",
	"codex_app_server":       "the drive cannot be changed by a morph",
	"auto_review":            "auto-review cannot be changed by a morph",
}

func decodeMorphRequest(w http.ResponseWriter, r *http.Request) (morphRequest, bool) {
	var req morphRequest
	raw := map[string]json.RawMessage{}
	body, err := readLimitedBody(r)
	if err == nil && len(bytes.TrimSpace(body)) > 0 {
		err = json.Unmarshal(body, &raw)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "decode morph request: "+err.Error())
		return req, false
	}
	for key := range raw {
		if reason, refused := morphRefusedFields[key]; refused {
			writeError(w, http.StatusBadRequest, "not_morphable", key+": "+reason)
			return req, false
		}
	}
	if len(raw) > 0 {
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", "decode morph request: "+err.Error())
			return req, false
		}
	}
	req.Profile = strings.TrimSpace(req.Profile)
	req.Harness = strings.TrimSpace(req.Harness)
	switch req.History = strings.ReplaceAll(strings.TrimSpace(req.History), "-", "_"); req.History {
	case "", "auto":
		req.History = ""
	case "context_file", "handoff":
	default:
		writeError(w, http.StatusBadRequest, "invalid_arg", "history must be auto, context_file or handoff")
		return req, false
	}
	modes := 0
	for _, on := range []bool{req.Back, req.Cancel, req.hasForm()} {
		if on {
			modes++
		}
	}
	if modes != 1 {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			"give exactly one of: a new form (profile and/or launch fields), back, or cancel")
		return req, false
	}
	if req.Cancel && (req.Now || req.DryRun) {
		writeError(w, http.StatusBadRequest, "invalid_arg", "cancel cannot be combined with now or dry_run")
		return req, false
	}
	return req, true
}

func readLimitedBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	var buf bytes.Buffer
	_, err := buf.ReadFrom(http.MaxBytesReader(nil, r.Body, 64<<10))
	return buf.Bytes(), err
}

func (q morphRequest) hasFieldOverrides() bool {
	return q.Model != nil || q.Effort != nil || q.Approval != nil ||
		q.Tools != nil || q.AskTimeout != nil || q.AutoCompactWindow != nil || q.TrustDir != nil || q.HistoryWindowTokens != nil
}

func (q morphRequest) hasForm() bool {
	return q.Profile != "" || q.Harness != "" || q.Model != nil || q.Effort != nil || q.Approval != nil ||
		q.Tools != nil || q.AskTimeout != nil || q.AutoCompactWindow != nil || q.HistoryWindowTokens != nil
}

// resolveMorphTarget builds and validates the requested form against the
// target's own harness, with the same validators spawn and resume use. A
// named profile supplies defaults; explicit fields override it.
// A cross-harness form may name no field here: its approval requirement and
// defaults are applied by planCrossHarnessMorph.
func resolveMorphTarget(h *harness.Harness, req morphRequest, prof *db.SpawnProfile, cross bool) (db.AgentMorphForm, error) {
	var form db.AgentMorphForm
	if req.HistoryWindowTokens != nil {
		if !cross {
			return form, errors.New("history_window_tokens is only for cross-harness morphs")
		}
		if *req.HistoryWindowTokens < 1024 || *req.HistoryWindowTokens > 10_000_000 {
			return form, errors.New("history_window_tokens must be between 1024 and 10000000")
		}
		form.HistoryWindowTokens = req.HistoryWindowTokens
	}
	pick := func(explicit *string, fromProfile string) *string {
		if explicit != nil {
			v := strings.TrimSpace(*explicit)
			return &v
		}
		if strings.TrimSpace(fromProfile) != "" {
			v := strings.TrimSpace(fromProfile)
			return &v
		}
		return nil
	}
	var p db.SpawnProfile
	if prof != nil {
		p = *prof
		// The label claims the form IS that profile; a later morph --back
		// is authorized by it, so a form with overrides carries none.
		if !req.hasFieldOverrides() {
			form.Profile = prof.Name
		}
	}
	if cross && h.Name == harness.CopilotName && p.ContextWindowMax > 0 {
		if form.HistoryWindowTokens == nil || *form.HistoryWindowTokens > p.ContextWindowMax {
			form.HistoryWindowTokens = &p.ContextWindowMax
		}
	}
	model := pick(req.Model, p.Model)
	effort := pick(req.Effort, p.Effort)
	approval := pick(req.Approval, p.Approval)
	tools := pick(req.Tools, p.ToolGovernance)
	askTimeout := pick(req.AskTimeout, p.AskUserQuestionTimeout)
	autoCompact := pick(req.AutoCompactWindow, p.AutoCompactWindow)

	if model != nil && *model != "" {
		validated, err := h.Models.ValidateModel(*model)
		if err != nil {
			return form, fmt.Errorf("model: %w", err)
		}
		window := int64(0)
		if h.Name == harness.DefaultName && strings.HasSuffix(validated, "[1m]") {
			validated = strings.TrimSuffix(validated, "[1m]")
			window = oneMillionContextWindow
		}
		form.Model = &validated
		if h.Name == harness.DefaultName {
			form.ContextWindowSize = &window
		}
	} else if model != nil {
		return form, errors.New("model: an empty model is not a morph; name the model to switch to")
	}
	if effort != nil {
		validated, err := h.Models.ValidateEffort(*effort)
		if err != nil {
			return form, fmt.Errorf("effort: %w", err)
		}
		form.Effort = &validated
	}
	if approval != nil {
		validated, err := harness.ValidateApprovalPolicy(h, *approval)
		if err != nil {
			return form, fmt.Errorf("approval: %w", err)
		}
		if validated == "" {
			return form, errors.New("approval: name the approval posture to switch to")
		}
		form.Approval = &validated
	}
	if tools != nil {
		validated, err := harness.ResolveToolGovernance(h, *tools)
		if err != nil {
			return form, fmt.Errorf("tools: %w", err)
		}
		form.Tools = &validated
	}
	if askTimeout != nil {
		validated, err := harness.ResolveAskTimeoutMode(h, *askTimeout)
		if err != nil {
			return form, fmt.Errorf("ask timeout: %w", err)
		}
		form.AskTimeout = &validated
	}
	if autoCompact != nil {
		validated, err := harness.ResolveAutoCompactWindow(h, *autoCompact)
		if err != nil {
			return form, fmt.Errorf("auto-compact window: %w", err)
		}
		form.AutoCompactWindow = &validated
	}
	if !cross && form.Model == nil && form.Effort == nil && form.Approval == nil && form.Tools == nil &&
		form.AskTimeout == nil && form.AutoCompactWindow == nil {
		return form, errors.New("the profile sets no morphable launch field for this harness")
	}
	return form, nil
}

// morphFormView is the display shape of a form in responses and the note.
type morphFormView struct {
	Harness           string `json:"harness"`
	Model             string `json:"model,omitempty"`
	Effort            string `json:"effort,omitempty"`
	Approval          string `json:"approval,omitempty"`
	Tools             string `json:"tools,omitempty"`
	AskTimeout        string `json:"ask_timeout,omitempty"`
	AutoCompactWindow string `json:"auto_compact_window,omitempty"`
	Profile           string `json:"profile,omitempty"`
	// Sandbox is the harness-native sandbox mode, shown only for a
	// cross-harness morph, where it changes with the harness. It is one
	// layer: morphResponse.Sandbox says how the agent is confined.
	Sandbox string `json:"sandbox,omitempty"`
}

// overlayMorphForm returns base with every non-nil field of f applied.
func overlayMorphForm(base, f db.AgentMorphForm) db.AgentMorphForm {
	out := base
	if f.Model != nil {
		out.Model = f.Model
	}
	if f.ContextWindowSize != nil {
		out.ContextWindowSize = f.ContextWindowSize
	}
	if f.Effort != nil {
		out.Effort = f.Effort
	}
	if f.Approval != nil {
		out.Approval = f.Approval
	}
	if f.Tools != nil {
		out.Tools = f.Tools
	}
	if f.AskTimeout != nil {
		out.AskTimeout = f.AskTimeout
	}
	if f.AutoCompactWindow != nil {
		out.AutoCompactWindow = f.AutoCompactWindow
	}
	out.Profile = f.Profile
	return out
}

func viewMorphForm(harnessName string, f db.AgentMorphForm) morphFormView {
	s := func(v *string) string {
		if v == nil {
			return ""
		}
		return *v
	}
	model := s(f.Model)
	if model != "" && harnessName == harness.DefaultName && f.ContextWindowSize != nil && *f.ContextWindowSize == oneMillionContextWindow {
		model += "[1m]"
	}
	return morphFormView{
		Harness: harnessName, Model: model, Effort: s(f.Effort), Approval: s(f.Approval),
		Tools: s(f.Tools), AskTimeout: s(f.AskTimeout), AutoCompactWindow: s(f.AutoCompactWindow),
		Profile: f.Profile,
	}
}

func (v morphFormView) String() string {
	parts := []string{v.Harness}
	model := v.Model
	if model == "" {
		model = "default model"
	}
	parts = append(parts, model)
	if v.Effort != "" {
		parts = append(parts, "effort "+v.Effort)
	}
	if v.Approval != "" {
		parts = append(parts, "approval "+v.Approval)
	}
	if v.Tools != "" {
		parts = append(parts, "tools "+v.Tools)
	}
	if v.AskTimeout != "" {
		parts = append(parts, "ask timeout "+v.AskTimeout)
	}
	if v.AutoCompactWindow != "" {
		parts = append(parts, "auto-compact "+v.AutoCompactWindow)
	}
	if v.Profile != "" {
		parts = append(parts, "profile "+v.Profile)
	}
	if v.Sandbox != "" {
		parts = append(parts, "harness sandbox "+v.Sandbox)
	}
	return strings.Join(parts, " · ")
}

// morphResponse is one agent's morph outcome.
type morphResponse struct {
	AgentID string `json:"agent_id"`
	ConvID  string `json:"conv_id"`
	// Result is morphed | pending | cancelled | dry_run.
	Result       string                `json:"result"`
	Before       morphFormView         `json:"before"`
	After        morphFormView         `json:"after"`
	Detail       string                `json:"detail,omitempty"`
	PendingMorph *db.AgentPendingMorph `json:"pending_morph,omitempty"`
	Authority    string                `json:"authority,omitempty"`
	// History is the cross-harness history report: the preview for a dry
	// run or a pending morph, the applied translation for a morph.
	History *transcript.LossReport `json:"history,omitempty"`
	// Sandbox is the cross-harness posture after the morph, both layers.
	Sandbox *morphSandboxView `json:"sandbox,omitempty"`
}

// morphOutcome is the result of applying one morph.
type morphOutcome struct {
	result       string
	status       int
	code, detail string
	// convID is the agent's conversation after a cross-harness morph.
	convID  string
	history *transcript.LossReport
}

func handleWhoamiMorph(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	caller, isHuman, ok := authedCaller(w, r)
	if !ok {
		return
	}
	if isHuman || caller == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			"this endpoint morphs the calling agent; humans use POST /v1/agent/{selector}/morph")
		return
	}
	handleMorph(w, r, caller, true)
}

// handleAgentMorph handles POST /v1/agent/{sel}/morph. A self-target is
// routed to the self.morph gate: groups.members.morph and agent.morph never
// authorize an agent to morph itself.
func handleAgentMorph(w http.ResponseWriter, r *http.Request, targetConv string) {
	if r.Method == http.MethodGet {
		_, human, ok := authedCaller(w, r)
		if !ok {
			return
		}
		if !human {
			writeError(w, http.StatusForbidden, "operator_only", "morph progress is an operator dashboard read")
			return
		}
		id, err := db.AgentIDForConv(targetConv)
		if err != nil || id == "" {
			writeError(w, http.StatusNotFound, "not_agent", "agent not found")
			return
		}
		agent, err := db.GetAgent(id)
		if err != nil || agent == nil {
			writeError(w, http.StatusNotFound, "not_agent", "agent not found")
			return
		}
		pending, err := db.PendingMorphForAgent(id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"agent_id": id, "conv_id": agent.CurrentConvID, "pending_morph": pending})
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	p := peerFromContext(r.Context())
	self := classify(p) == classAgent && sameActor(p.ConvID, targetConv)
	handleMorph(w, r, targetConv, self)
}

func handleMorph(w http.ResponseWriter, r *http.Request, targetConv string, self bool) {
	req, ok := decodeMorphRequest(w, r)
	if !ok {
		return
	}
	agentID, err := db.AgentIDForConv(targetConv)
	if err != nil || agentID == "" {
		writeError(w, http.StatusConflict, "not_agent", "morph requires a stable agent identity")
		return
	}
	current, err := db.GetAgent(agentID)
	if err != nil || current == nil || !current.Active() {
		writeError(w, http.StatusConflict, "not_agent", "morph requires an active agent")
		return
	}
	targetConv = current.CurrentConvID
	h := harnessForConv(targetConv)
	if h == nil || h.Models == nil {
		writeError(w, http.StatusConflict, "relaunch_profile", "the agent's harness cannot be resolved")
		return
	}
	var prof *db.SpawnProfile
	if req.Profile != "" {
		prof, err = db.ResolveSpawnProfile(req.Profile)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		if prof == nil {
			writeError(w, http.StatusNotFound, "not_found", "no spawn profile "+req.Profile)
			return
		}
		if prof.Disabled {
			writeError(w, http.StatusConflict, "profile_disabled", "spawn profile "+prof.Name+" is disabled")
			return
		}
	}
	before, previous, err := db.CurrentMorphForm(targetConv)
	if err != nil {
		writeError(w, http.StatusConflict, "relaunch_profile", err.Error())
		return
	}
	if req.Back && previous == nil {
		writeError(w, http.StatusConflict, "no_previous_form", "the agent has not been morphed, so there is no previous form")
		return
	}

	// The destination harness: explicit, else the profile's, else (for
	// --back) the one the previous form was for, else the agent's own.
	dst := h
	if req.Harness != "" {
		if dst, err = harness.Resolve(req.Harness); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", "harness: "+err.Error())
			return
		}
	}
	if prof != nil && prof.Harness != "" {
		profileHarness := harnessOrDefault(prof.Harness)
		if req.Harness != "" && profileHarness != dst.Name {
			writeError(w, http.StatusBadRequest, "invalid_arg",
				fmt.Sprintf("spawn profile %s is for harness %s, not %s", prof.Name, profileHarness, dst.Name))
			return
		}
		if dst, err = harness.Resolve(profileHarness); err != nil {
			writeError(w, http.StatusConflict, "invalid_arg", "spawn profile "+prof.Name+": "+err.Error())
			return
		}
	}
	if req.Back && isCrossHarnessTarget(targetConv, *previous) {
		if dst, err = harness.Resolve(previous.Harness); err != nil {
			writeError(w, http.StatusConflict, "relaunch_profile", "previous form: "+err.Error())
			return
		}
	}
	cross := dst.Name != h.Name
	if cross && dst.Models == nil {
		writeError(w, http.StatusConflict, "cross_harness", "harness "+dst.Name+" has no launch-form catalog")
		return
	}

	// A spawn_profile-scoped grant covers a morph into that profile only.
	// Explicit fields on top of the profile make it a free-form morph, which
	// such a grant must not authorize, so the dimension is left undescribed.
	// A cross-harness --back is a morph into the profile its form came from.
	actx := ActionContext{}
	if prof != nil && !req.hasFieldOverrides() {
		actx.SpawnProfile = prof.Name
	} else if cross && req.Back && previous.Profile != "" {
		actx.SpawnProfile = previous.Profile
	}
	var caller string
	if self {
		caller, ok = requirePermission(w, r, PermSelfMorph, actx)
		if ok && req.Now {
			writeError(w, http.StatusBadRequest, "invalid_arg",
				"a self-morph cannot use now: it would stop the turn that asked for it; it applies when this turn ends")
			return
		}
	} else {
		caller, ok = requireCrossAgentPermission(w, r, PermAgentMorph, targetConv, actx)
	}
	if !ok {
		return
	}
	authority := authorizedPermissionForRequest(r, PermSelfMorph)
	if caller == "" {
		authority = "operator"
	}
	if prof != nil && prof.OperatorOnly && caller != "" {
		writeError(w, http.StatusForbidden, "operator_only", "spawn profile "+prof.Name+" is operator-only")
		return
	}
	// An agent moves another harness only into an operator-curated form: a
	// destination spawn profile, with nothing overridden on top.
	if cross && caller != "" && !req.Cancel && actx.SpawnProfile == "" {
		writeError(w, http.StatusForbidden, "profile_required",
			"an agent may morph an agent into another harness only through a "+dst.Name+" spawn profile, without overriding its fields")
		return
	}
	if cross && caller != "" && !req.Back && (prof == nil || prof.Harness == "") {
		writeError(w, http.StatusForbidden, "profile_required",
			"an agent may morph an agent into another harness only through a spawn profile for that harness")
		return
	}

	resp := morphResponse{AgentID: agentID, ConvID: targetConv, Before: viewMorphForm(h.Name, before), Authority: authority}
	setAuditTargetConv(r, targetConv)

	if req.Cancel {
		// Under the launch lock, so a cancel cannot report success while the
		// watcher is applying the same pending morph.
		lock := resumeLaunchLock(targetConv)
		lock.Lock()
		pending, _ := db.PendingMorphForAgent(agentID)
		if pending == nil {
			lock.Unlock()
			writeError(w, http.StatusNotFound, "no_pending_morph", "the agent has no pending morph")
			return
		}
		if failure := pendingMorphOwnedByOther(pending, caller); failure != "" {
			lock.Unlock()
			writeError(w, http.StatusConflict, "pending_morph_exists", failure)
			return
		}
		err := db.SetAgentPendingMorphForConv(targetConv, nil)
		lock.Unlock()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		resp.Result, resp.After = "cancelled", resp.Before
		setAuditDetail(r, "cancelled pending morph to "+pendingTargetView(targetConv, h.Name, before, pending.Target).String())
		writeJSON(w, http.StatusOK, resp)
		return
	}

	var target db.AgentMorphForm
	if req.Back {
		target = *previous
		if caller != "" && target.Profile != "" {
			back, _ := db.ResolveSpawnProfile(target.Profile)
			if back != nil && back.OperatorOnly {
				writeError(w, http.StatusForbidden, "operator_only",
					"the previous form came from operator-only spawn profile "+back.Name)
				return
			}
			if cross && (back == nil || back.Disabled) {
				writeError(w, http.StatusForbidden, "profile_required",
					"the previous form's spawn profile "+target.Profile+" is gone or disabled")
				return
			}
		}
		if !cross {
			// A same-harness previous form never moves the sandbox.
			target.Harness, target.HarnessBuiltinMode = "", nil
		}
	} else {
		target, err = resolveMorphTarget(dst, req, prof, cross)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_form", err.Error())
			return
		}
		if cross {
			target.Harness = dst.Name
			if prof != nil && strings.TrimSpace(prof.Sandbox) != "" {
				mode := strings.TrimSpace(prof.Sandbox)
				target.HarnessBuiltinMode = &mode
			}
		}
	}
	if !cross && req.History != "" {
		writeError(w, http.StatusBadRequest, "invalid_form",
			"history: only a cross-harness morph translates the history")
		return
	}
	if !cross && req.TrustDir != nil {
		writeError(w, http.StatusBadRequest, "invalid_form",
			"trust_dir: only a cross-harness morph launches a harness in a directory it may not trust yet")
		return
	}
	if cross {
		trust := req.TrustDir
		if trust == nil && prof != nil && !req.Back {
			trust = prof.TrustDir
		}
		target.TrustDir = false
		if trust != nil {
			if target.TrustDir, err = harness.ResolveTrustDir(dst, *trust); err != nil {
				writeError(w, http.StatusBadRequest, "invalid_form", "trust_dir: "+err.Error())
				return
			}
		}
	}

	var cm *crossMorph
	if cross {
		var fail *spawnFailure
		cm, fail = planCrossHarnessMorph(targetConv, dst, target, caller)
		if fail != nil {
			writeError(w, fail.Status, fail.Kind, fail.Msg)
			return
		}
		target = cm.target
		resp.Before.Sandbox = cm.sandboxBefore
		resp.After = viewMorphForm(dst.Name, target)
		resp.After.Sandbox = cm.sandboxAfter
		resp.Sandbox = &cm.sandbox
	} else {
		resp.After = viewMorphForm(h.Name, overlayMorphForm(before, target))
	}

	// Approval lineage: an agent may not morph a target into a broader
	// approval posture than its own. Unchanged postures need no check; the
	// cross-harness plan checked its own destination posture.
	if !cross && caller != "" && target.Approval != nil && (before.Approval == nil || *before.Approval != *target.Approval) {
		autoReview := false
		if rp, perr := db.AgentRelaunchProfileForConv(targetConv); perr == nil && rp != nil && rp.ApprovalAutoReview != nil {
			autoReview = *rp.ApprovalAutoReview
		}
		if failure := spawnApprovalLineageFailure(caller, h.Name, *target.Approval, autoReview); failure != nil {
			writeError(w, failure.Status, failure.Kind, strings.Replace(failure.Msg, "may not spawn a", "may not morph an agent into a", 1))
			return
		}
	}

	if existing, _ := db.PendingMorphForAgent(agentID); existing != nil {
		if failure := pendingMorphOwnedByOther(existing, caller); failure != "" {
			writeError(w, http.StatusConflict, "pending_morph_exists", failure)
			return
		}
	}

	history, handoffRequired := req.History, false
	if cm != nil {
		live := pickAliveSession(targetConv) != nil
		report, err := cm.preview(targetConv)
		if err != nil {
			// An unreadable history leaves only the handoff rung, which
			// needs the agent to take one more turn.
			if history == "context_file" || !live {
				msg := "the agent's " + h.Name + " history cannot be translated (" + err.Error() + ")"
				if !live {
					msg += ", and it is offline, so it cannot write a handoff either"
				}
				writeError(w, http.StatusConflict, "history_unreadable", msg)
				return
			}
			history, handoffRequired = "handoff", true
			report = &transcript.LossReport{Source: h.Name, Target: dst.Name}
			report.Add(transcript.LossHistoryUndecodable, 1, "")
		}
		if report.Count(transcript.LossEncryptedSummary) > 0 {
			report.NeedsHandoff = true
			report.Fallback = "native_handoff"
			report.SelectionReason = "compaction summary unreadable; ask the source for a handoff and carry it alongside the readable tail"
			// The readable tail remains useful even when the operator selected
			// handoff explicitly. Use the shared handoff-plus-tail path.
			history = "summary"
			handoffRequired = true
		}
		if report.NeedsSummary {
			report.Fallback = "native_summary"
			if !req.DryRun && !req.ConfirmHistoryLoss && !req.ProceedWithoutSummary {
				writeError(w, http.StatusConflict, "history_confirmation_required", fmt.Sprintf("drop %d%% of inline history, carry a source-written summary; confirm_history_loss is required", report.DroppedPercent))
				return
			}
			if history != "handoff" {
				history = "summary"
			}
			handoffRequired = true
		}
		if req.ProceedWithoutSummary && history == "summary" {
			history, handoffRequired = "", false
			report.Fallback = "native_without_summary"
			report.SelectionReason = "source could not write a summary; proceed explicitly with readable history and the full-transcript pointer"
		}
		if history == "summary" && !req.DryRun && !live {
			writeError(w, http.StatusConflict, "agent_offline", "the source agent is offline, so it cannot write the required handoff or summary; explicitly confirm proceed_without_summary to carry only readable history with its full-transcript pointer")
			return
		}
		if history == "handoff" && !live {
			writeError(w, http.StatusConflict, "agent_offline", "an offline agent cannot write a handoff")
			return
		}
		if (history == "handoff" || history == "summary") && req.Now {
			writeError(w, http.StatusBadRequest, "invalid_arg",
				"a handoff morph cannot use now: the agent has to take a turn to write the handoff")
			return
		}
		if history != "" && history != "summary" {
			report.Fallback = history
		}
		resp.History = report
	}

	if req.DryRun {
		resp.Result = "dry_run"
		writeJSON(w, http.StatusOK, resp)
		return
	}

	actorLabel := "operator"
	if caller != "" {
		actorLabel = auditConvLabel(caller)
	}
	pending := db.AgentPendingMorph{
		ConfirmHistoryLoss: req.ConfirmHistoryLoss || req.ProceedWithoutSummary, ProceedWithoutSummary: req.ProceedWithoutSummary, Target: target, Back: req.Back, ActorConv: caller, Actor: actorLabel, ConvID: targetConv, History: history,
		HandoffRequired: handoffRequired,
	}
	if resp.History != nil && resp.History.NeedsHandoff {
		pending.HandoffReason = "encrypted_compaction_summary"
	}
	out := morphOrDefer(agentID, targetConv, pending, req.Now, self)
	if out.status != http.StatusOK {
		writeError(w, out.status, out.code, out.detail)
		return
	}
	resp.Result, resp.Detail = out.result, out.detail
	if out.convID != "" {
		resp.ConvID = out.convID
	}
	if out.history != nil {
		resp.History = out.history
	}
	if out.result == "pending" {
		resp.PendingMorph, _ = db.PendingMorphForAgent(agentID)
	}
	setAuditDetail(r, fmt.Sprintf("%s: %s -> %s", out.result, resp.Before, resp.After))
	writeJSON(w, http.StatusOK, resp)
}

// pendingTargetView renders a pending morph's target for audit text.
func pendingTargetView(convID, harnessName string, before, target db.AgentMorphForm) morphFormView {
	if isCrossHarnessTarget(convID, target) {
		return viewMorphForm(target.Harness, target)
	}
	return viewMorphForm(harnessName, overlayMorphForm(before, target))
}

// pendingMorphOwnedByOther refuses an agent caller that would replace or
// cancel a pending morph someone else requested. The operator may always.
func pendingMorphOwnedByOther(pending *db.AgentPendingMorph, caller string) string {
	if caller == "" || pending == nil || sameActor(pending.ActorConv, caller) {
		return ""
	}
	return "the agent already has a pending morph requested by " + pending.Actor +
		"; it can be cancelled by its requester or the operator"
}

// morphOrDefer applies the morph now when the target is offline, idle, or now
// was requested, and otherwise records it as pending for the idle watcher.
// A self-morph is always deferred: its own turn is still running.
func morphOrDefer(agentID, convID string, pending db.AgentPendingMorph, now, self bool) morphOutcome {
	lock := resumeLaunchLock(convID)
	lock.Lock()
	defer lock.Unlock()
	if err := requireCurrentAgentGeneration(agentID, convID); err != nil {
		return morphOutcome{status: http.StatusConflict, code: "stale_generation", detail: err.Error()}
	}
	live := pickAliveSession(convID)
	handoff := pending.History == "handoff" || pending.History == "summary"
	if handoff && live == nil {
		return morphOutcome{status: http.StatusConflict, code: "agent_offline", detail: "an offline agent cannot write a handoff"}
	}
	if live != nil && (self || handoff || (!now && agentRestartIdleFailure(live, time.Now()) != "")) {
		pending.RequestedAt = time.Now().UTC()
		pending.ExpiresAt = pending.RequestedAt.Add(morphPendingTTL)
		if err := db.SetAgentPendingMorphForConv(convID, &pending); err != nil {
			return morphOutcome{status: http.StatusInternalServerError, code: "io", detail: err.Error()}
		}
		if handoff {
			requestMorphHandoff(convID, pending)
			if pending.History == "summary" {
				return morphOutcome{result: "pending", status: http.StatusOK, detail: "waiting for the source agent's history summary until " + pending.ExpiresAt.Format(time.RFC3339) + "; cancel any time; timeout leaves the source unchanged"}
			}
			return morphOutcome{result: "pending", status: http.StatusOK,
				detail: "waiting for the agent's handoff; the morph applies once it has submitted one and is idle " +
					"(by " + pending.ExpiresAt.Format(time.RFC3339) + " it proceeds without one)"}
		}
		return morphOutcome{result: "pending", status: http.StatusOK,
			detail: "the agent is busy; the morph applies when it is next fully idle (expires " +
				pending.ExpiresAt.Format(time.RFC3339) + ")"}
	}
	return applyMorphUnderLaunchLock(agentID, convID, pending, live)
}

// applyMorphUnderLaunchLock writes the new form and, for a live agent, stops
// and resumes the same conversation through the restart path. The caller
// holds resumeLaunchLock(convID) and has checked the generation. A form for
// another harness takes the cross-harness path instead.
func applyMorphUnderLaunchLock(agentID, convID string, pending db.AgentPendingMorph, live *db.SessionRow) morphOutcome {
	if isCrossHarnessTarget(convID, pending.Target) {
		return applyCrossHarnessMorphUnderLaunchLock(agentID, convID, pending, live)
	}
	// A same-harness form never moves the sandbox or the harness.
	pending.Target.Harness, pending.Target.HarnessBuiltinMode = "", nil
	fail := func(status int, code, detail string) morphOutcome {
		return morphOutcome{status: status, code: code, detail: detail}
	}
	if live == nil {
		if _, err := durableRelaunchConfigForConvWith(convID, pending.Target.ApplyTo); err != nil {
			return fail(http.StatusConflict, "relaunch_profile", "the new form cannot be launched: "+err.Error())
		}
		previous, err := db.ApplyAgentMorphForConv(convID, pending.Target)
		if err != nil {
			return fail(http.StatusInternalServerError, "io", err.Error())
		}
		deliverMorphNote(convID, pending, previous, nil)
		return morphOutcome{result: "morphed", status: http.StatusOK,
			detail: "the agent is offline; the new form applies when it is next woken"}
	}
	// Resolve the full config with the new form applied in memory, so a form
	// resume would reject fails here, before the agent is stopped.
	if _, err := durableRelaunchConfigForConvWith(convID, pending.Target.ApplyTo); err != nil {
		return fail(http.StatusConflict, "relaunch_profile", "the new form cannot be launched: "+err.Error())
	}
	// Nor stop an agent whose Claude subscription token is gone.
	if agentID, _ := db.AgentIDForConv(convID); agentID != "" {
		if err := claudeSubscriptionRelaunchBlocked(agentID); err != nil {
			return fail(http.StatusConflict, claudeSubscriptionLostCode, err.Error())
		}
	}
	clientHandoff := beginAgentRestartTmuxHandoff(live.TmuxSession)
	defer clientHandoff.finishForConv(convID)
	stopped := escalateShutdownUnderLaunchLock(convID, shutdownGrace)
	if stopped.Outcome == shutdownFailed {
		return fail(http.StatusInternalServerError, "shutdown_failed",
			"could not stop the agent to morph it: "+stopped.Detail)
	}
	if err := requireCurrentAgentGeneration(agentID, convID); err != nil {
		return fail(http.StatusConflict, "stale_generation",
			"agent generation changed while stopping; it was not morphed: "+err.Error())
	}
	// Written after the stop so a late hook from the old process cannot
	// project the old model back over the new form.
	previous, err := db.ApplyAgentMorphForConv(convID, pending.Target)
	if err != nil {
		return fail(http.StatusInternalServerError, "io",
			"the agent stopped, but its new form could not be recorded: "+err.Error()+"; use the normal wake action to resume it")
	}
	resume := resumeOneConvUnderLaunchLock(convID, false, nil)
	clientHandoff.finishForConv(convID)
	if resume.Action != "resumed" {
		return fail(http.StatusInternalServerError, "restart_failed",
			"the new form is recorded and the agent stopped, but it could not be resumed: "+resume.Detail+
				"; use the normal wake action to retry")
	}
	deliverMorphNote(convID, pending, previous, nil)
	return morphOutcome{result: "morphed", status: http.StatusOK}
}

// deliverMorphNote tells the morphed agent what changed. It rides the inbox
// like any message; nothing user-controlled is typed into the pane.
// history is the cross-harness history report (nil within a harness); the
// note carries its counts and categories only, never transcript content.
func deliverMorphNote(convID string, pending db.AgentPendingMorph, previous db.AgentMorphForm, history *transcript.LossReport) {
	h := harnessForConv(convID)
	harnessName := harnessOrDefault("")
	if h != nil {
		harnessName = h.Name
	}
	wasHarness := harnessName
	if previous.Harness != "" {
		wasHarness = previous.Harness
	}
	was := viewMorphForm(wasHarness, previous)
	now := viewMorphForm(harnessName, overlayMorphForm(previous, pending.Target))
	verb := "You were morphed"
	if pending.Back {
		verb = "You were morphed back"
	}
	by := pending.Actor
	if pending.ActorConv != "" && sameActor(pending.ActorConv, convID) {
		by = "yourself"
	}
	body := fmt.Sprintf("%s by %s.\n\nWas: %s\nNow: %s\n\nYour conversation history, identity, inbox, groups and permissions are unchanged.",
		verb, by, was, now)
	if history != nil {
		body = fmt.Sprintf("%s by %s.\n\nWas: %s\nNow: %s\n\n"+
			"Your identity, inbox, groups and permissions are unchanged. Your conversation was carried over "+
			"into a new %s conversation; tool calls from before appear as ⟦tool call⟧/⟦tool result⟧ records, "+
			"which are history, not tools you have.\n\n%s",
			verb, by, was, now, harnessName, history.Summary())
	}
	var groupID int64
	if groups, err := db.ListGroupsForConv(convID); err == nil && len(groups) > 0 {
		groupID = groups[0].ID
	}
	from := pending.ActorConv
	if sameActor(from, convID) {
		from = ""
	}
	if _, err := queueAgentMessage(&db.AgentMessage{
		GroupID: groupID, FromConv: from, ToConv: convID, Subject: "Morphed: " + now.Model, Body: body,
	}); err != nil {
		slog.Warn("morph: deliver note failed", "conv", convID, "error", err)
	}
}

// Pending-morph watcher. Applies a pending morph once the agent is fully idle,
// and drops it when it expires or the agent's generation changed.

var morphWatcherInflight sync.Map // agentID → struct{}

func startMorphWatcher(stop <-chan struct{}) {
	go func() {
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				sweepPendingMorphs(false)
			}
		}
	}()
}

// sweepPendingMorphs runs one watcher pass. sync makes it wait for every
// apply (tests); the daemon applies each agent in its own goroutine so one
// slow shutdown does not hold the others.
func sweepPendingMorphs(sync bool) {
	rows, err := db.ListAgentsWithPendingMorph()
	if err != nil {
		slog.Warn("morph watcher: list pending failed", "error", err)
		return
	}
	for _, row := range rows {
		if _, busy := morphWatcherInflight.LoadOrStore(row.AgentID, struct{}{}); busy {
			continue
		}
		run := func(row db.AgentWithPendingMorph) {
			defer morphWatcherInflight.Delete(row.AgentID)
			applyPendingMorph(row)
		}
		if sync {
			run(row)
		} else {
			go run(row)
		}
	}
}

func applyPendingMorph(row db.AgentWithPendingMorph) {
	pending := row.Pending
	convID := pending.ConvID
	lock := resumeLaunchLock(convID)
	lock.Lock()
	defer lock.Unlock()
	current, err := db.PendingMorphForAgent(row.AgentID)
	if err != nil || current == nil || !current.RequestedAt.Equal(pending.RequestedAt) {
		return // cancelled or replaced meanwhile
	}
	pending = *current // carries a handoff submitted since the sweep listed it
	drop := func(reason string) {
		_ = db.SetAgentPendingMorphForConv(row.CurrentConvID, nil)
		auditMorphSystem(row.AgentID, row.CurrentConvID, "dropped pending morph: "+reason, http.StatusConflict)
	}
	if row.CurrentConvID != convID || requireCurrentAgentGeneration(row.AgentID, convID) != nil {
		drop("the agent's conversation generation changed")
		return
	}
	expired := time.Now().After(pending.ExpiresAt)
	live := pickAliveSession(convID)
	if pending.History == "handoff" || pending.History == "summary" {
		// A handoff morph waits for the handoff, then for the agent to go
		// idle. At expiry it proceeds regardless: without a handoff it
		// falls back to the context-file rung, and a stuck agent is stopped.
		submitted := pending.Handoff != nil && strings.TrimSpace(pending.Handoff.Body) != ""
		if !expired && (!submitted || (live != nil && agentRestartIdleFailure(live, time.Now()) != "")) {
			return
		}
		if (expired && pending.History == "summary") || (!submitted && pending.HandoffRequired) {
			// The history cannot be read, so without a handoff there is
			// nothing to carry: drop it rather than stop the agent for a
			// morph that cannot succeed.
			_ = db.SetAgentPendingMorphForConv(convID, nil)
			detail := "the required handoff did not arrive before expiry; nothing to carry, so the agent was left unchanged"
			if pending.History == "summary" {
				detail = "the history summary was not ready to apply before expiry; the agent was left unchanged"
			}
			auditMorphSystem(row.AgentID, convID, "dropped pending morph: "+detail, http.StatusConflict)
			reportPendingMorphFailure(row.AgentID, convID, pending, detail)
			return
		}
		if !submitted {
			pending.History = "context_file"
		}
	} else {
		if expired {
			drop("it expired before the agent went idle")
			return
		}
		if live != nil && agentRestartIdleFailure(live, time.Now()) != "" {
			return
		}
	}
	out := applyMorphUnderLaunchLock(row.AgentID, convID, pending, live)
	result, status, detail := out.result, out.status, out.detail
	if status != http.StatusOK {
		// A failed pending morph is not retried: drop it, audit it, and tell
		// the requester and the operator, since the agent may now be stopped.
		_ = db.SetAgentPendingMorphForConv(convID, nil)
		auditMorphSystem(row.AgentID, convID, "pending morph failed: "+detail, status)
		reportPendingMorphFailure(row.AgentID, convID, pending, detail)
		return
	}
	if out.convID != "" {
		convID = out.convID
	}
	h := harnessForConv(convID)
	name := harnessOrDefault("")
	if h != nil {
		name = h.Name
	}
	auditMorphSystem(row.AgentID, convID, fmt.Sprintf("%s (pending, requested by %s): -> %s",
		result, pending.Actor, viewMorphForm(name, pending.Target)), http.StatusOK)
}

func reportPendingMorphFailure(agentID, convID string, pending db.AgentPendingMorph, detail string) {
	name := auditConvLabel(convID)
	subject := "Morph of " + name + " failed"
	body := fmt.Sprintf("The pending morph of %s (%s), requested by %s, failed: %s\n\nIf the agent is offline, wake it to resume it.",
		name, agentID, pending.Actor, detail)
	if _, err := recordHumanMessage("", subject, body); err != nil {
		slog.Warn("morph watcher: notify operator failed", "agent", agentID, "error", err)
	}
	if pending.ActorConv == "" || sameActor(pending.ActorConv, convID) {
		return
	}
	if _, err := queueAgentMessage(&db.AgentMessage{ToConv: pending.ActorConv, Subject: subject, Body: body}); err != nil {
		slog.Warn("morph watcher: notify requester failed", "agent", agentID, "error", err)
	}
}

func auditMorphSystem(agentID, convID, detail string, status int) {
	if _, err := db.InsertAuditLog(db.AuditLogEntry{
		ActorKind: db.AuditActorSystem, ActorLabel: "agentd morph watcher", Verb: "morph",
		TargetConv: convID, TargetAgent: agentID, TargetLabel: auditConvLabel(convID), Detail: auditClip(detail, 240),
		Method: http.MethodPost, Path: "internal://morph-watcher", Status: status,
		Source: db.AuditSourceReconcile,
	}); err != nil {
		slog.Warn("morph watcher: audit failed", "agent", agentID, "error", err)
	}
}
