package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/executor"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
)

const (
	processRunFallbackInterval = time.Minute
	processRunListDefault      = 20
	maxProcessRunRequestBytes  = db.MaxProcessRunParamsBytes + db.MaxProcessRunAuthorizationsBytes + 16<<10
)

var (
	errProcessRunClaimed     = errors.New("process run is already being driven")
	errProcessRuntimeStopped = errors.New("process runtime is shutting down")
	processProgramExecute    = executor.Execute
	processRuns              = newProcessRunManager()
)

type processRunClaim struct {
	run    *executor.Run
	ctx    context.Context
	cancel context.CancelFunc
	done   sync.Once
}

// processRunManager owns only short-lived in-process claims. SQLite remains
// authoritative for every checkpoint and command transition; a claim retains
// the one prepared executor.Run only while that run is actively advancing.
type processRunManager struct {
	mu          sync.Mutex
	claims      map[string]*processRunClaim
	sweepCursor string
	stopped     bool
	wg          sync.WaitGroup
}

type processRunStartMode int

const (
	processRunResume processRunStartMode = iota
	processRunReissue
	processRunRecordOutcome
)

type processRunStart struct {
	mode    processRunStartMode
	actor   string
	outcome executor.RecordedOutcome
}

type processRunView struct {
	ID                    string            `json:"id"`
	TemplateRef           string            `json:"templateRef"`
	Params                map[string]string `json:"params"`
	ProgramAuthorizations []string          `json:"programAuthorizations"`
	Status                engine.RunStatus  `json:"status"`
	StateVersion          int64             `json:"stateVersion"`
	Checkpoint            engine.Checkpoint `json:"checkpoint"`
	Action                string            `json:"action"`
	NeedsReconcile        bool              `json:"needsReconcile"`
	CreatedAt             time.Time         `json:"createdAt"`
	UpdatedAt             time.Time         `json:"updatedAt"`
}

type processRunCreateRequest struct {
	ID                       string            `json:"id,omitempty"`
	TemplateID               string            `json:"templateId"`
	Params                   map[string]string `json:"params,omitempty"`
	AuthorizeProgramProfiles []string          `json:"authorizeProgramProfiles"`
}

type processRunOutcomeRequest struct {
	Outcome  engine.ProgramOutcome `json:"outcome"`
	ExitCode int                   `json:"exitCode"`
	Error    string                `json:"error,omitempty"`
	Note     string                `json:"note,omitempty"`
}

func newProcessRunManager() *processRunManager {
	return &processRunManager{claims: make(map[string]*processRunClaim)}
}

