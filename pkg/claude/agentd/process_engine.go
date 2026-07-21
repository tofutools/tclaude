package agentd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	processengine "github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/evidence"
	processexec "github.com/tofutools/tclaude/pkg/claude/process/exec"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/state/epochv8"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	processverify "github.com/tofutools/tclaude/pkg/claude/process/verify"
	processview "github.com/tofutools/tclaude/pkg/claude/process/view"
)

const processEngineTickInterval = time.Second

var (
	processStoreRootMu       sync.RWMutex
	processStoreRootOverride string
)

func startProcessEngine(stop <-chan struct{}) <-chan struct{} {
	return startProcessEngineAtInterval(stop, processEngineTickInterval)
}

func startProcessEngineAtInterval(stop <-chan struct{}, interval time.Duration) <-chan struct{} {
	return startProcessEngineSupervisor(stop, interval, nil, nil)
}

func startProcessEngineSupervisor(
	stop <-chan struct{},
	interval time.Duration,
	manualPulses <-chan struct{},
	observed chan<- bool,
) <-chan struct{} {
	supervisorDone := make(chan struct{})
	go func() {
		defer close(supervisorDone)
		var (
			ticker  *time.Ticker
			tickerC <-chan time.Time
		)
		if interval > 0 {
			ticker = time.NewTicker(interval)
			tickerC = ticker.C
			defer ticker.Stop()
		}
		pulses := make(chan struct{}, 1)
		pulses <- struct{}{}
		var (
			host                        *processengine.Host
			cancel                      context.CancelFunc
			done                        <-chan struct{}
			reportDisabledWhenTickStops bool
		)
		stopActiveTick := func() {
			if cancel != nil {
				cancel()
			}
			if done != nil {
				<-done
			}
			cancel = nil
			done = nil
		}
		defer stopActiveTick()
		reportObserved := func(enabled bool) bool {
			if observed == nil {
				return true
			}
			select {
			case observed <- enabled:
				return true
			case <-stop:
				return false
			}
		}
		for {
			select {
			case <-stop:
				return
			case <-done:
				cancel = nil
				done = nil
				if reportDisabledWhenTickStops {
					reportDisabledWhenTickStops = false
					if !reportObserved(false) {
						return
					}
				}
			case <-tickerC:
				select {
				case pulses <- struct{}{}:
				default:
				}
			case _, ok := <-manualPulses:
				if !ok {
					manualPulses = nil
					continue
				}
				select {
				case pulses <- struct{}{}:
				default:
				}
			case <-pulses:
				cfg, err := config.Load()
				enabled := err == nil && cfg.ProcessesEnabled()
				if !enabled {
					if cancel != nil {
						cancel()
						// Production keeps supervising asynchronously while the
						// canceled tick exits. Tests receive the disabled barrier
						// only after the same done path observes quiescence.
						reportDisabledWhenTickStops = observed != nil
						continue
					}
					if !reportObserved(false) {
						return
					}
					continue
				}
				if done != nil {
					if !reportObserved(true) {
						return
					}
					continue
				}
				if host == nil {
					created, createErr := newProcessEngineHost(processStoreRoot())
					if createErr != nil {
						slog.Warn("process engine: initialize failed", "error", createErr)
						if !reportObserved(true) {
							return
						}
						continue
					}
					host = created
				}
				ctx, tickCancel := context.WithCancel(context.Background())
				finished := make(chan struct{})
				cancel = tickCancel
				done = finished
				go func() {
					defer close(finished)
					results, tickErr := host.Tick(ctx)
					if ctx.Err() != nil {
						return
					}
					if tickErr != nil && ctx.Err() == nil {
						slog.Warn("process engine: tick failed", "error", tickErr)
						return
					}
					for _, result := range results {
						if result.Error != "" && !result.LeaseContended {
							slog.Warn("process engine: run tick failed", "run", result.RunID, "error", result.Error)
						} else if result.Waiting != "" {
							slog.Debug("process engine: run waiting", "run", result.RunID, "reason", result.Waiting)
						}
					}
				}()
				if !reportObserved(true) {
					return
				}
			}
		}
	}()
	return supervisorDone
}

