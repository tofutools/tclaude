package agentd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
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
	supervisorDone := make(chan struct{})
	go func() {
		defer close(supervisorDone)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		pulses := make(chan time.Time, 1)
		pulses <- time.Now()
		var (
			host   *processengine.Host
			cancel context.CancelFunc
			done   <-chan struct{}
		)
		for {
			select {
			case <-stop:
				if cancel != nil {
					cancel()
				}
				if done != nil {
					<-done
				}
				return
			case <-done:
				cancel = nil
				done = nil
			case now := <-ticker.C:
				select {
				case pulses <- now:
				default:
				}
			case <-pulses:
				cfg, err := config.Load()
				enabled := err == nil && cfg.ProcessesEnabled()
				if !enabled {
					if cancel != nil {
						cancel()
					}
					continue
				}
				if done != nil {
					continue
				}
				if host == nil {
					created, createErr := newProcessEngineHost(processStoreRoot())
					if createErr != nil {
						slog.Warn("process engine: initialize failed", "error", createErr)
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
			}
		}
	}()
	return supervisorDone
}

func newProcessEngineHost(root string) (*processengine.Host, error) {
	fs, err := store.NewFS(root)
	if err != nil {
		return nil, err
	}
	host := processengine.New(fs, processEngineHolder(), map[model.PerformerKind]processexec.Adapter{
		model.PerformerProgram: processexec.ProgramAdapter{},
		model.PerformerAgent:   processAgentAdapter{},
		model.PerformerHuman:   processHumanAdapter{},
	})
	return host, nil
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
		verification := processverify.StoreRun(r.Context(), fs, run.ID)
		st, _ := fs.LoadRunState(r.Context(), run.ID)
		views = append(views, runView{
			ID: run.ID, TemplateRef: run.TemplateRef, Status: verification.EffectiveStatus,
			Started: run.CreatedAt, CurrentActivity: currentProcessActivity(st), Verification: verification,
		})
	}
	writeProcessJSON(w, http.StatusOK, map[string]any{"runs": views})
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
	runID := r.PathValue("id")
	fs, err := store.NewFS(processStoreRoot())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "process_store", err.Error())
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
	runID := r.PathValue("id")
	fs, err := store.NewFS(processStoreRoot())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "process_view", "process run view is unavailable")
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
	actor := state.ActorRef("agent:" + callerAgent)
	if _, err := executor.RecordOutstandingObservation(r.Context(), snapshot.Run.ID, body.CommandID, processexec.Observation{
		Actor: actor, Verdict: body.Verdict, Feedback: strings.TrimSpace(body.Feedback), EvidenceRef: body.EvidenceRef,
	}); err != nil {
		writeError(w, http.StatusConflict, "process_report", err.Error())
		return
	}
	writeProcessJSON(w, http.StatusOK, map[string]any{"recorded": true, "actor": actor})
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

// StartProcessEngineForTest starts the dynamic feature supervisor with a
// test-scale interval and returns a channel closed after shutdown completes.
func StartProcessEngineForTest(stop <-chan struct{}, interval time.Duration) <-chan struct{} {
	return startProcessEngineAtInterval(stop, interval)
}