func (m *processRunManager) claim(runID string) (*processRunClaim, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped {
		return nil, false, errProcessRuntimeStopped
	}
	if _, exists := m.claims[runID]; exists {
		return nil, false, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	claim := &processRunClaim{ctx: ctx, cancel: cancel}
	m.claims[runID] = claim
	m.wg.Add(1)
	return claim, true, nil
}

func (m *processRunManager) release(runID string, claim *processRunClaim) {
	claim.done.Do(func() {
		m.mu.Lock()
		if m.claims[runID] == claim {
			delete(m.claims, runID)
		}
		m.mu.Unlock()
		claim.cancel()
		m.wg.Done()
	})
}

func (m *processRunManager) claimed(runID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.claims[runID]
	return ok
}

func (m *processRunManager) begin(runID string, start processRunStart) (bool, error) {
	claim, acquired, err := m.claim(runID)
	if err != nil || !acquired {
		if !acquired && start.mode != processRunResume {
			return false, errProcessRunClaimed
		}
		return false, err
	}
	release := true
	defer func() {
		if release {
			m.release(runID, claim)
		}
	}()

	run, err := executor.LoadRun(runID)
	if err != nil {
		return false, err
	}
	claim.run = run
	if claim.ctx.Err() != nil {
		return false, errProcessRuntimeStopped
	}

	var dispatch *executor.Dispatch
	switch start.mode {
	case processRunResume:
		if run.Action().Kind == executor.ActionNeedsReconcile {
			return false, executor.ErrNeedsReconcile
		}
		dispatch, err = prepareProcessRun(run)
	case processRunReissue:
		dispatch, err = executor.Reissue(run, start.actor)
	case processRunRecordOutcome:
		err = executor.RecordOutcome(run, start.actor, start.outcome)
		if err == nil {
			dispatch, err = prepareProcessRun(run)
		}
	default:
		err = fmt.Errorf("unknown process run start mode")
	}
	if err != nil {
		return false, err
	}
	if dispatch == nil {
		switch run.Action().Kind {
		case executor.ActionNeedsReconcile:
			return false, executor.ErrNeedsReconcile
		case executor.ActionTerminal:
			return false, nil
		default:
			return false, fmt.Errorf("process run did not become dispatchable or terminal")
		}
	}

	release = false
	go m.drive(claim.ctx, runID, claim, dispatch)
	return true, nil
}

func prepareProcessRun(run *executor.Run) (*executor.Dispatch, error) {
	switch run.Action().Kind {
	case executor.ActionContinue:
		return executor.Prepare(run)
	case executor.ActionNeedsReconcile:
		return nil, executor.ErrNeedsReconcile
	case executor.ActionTerminal:
		return nil, nil
	default:
		return nil, fmt.Errorf("process run already has a live dispatch")
	}
}

func (m *processRunManager) drive(ctx context.Context, runID string, claim *processRunClaim, dispatch *executor.Dispatch) {
	defer m.release(runID, claim)
	for dispatch != nil {
		action := claim.run.Action()
		if action.Kind != executor.ActionDispatch || action.Command == nil {
			slog.Warn("process runtime: dispatch claim lost its command", "run", runID)
			return
		}
		authorization, ok := claim.run.AuthorizationFor(action.Command.Program.Profile)
		if !ok {
			// Creation refuses this shape, but an old/future malformed row must
			// still fail closed. Dropping the live permission makes the durable
			// outstanding command explicitly reconcilable on the next load.
			slog.Warn("process runtime: persisted program authorization missing",
				"run", runID, "profile", action.Command.Program.Profile)
			return
		}
		if _, err := processProgramExecute(ctx, claim.run, dispatch, authorization); err != nil {
			slog.Warn("process runtime: program drive stopped", "run", runID, "error", err)
			return
		}
		dispatch = nil
		switch claim.run.Action().Kind {
		case executor.ActionContinue:
			var err error
			dispatch, err = executor.Prepare(claim.run)
			if err != nil {
				slog.Warn("process runtime: continuation failed", "run", runID, "error", err)
				return
			}
		case executor.ActionTerminal, executor.ActionNeedsReconcile:
			return
		default:
			slog.Warn("process runtime: unexpected post-execution action", "run", runID,
				"action", claim.run.Action().Kind)
			return
		}
	}
}

func (m *processRunManager) shutdown(ctx context.Context) error {
	m.mu.Lock()
	m.stopped = true
	claims := make([]*processRunClaim, 0, len(m.claims))
	for _, claim := range m.claims {
		claims = append(claims, claim)
	}
	m.mu.Unlock()
	for _, claim := range claims {
		claim.cancel()
	}
	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func startProcessRunRuntime(stop <-chan struct{}) {
	go func() {
		sweepProcessRuns()
		ticker := time.NewTicker(processRunFallbackInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				sweepProcessRuns()
			}
		}
	}()
}

// sweepProcessRuns loads at most one store page. A cursor rotates the coarse
// fallback across larger active sets; events never trigger a full scan.
func sweepProcessRuns() {
	if !processRoutesEnabled() {
		return
	}
	processRuns.mu.Lock()
	after := processRuns.sweepCursor
	processRuns.mu.Unlock()
	runs, err := db.ListActiveProcessRuns(after, db.MaxProcessRunReadPage)
	if err != nil {
		slog.Warn("process runtime: active-run sweep failed", "error", err)
		return
	}
	next := ""
	if len(runs) == db.MaxProcessRunReadPage {
		next = runs[len(runs)-1].ID
	}
	processRuns.mu.Lock()
	processRuns.sweepCursor = next
	processRuns.mu.Unlock()
	for i := range runs {
		if _, err := processRuns.begin(runs[i].ID, processRunStart{mode: processRunResume}); err != nil &&
			!errors.Is(err, executor.ErrNeedsReconcile) && !errors.Is(err, errProcessRuntimeStopped) {
			slog.Warn("process runtime: active run did not start", "run", runs[i].ID, "error", err)
		}
	}
}

func handleProcessRuns(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		handleProcessRunList(w, r)
	case http.MethodPost:
		handleProcessRunCreate(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method", "method not allowed")
	}
}

func handleProcessRunList(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermProcessRunsRead); !ok {
		return
	}
	limit := processRunListDefault
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 || parsed > db.MaxProcessRunReadPage {
			writeError(w, http.StatusBadRequest, "process_run_limit", fmt.Sprintf("limit must be 1..%d", db.MaxProcessRunReadPage))
			return
		}
		limit = parsed
	}
	after := strings.TrimSpace(r.URL.Query().Get("after"))
	runs, err := db.ListProcessRuns(after, limit)
	if err != nil {
		writeProcessRuntimeError(w, err)
		return
	}
	views := make([]processRunView, 0, len(runs))
	for i := range runs {
		view, err := processRunViewOf(&runs[i])
		if err != nil {
			writeProcessRuntimeError(w, err)
			return
		}
		views = append(views, view)
	}
	next := ""
	if len(runs) == limit {
		next = runs[len(runs)-1].ID
	}
	writeProcessJSON(w, http.StatusOK, map[string]any{"runs": views, "next": next})
}