func newProcessEngineHost(root string) (*processengine.Host, error) {
	return newLegacyProcessEngineHost(root)
}

// newLegacyProcessEngineHost is retained as the focused test constructor for
// the ordinary production host. Persisted schema classification inside Host
// keeps schemas 1-6 on this executor and refuses schemas 7 and 8 before decode.
func newLegacyProcessEngineHost(root string) (*processengine.Host, error) {
	fs, err := store.NewFS(root)
	if err != nil {
		return nil, err
	}
	return processengine.New(fs, processEngineHolder(), map[model.PerformerKind]processexec.Adapter{
		model.PerformerProgram: processexec.ProgramAdapter{},
		model.PerformerAgent:   processAgentAdapter{},
		model.PerformerHuman:   processHumanAdapter{},
	}), nil
}

func processEngineHolder() string {
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		return fmt.Sprintf("agentd:%d:%d", os.Getpid(), time.Now().UnixNano())
	}
	return fmt.Sprintf("agentd:%d:%s", os.Getpid(), hex.EncodeToString(random))
}

func processStoreRoot() string {
	processStoreRootMu.RLock()
	override := processStoreRootOverride
	processStoreRootMu.RUnlock()
	if override != "" {
		return override
	}
	return store.DefaultRoot()
}

func processRoutesEnabled() bool {
	cfg, err := config.Load()
	return err == nil && cfg.ProcessesEnabled()
}

func processRoute(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !processRoutesEnabled() {
			http.NotFound(w, r)
			return
		}
		next(w, r)
	}
}

func handleProcessRuns(w http.ResponseWriter, r *http.Request) {
	fs, err := store.NewFS(processStoreRoot())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "process_store", err.Error())
		return
	}
	runs, err := fs.ListRuns(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "process_runs", err.Error())
		return
	}
	type runView struct {
		ID              string               `json:"id"`
		TemplateRef     string               `json:"templateRef"`
		Status          state.RunStatus      `json:"status"`
		Started         time.Time            `json:"started"`
		CurrentActivity string               `json:"currentActivity,omitempty"`
		Verification    processverify.Report `json:"verification"`
	}
	views := make([]runView, 0, len(runs))
	for _, run := range runs {
		kind, schemaErr := supportedProcessRunSchema(r.Context(), fs, run.ID)
		if schemaErr != nil {
			verification := processRunLoadFailure(run.ID, schemaErr)
			views = append(views, runView{ID: run.ID, TemplateRef: run.TemplateRef, Status: verification.EffectiveStatus, Started: run.CreatedAt, Verification: verification})
			continue
		}
		if kind == store.RunSchemaEpochV8 {
			snapshot, loadErr := fs.LoadEpochV8RunView(r.Context(), run.ID)
			if loadErr != nil {
				verification := processRunLoadFailure(run.ID, loadErr)
				views = append(views, runView{ID: run.ID, TemplateRef: run.TemplateRef, Status: verification.EffectiveStatus, Started: run.CreatedAt, Verification: verification})
				continue
			}
			status := epochV8EffectiveStatus(snapshot)
			verification := processverify.Report{RunID: run.ID, EffectiveStatus: status}
			views = append(views, runView{ID: run.ID, TemplateRef: snapshot.Run.TemplateRef, Status: status, Started: run.CreatedAt, CurrentActivity: "epoch_v8", Verification: verification})
			continue
		}
		if kind == store.RunSchemaResetRequired {
			verification := processRunLoadFailure(run.ID, store.ErrRunResetRequired)
			views = append(views, runView{ID: run.ID, TemplateRef: run.TemplateRef, Status: verification.EffectiveStatus, Started: run.CreatedAt, CurrentActivity: string(store.RunSchemaResetRequired), Verification: verification})
			continue
		}
		verification := processverify.StoreRun(r.Context(), fs, run.ID)
		st, _ := fs.LoadRunState(r.Context(), run.ID)
		views = append(views, runView{
			ID: run.ID, TemplateRef: run.TemplateRef, Status: verification.EffectiveStatus,
			Started: run.CreatedAt, CurrentActivity: currentProcessActivity(st), Verification: verification,
		})
	}
	writeProcessJSON(w, http.StatusOK, map[string]any{"runs": views})
}