func handleProcessRunCreate(w http.ResponseWriter, r *http.Request) {
	caller, ok := requirePermission(w, r, PermProcessRunsManage)
	if !ok {
		return
	}
	var request processRunCreateRequest
	if err := decodeProcessRuntimeRequest(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "process_run_request", err.Error())
		return
	}
	actor, err := processTemplateAuthor(caller)
	if err != nil {
		writeError(w, http.StatusForbidden, "process_run_actor", err.Error())
		return
	}
	runID, err := createProcessRun(r.Context(), request, string(actor))
	if err != nil {
		writeProcessRuntimeError(w, err)
		return
	}
	if _, err := processRuns.begin(runID, processRunStart{mode: processRunResume}); err != nil {
		writeError(w, http.StatusInternalServerError, "process_run_created_not_started",
			fmt.Sprintf("run %q was created but could not start: %v", runID, err))
		return
	}
	view, err := loadProcessRunView(runID)
	if err != nil {
		writeProcessRuntimeError(w, err)
		return
	}
	writeProcessJSON(w, http.StatusCreated, view)
}

func handleProcessRun(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermProcessRunsRead); !ok {
		return
	}
	view, err := loadProcessRunView(r.PathValue("id"))
	if err != nil {
		writeProcessRuntimeError(w, err)
		return
	}
	writeProcessJSON(w, http.StatusOK, view)
}

func handleProcessRunResume(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermProcessRunsManage); !ok {
		return
	}
	runID := r.PathValue("id")
	started, err := processRuns.begin(runID, processRunStart{mode: processRunResume})
	if err != nil {
		writeProcessRuntimeError(w, err)
		return
	}
	view, err := loadProcessRunView(runID)
	if err != nil {
		writeProcessRuntimeError(w, err)
		return
	}
	writeProcessJSON(w, http.StatusAccepted, map[string]any{"started": started, "run": view})
}

func handleProcessRunReissue(w http.ResponseWriter, r *http.Request) {
	caller, ok := requirePermission(w, r, PermProcessRunsManage)
	if !ok {
		return
	}
	actor, err := processTemplateAuthor(caller)
	if err != nil {
		writeError(w, http.StatusForbidden, "process_run_actor", err.Error())
		return
	}
	runID := r.PathValue("id")
	started, err := processRuns.begin(runID, processRunStart{mode: processRunReissue, actor: string(actor)})
	if err != nil {
		writeProcessRuntimeError(w, err)
		return
	}
	view, err := loadProcessRunView(runID)
	if err != nil {
		writeProcessRuntimeError(w, err)
		return
	}
	writeProcessJSON(w, http.StatusAccepted, map[string]any{"started": started, "run": view})
}

func handleProcessRunRecordOutcome(w http.ResponseWriter, r *http.Request) {
	caller, ok := requirePermission(w, r, PermProcessRunsManage)
	if !ok {
		return
	}
	var request processRunOutcomeRequest
	if err := decodeProcessRuntimeRequest(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "process_run_outcome", err.Error())
		return
	}
	if request.Outcome != engine.ProgramSucceeded && request.Outcome != engine.ProgramFailed {
		writeError(w, http.StatusUnprocessableEntity, "process_run_outcome", "outcome must be succeeded or failed")
		return
	}
	if len(request.Error) > executor.MaxProgramErrorBytes || !utf8.ValidString(request.Error) || len(request.Note) > db.MaxProcessRunEventActor*16 || !utf8.ValidString(request.Note) {
		writeError(w, http.StatusUnprocessableEntity, "process_run_outcome", "error or note exceeds its bounded UTF-8 limit")
		return
	}
	actor, err := processTemplateAuthor(caller)
	if err != nil {
		writeError(w, http.StatusForbidden, "process_run_actor", err.Error())
		return
	}
	runID := r.PathValue("id")
	started, err := processRuns.begin(runID, processRunStart{
		mode: processRunRecordOutcome, actor: string(actor),
		outcome: executor.RecordedOutcome{Outcome: request.Outcome, ExitCode: request.ExitCode, Error: request.Error, Note: request.Note},
	})
	if err != nil {
		writeProcessRuntimeError(w, err)
		return
	}
	view, err := loadProcessRunView(runID)
	if err != nil {
		writeProcessRuntimeError(w, err)
		return
	}
	writeProcessJSON(w, http.StatusAccepted, map[string]any{"started": started, "run": view})
}

func createProcessRun(ctx context.Context, request processRunCreateRequest, actor string) (string, error) {
	templateID := strings.TrimSpace(request.TemplateID)
	if templateID == "" {
		return "", fmt.Errorf("%w: templateId is required", db.ErrProcessRunInvalid)
	}
	fs, err := store.NewFS(processStoreRoot())
	if err != nil {
		return "", err
	}
	head, err := fs.GetTemplateHead(ctx, templateID)
	if err != nil {
		return "", err
	}
	tmpl, err := fs.GetTemplate(ctx, head.Ref)
	if err != nil {
		return "", err
	}
	params := request.Params
	if params == nil {
		params = map[string]string{}
	}
	definition, err := engine.Prepare(tmpl, params)
	if err != nil {
		return "", err
	}
	authorizations, err := normalizeProcessRunAuthorizations(request.AuthorizeProgramProfiles)
	if err != nil {
		return "", err
	}
	if missing := missingProcessProgramAuthorizations(tmpl, authorizations); len(missing) > 0 {
		return "", &processProgramAuthorizationError{Profiles: missing}
	}
	runID := strings.TrimSpace(request.ID)
	if runID == "" {
		runID = db.NewProcessRunID()
	}
	checkpoint, err := engine.Initialize(runID, definition)
	if err != nil {
		return "", err
	}
	snapshot, err := model.CanonicalSemanticJSON(tmpl)
	if err != nil {
		return "", err
	}
	paramsJSON, err := json.Marshal(params)
	if err != nil {
		return "", err
	}
	authorizationsJSON, err := json.Marshal(authorizations)
	if err != nil {
		return "", err
	}
	checkpointJSON, err := json.Marshal(checkpoint)
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(struct {
		TemplateRef           string   `json:"templateRef"`
		ProgramAuthorizations []string `json:"programAuthorizations"`
	}{TemplateRef: head.Ref, ProgramAuthorizations: authorizations})
	if err != nil {
		return "", err
	}
	err = db.CreateProcessRun(db.ProcessRunCreate{
		ID: runID, TemplateRef: head.Ref, TemplateSnapshotJSON: snapshot,
		ParamsJSON: paramsJSON, ProgramAuthorizationsJSON: authorizationsJSON,
		Status: string(checkpoint.Status), CheckpointJSON: checkpointJSON,
		InitialEvents: []db.ProcessRunEvent{{
			Sequence: 1, OccurredAt: time.Now().UTC(), Kind: "run_created",
			PayloadJSON: payload, Actor: actor,
		}},
	})
	if err != nil {
		return "", err
	}
	return runID, nil
}

type processProgramAuthorizationError struct{ Profiles []string }

func (e *processProgramAuthorizationError) Error() string {
	profiles := make([]string, len(e.Profiles))
	for i, profile := range e.Profiles {
		if profile == "" {
			profiles[i] = "<empty>"
		} else {
			profiles[i] = profile
		}
	}
	return "program profiles require explicit authorization: " + strings.Join(profiles, ", ")
}

func normalizeProcessRunAuthorizations(profiles []string) ([]string, error) {
	if len(profiles) > db.MaxProcessRunAuthorizationProfiles {
		return nil, fmt.Errorf("%w: too many program authorization profiles", db.ErrProcessRunInvalid)
	}
	normalized := append([]string(nil), profiles...)
	slices.Sort(normalized)
	for i, profile := range normalized {
		if len(profile) > db.MaxProcessRunAuthorizationProfile || !utf8.ValidString(profile) {
			return nil, fmt.Errorf("%w: invalid program authorization profile", db.ErrProcessRunInvalid)
		}
		if i > 0 && profile == normalized[i-1] {
			return nil, fmt.Errorf("%w: duplicate program authorization profile %q", db.ErrProcessRunInvalid, profile)
		}
	}
	return normalized, nil
}