type processRunCreateRequest struct {
	TemplateRef string            `json:"templateRef"`
	RunID       string            `json:"runId,omitempty"`
	Params      map[string]string `json:"params,omitempty"`
}

type processRunCreatedView struct {
	ID          string    `json:"id"`
	TemplateRef string    `json:"templateRef"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// handleProcessRunCreate instantiates one exact, immutable template version
// into the same filesystem store watched by the daemon's engine host. The
// shared engine.Instantiate helper is also used by `tclaude process run`, so
// defaults, required params, ids, initial state, and pinned run records cannot
// drift between the CLI and dashboard paths.
func handleProcessRunCreate(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermProcessRunsCreate); !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxProcessEditBody)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var body processRunCreateRequest
	if err := decoder.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "json", "request body must be one JSON object containing only templateRef, runId, and string-valued params")
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "json", "request body must contain exactly one JSON object")
		return
	}
	body.TemplateRef = strings.TrimSpace(body.TemplateRef)
	body.RunID = strings.TrimSpace(body.RunID)
	if body.TemplateRef == "" {
		writeError(w, http.StatusUnprocessableEntity, "process_run_invalid", "templateRef is required")
		return
	}
	fs, err := store.NewFS(processStoreRoot())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "process_store", "process store is unavailable")
		return
	}
	if _, err := fs.GetTemplateExact(r.Context(), body.TemplateRef); err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			http.NotFound(w, r)
		default:
			writeError(w, http.StatusUnprocessableEntity, "process_run_invalid", "templateRef must identify an available exact content-addressed template version")
		}
		return
	}
	run, err := processengine.Instantiate(r.Context(), fs, processengine.InstantiateRequest{
		TemplateRef:        body.TemplateRef,
		RunID:              body.RunID,
		Params:             body.Params,
		ReplayExisting:     body.RunID != "",
		EngineCapabilities: processengine.ProductionEngineCapabilities(),
	})
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			http.NotFound(w, r)
		case errors.Is(err, store.ErrRunExists):
			writeError(w, http.StatusConflict, "process_run_exists", "the requested process run id already exists")
		case processengine.IsInstantiateInputError(err):
			writeError(w, http.StatusUnprocessableEntity, "process_run_invalid", "template, runId, or params are invalid")
		default:
			writeError(w, http.StatusInternalServerError, "process_run_create", "process run could not be created")
		}
		return
	}
	// The audit route intentionally never buffers this request body because
	// params may contain secrets. Hand the middleware only the safe durable run
	// id after creation (or idempotent replay) succeeds.
	setAuditTargetLabel(r, run.ID)
	w.Header().Set("Location", "/v1/process/runs/"+url.PathEscape(run.ID)+"/view")
	writeProcessJSON(w, http.StatusCreated, map[string]any{"run": processRunCreatedView{
		ID: run.ID, TemplateRef: run.TemplateRef, CreatedAt: run.CreatedAt, UpdatedAt: run.UpdatedAt,
	}})
}

func currentProcessActivity(st *state.State) string {
	if st == nil {
		return ""
	}
	ids := make([]string, 0, len(st.Nodes))
	for id := range st.Nodes {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, status := range []state.NodeStatus{
		state.NodeStatusRunning,
		state.NodeStatusWaitingHuman,
		state.NodeStatusWaitingAgent,
		state.NodeStatusWaitingProgram,
		state.NodeStatusWaitingTimer,
		state.NodeStatusWaitingSignal,
		state.NodeStatusBlocked,
		state.NodeStatusReady,
	} {
		for _, id := range ids {
			if st.Nodes[id].Status == status {
				return id
			}
		}
	}
	return ""
}

func handleProcessRun(w http.ResponseWriter, r *http.Request) {
	setProcessNoStoreHeaders(w)
	runID := r.PathValue("id")
	fs, err := store.NewFS(processStoreRoot())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "process_store", err.Error())
		return
	}
	kind, schemaErr := supportedProcessRunSchema(r.Context(), fs, runID)
	if schemaErr != nil {
		if errors.Is(schemaErr, store.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		writeError(w, http.StatusInternalServerError, "process_run", "process run schema is unavailable")
		return
	}
	if kind == store.RunSchemaResetRequired {
		writeError(w, http.StatusConflict, "process_run_reset_required", store.ErrRunResetRequired.Error())
		return
	}
	if kind == store.RunSchemaEpochV8 {
		snapshot, loadErr := fs.LoadEpochV8RunView(r.Context(), runID)
		if loadErr != nil {
			writeError(w, http.StatusInternalServerError, "process_run", "schema-8 process run is unavailable")
			return
		}
		envelope, envelopeErr := epochV8SafeEnvelope(r.Context(), snapshot)
		if envelopeErr != nil {
			writeError(w, http.StatusConflict, "process_run_inconsistent", "schema-8 process run is not coherent")
			return
		}
		writeProcessJSON(w, http.StatusOK, envelope)
		return
	}
	snapshot, err := fs.LoadRun(r.Context(), runID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		writeError(w, http.StatusInternalServerError, "process_run", err.Error())
		return
	}
	writeProcessJSON(w, http.StatusOK, map[string]any{
		"run":          snapshot.Run,
		"state":        snapshot.State,
		"verification": processverify.Snapshot(snapshot),
	})
}

func handleProcessRunView(w http.ResponseWriter, r *http.Request) {
	setProcessNoStoreHeaders(w)
	runID := r.PathValue("id")
	fs, err := store.NewFS(processStoreRoot())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "process_view", "process run view is unavailable")
		return
	}
	kind, schemaErr := supportedProcessRunSchema(r.Context(), fs, runID)
	if schemaErr != nil {
		if errors.Is(schemaErr, store.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		// A malformed header is not a schema decision. Preserve the established
		// confirmed-existing degraded view below; valid unsupported schemas and
		// filesystem failures remain closed.
		if !store.IsDecodeError(schemaErr) {
			writeError(w, http.StatusInternalServerError, "process_view", "process run view is unavailable")
			return
		}
	}
	if kind == store.RunSchemaResetRequired {
		writeError(w, http.StatusConflict, "process_run_reset_required", store.ErrRunResetRequired.Error())
		return
	}
	if kind == store.RunSchemaEpochV8 {
		snapshot, loadErr := fs.LoadEpochV8RunView(r.Context(), runID)
		if loadErr != nil {
			writeError(w, http.StatusInternalServerError, "process_view", "process run view is unavailable")
			return
		}
		envelope, envelopeErr := epochV8SafeEnvelope(r.Context(), snapshot)
		if envelopeErr != nil {
			writeError(w, http.StatusConflict, "process_view_inconsistent", "schema-8 process run is not coherent")
			return
		}
		writeProcessJSON(w, http.StatusOK, envelope)
		return
	}
	snapshot, err := fs.LoadRunView(r.Context(), runID)
	if err != nil {
		exists, lookupErr := fs.HasRunView(runID)
		if lookupErr != nil {
			writeError(w, http.StatusInternalServerError, "process_view", "process run view is unavailable")
			return
		}
		if !exists && errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		if exists && degradableProcessViewError(err) {
			writeProcessJSON(w, http.StatusOK, processview.NewEnvelope(runID, processRunLoadFailure(runID, err)))
			return
		}
		writeError(w, http.StatusInternalServerError, "process_view", "process run view is unavailable")
		return
	}
	verification, tmpl, err := processverify.SnapshotWithExactPinnedTemplate(r.Context(), fs, snapshot)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "process_view", "process run view is unavailable")
		return
	}
	writeProcessJSON(w, http.StatusOK, processview.Build(snapshot, tmpl, verification))
}

func degradableProcessViewError(err error) bool {
	if store.IsDecodeError(err) || errors.Is(err, store.ErrNotFound) {
		return true
	}
	var readErr *evidence.ReadError
	return errors.As(err, &readErr)
}

// processRunLoadFailure deliberately omits the wrapped load error. Decode
// errors may contain corrupt bytes and filesystem errors may contain private
// absolute paths; the API needs a stable alarm code, not those internals.
func processRunLoadFailure(runID string, err error) processverify.Report {
	diagnostic := processverify.Diagnostic{
		Layer:    processverify.LayerLoad,
		Severity: model.SeverityError,
		Code:     "snapshot_unreadable",
		Message:  "run snapshot could not be read or decoded; advancement is halted pending verification and manual repair",
	}
	var readErr *evidence.ReadError
	if errors.As(err, &readErr) {
		diagnostic.Layer = processverify.LayerEvidence
		switch readErr.Kind {
		case evidence.ReadErrorTornTail:
			diagnostic.Code = "read_torn_tail"
			diagnostic.Message = "evidence log has a torn final record; advancement is halted pending verification and manual repair"
		case evidence.ReadErrorMalformed:
			diagnostic.Code = "read_malformed"
			diagnostic.Message = "evidence log contains a malformed record; advancement is halted pending verification and manual repair"
		}
	}
	return processverify.Report{
		RunID:           runID,
		EffectiveStatus: state.RunStatusInconsistent,
		Diagnostics:     []processverify.Diagnostic{diagnostic},
	}
}

type processReportRequest struct {
	CommandID   string `json:"command_id"`
	Verdict     string `json:"verdict"`
	EvidenceRef string `json:"evidence_ref"`
	Feedback    string `json:"feedback"`
}

func handleProcessReport(w http.ResponseWriter, r *http.Request) {
	callerConv, isHuman, ok := authedCaller(w, r)
	if !ok {
		return
	}
	if isHuman || callerConv == "" {
		writeError(w, http.StatusForbidden, "forbidden", "agent process reports require an authenticated agent pane")
		return
	}
	var body processReportRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "json", err.Error())
		return
	}
	body.CommandID = strings.TrimSpace(body.CommandID)
	body.Verdict = strings.TrimSpace(body.Verdict)
	body.EvidenceRef = strings.TrimSpace(body.EvidenceRef)
	if !processCommandIDPattern.MatchString(body.CommandID) || body.Verdict == "" || body.EvidenceRef == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "command_id, verdict, and evidence_ref are required")
		return
	}
	agentRow, err := db.AgentForProcessCommand(body.CommandID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	callerAgent := peerAgentID(callerConv)
	if agentRow == nil || callerAgent == "" || agentRow.AgentID != callerAgent {
		writeError(w, http.StatusForbidden, "forbidden", "caller does not own this process command")
		return
	}
	fs, err := store.NewFS(processStoreRoot())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "process_store", err.Error())
		return
	}
	actor := state.ActorRef("agent:" + callerAgent)
	kind, schemaErr := supportedProcessRunSchema(r.Context(), fs, r.PathValue("id"))
	if schemaErr != nil {
		writeError(w, http.StatusConflict, "process_report", "process run schema is unavailable")
		return
	}
	if kind == store.RunSchemaResetRequired {
		writeError(w, http.StatusConflict, "process_report", store.ErrRunResetRequired.Error())
		return
	}
	if kind == store.RunSchemaEpochV8 {
		executor := processexec.NewEpochV8External(fs)
		if _, err := executor.RecordObservation(r.Context(), r.PathValue("id"), r.PathValue("node"), body.CommandID, processexec.Observation{
			Actor: actor, Verdict: body.Verdict, Feedback: strings.TrimSpace(body.Feedback), EvidenceRef: body.EvidenceRef,
		}); err != nil {
			writeError(w, http.StatusConflict, "process_report", err.Error())
			return
		}
		writeProcessJSON(w, http.StatusOK, map[string]any{"recorded": true, "actor": actor})
		return
	}
	snapshot, err := fs.LoadRun(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "process_run", err.Error())
		return
	}
	outstanding, exists := snapshot.State.OutstandingCommands[body.CommandID]
	if !exists || outstanding.NodeID != r.PathValue("node") {
		writeError(w, http.StatusBadRequest, "invalid_arg", "command does not belong to the requested run/node")
		return
	}
	executor := processexec.New(fs, nil)
	if _, err := executor.RecordOutstandingObservation(r.Context(), snapshot.Run.ID, body.CommandID, processexec.Observation{
		Actor: actor, Verdict: body.Verdict, Feedback: strings.TrimSpace(body.Feedback), EvidenceRef: body.EvidenceRef,
	}); err != nil {
		writeError(w, http.StatusConflict, "process_report", err.Error())
		return
	}
	writeProcessJSON(w, http.StatusOK, map[string]any{"recorded": true, "actor": actor})
}

type processSignalRequest struct {
	Signal string `json:"signal"`
}

func handleProcessSignal(w http.ResponseWriter, r *http.Request) {
	_, ok := requirePermission(w, r, PermProcessAdvance)
	if !ok {
		return
	}
	var body processSignalRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "json", err.Error())
		return
	}
	body.Signal = strings.TrimSpace(body.Signal)
	if body.Signal == "" || len(body.Signal) > 512 {
		writeError(w, http.StatusBadRequest, "invalid_arg", "signal is required")
		return
	}
	fs, err := store.NewFS(processStoreRoot())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "process_store", err.Error())
		return
	}
	kind, err := supportedProcessRunSchema(r.Context(), fs, r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusConflict, "process_signal", "process run schema is unavailable")
		return
	}
	if kind == store.RunSchemaResetRequired {
		writeError(w, http.StatusConflict, "process_signal", store.ErrRunResetRequired.Error())
		return
	}
	if kind == store.RunSchemaEpochV8 {
		executor := processexec.NewEpochV8External(fs)
		if _, err := executor.SatisfySignal(r.Context(), r.PathValue("id"), r.PathValue("node"), body.Signal, state.ActorRef("system:agentd")); err != nil {
			writeError(w, http.StatusConflict, "process_signal", err.Error())
			return
		}
		writeProcessJSON(w, http.StatusOK, map[string]any{"recorded": true})
		return
	}
	writeError(w, http.StatusConflict, "process_signal", "run has no schema-7 signal wait")
}

func supportedProcessRunSchema(ctx context.Context, fs *store.FS, runID string) (store.RunSchemaKind, error) {
	return fs.RunStateSchemaKind(ctx, runID)
}

func epochV8PublicState(checkpoint *epochv8.CheckpointV8) map[string]any {
	view := checkpoint.View()
	return map[string]any{
		"stateSchemaVersion": epochv8.StateSchemaVersion,
		"binding":            view.Binding,
		"runId":              view.RunID,
		"originalEpochId":    view.OriginalEpoch,
		"currentEpochId":     view.CurrentEpoch,
		"epochs":             view.Epochs,
		"authorities":        view.Authorities,
	}
}

func writeProcessJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// SetProcessStoreRootForTest redirects the daemon process surface without
// adding a production configuration knob before the feature's storage policy
// is finalized. The returned cleanup restores the previous value.
func SetProcessStoreRootForTest(root string) func() {
	processStoreRootMu.Lock()
	previous := processStoreRootOverride
	processStoreRootOverride = root
	processStoreRootMu.Unlock()
	return func() {
		processStoreRootMu.Lock()
		processStoreRootOverride = previous
		processStoreRootMu.Unlock()
	}
}

// RunProcessEngineTickForTest runs the same host tick used by agentd while
// leaving clock and adapter construction under the flow test's control.
func RunProcessEngineTickForTest(ctx context.Context, host *processengine.Host) ([]processengine.RunResult, error) {
	return host.Tick(ctx)
}

// NewProcessEngineHostForTest builds the production adapter set against a
// test-owned process store. Flow tests use it to exercise real spawn/message
// choreography while still replacing only tmux and session-new boundaries.
func NewProcessEngineHostForTest(root string) (*processengine.Host, error) {
	return newProcessEngineHost(root)
}

// NewLegacyProcessEngineHostForTest builds the production adapter host with
// schema 7 disabled, for flow tests that pin v6 legacy servicing contracts.
func NewLegacyProcessEngineHostForTest(root string) (*processengine.Host, error) {
	return newLegacyProcessEngineHost(root)
}

// StartProcessEngineForTest starts the dynamic feature supervisor with a
// caller-driven pulse source. Each observation reports the feature state after
// that pulse has been applied; a false observation also means any active tick
// has been canceled and joined.
func StartProcessEngineForTest(stop <-chan struct{}) (chan<- struct{}, <-chan bool, <-chan struct{}) {
	pulses := make(chan struct{})
	observed := make(chan bool)
	done := startProcessEngineSupervisor(stop, 0, pulses, observed)
	return pulses, observed, done
}