func missingProcessProgramAuthorizations(tmpl *model.Template, authorized []string) []string {
	allowed := make(map[string]struct{}, len(authorized))
	for _, profile := range authorized {
		allowed[profile] = struct{}{}
	}
	missingSet := make(map[string]struct{})
	for _, node := range tmpl.Nodes {
		if node.Type == model.NodeTypeTask && node.Performer != nil && node.Performer.Kind == model.PerformerProgram {
			if _, ok := allowed[node.Performer.Profile]; !ok {
				missingSet[node.Performer.Profile] = struct{}{}
			}
		}
	}
	missing := make([]string, 0, len(missingSet))
	for profile := range missingSet {
		missing = append(missing, profile)
	}
	slices.Sort(missing)
	return missing
}

func loadProcessRunView(runID string) (processRunView, error) {
	record, err := db.GetProcessRun(strings.TrimSpace(runID))
	if err != nil {
		return processRunView{}, err
	}
	return processRunViewOf(record)
}

func processRunViewOf(record *db.ProcessRun) (processRunView, error) {
	var checkpoint engine.Checkpoint
	if err := record.DecodeCheckpoint(&checkpoint); err != nil {
		return processRunView{}, err
	}
	var params map[string]string
	if err := record.DecodeParams(&params); err != nil {
		return processRunView{}, err
	}
	var authorizations []string
	if err := record.DecodeProgramAuthorizations(&authorizations); err != nil {
		return processRunView{}, err
	}
	claimed := processRuns.claimed(record.ID)
	action := "runnable"
	needsReconcile := false
	switch {
	case checkpoint.OutstandingCommand != nil && claimed:
		action = "executing"
	case checkpoint.OutstandingCommand != nil:
		action, needsReconcile = "needs_reconcile", true
	case checkpoint.Status != engine.RunRunning:
		action = "terminal"
	case claimed:
		action = "driving"
	}
	return processRunView{
		ID: record.ID, TemplateRef: record.TemplateRef, Params: params,
		ProgramAuthorizations: authorizations, Status: checkpoint.Status,
		StateVersion: record.StateVersion, Checkpoint: checkpoint,
		Action: action, NeedsReconcile: needsReconcile,
		CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt,
	}, nil
}

func decodeProcessRuntimeRequest(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxProcessRunRequestBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("request must contain one JSON value")
		}
		return err
	}
	return nil
}

func writeProcessRuntimeError(w http.ResponseWriter, err error) {
	var authorizationErr *processProgramAuthorizationError
	var eligibilityErr *engine.EligibilityError
	switch {
	case errors.As(err, &authorizationErr), errors.Is(err, executor.ErrUnauthorized):
		writeError(w, http.StatusForbidden, "process_program_unauthorized", err.Error())
	case errors.Is(err, db.ErrProcessRunNotFound), errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "process_run_not_found", err.Error())
	case errors.Is(err, db.ErrProcessRunExists):
		writeError(w, http.StatusConflict, "process_run_exists", err.Error())
	case errors.Is(err, errProcessRunClaimed):
		writeError(w, http.StatusConflict, "process_run_claimed", err.Error())
	case errors.Is(err, executor.ErrNeedsReconcile):
		writeError(w, http.StatusConflict, "process_run_needs_reconcile", err.Error())
	case errors.Is(err, executor.ErrNoReconciliation):
		writeError(w, http.StatusConflict, "process_run_not_reconcilable", err.Error())
	case errors.As(err, &eligibilityErr), errors.Is(err, engine.ErrTemplateIneligible),
		errors.Is(err, engine.ErrInvalidCheckpoint), errors.Is(err, engine.ErrInvalidTransition),
		errors.Is(err, db.ErrProcessRunInvalid):
		writeError(w, http.StatusUnprocessableEntity, "process_run_invalid", err.Error())
	case errors.Is(err, errProcessRuntimeStopped):
		writeError(w, http.StatusServiceUnavailable, "process_runtime_stopping", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "process_runtime", err.Error())
	}
}
