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
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	processengine "github.com/tofutools/tclaude/pkg/claude/process/engine"
	processexec "github.com/tofutools/tclaude/pkg/claude/process/exec"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	processverify "github.com/tofutools/tclaude/pkg/claude/process/verify"
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
		ID           string               `json:"id"`
		TemplateRef  string               `json:"templateRef"`
		Verification processverify.Report `json:"verification"`
	}
	views := make([]runView, 0, len(runs))
	for _, run := range runs {
		views = append(views, runView{ID: run.ID, TemplateRef: run.TemplateRef, Verification: processverify.StoreRun(r.Context(), fs, run.ID)})
	}
	writeProcessJSON(w, http.StatusOK, map[string]any{"runs": views})
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

// StartProcessEngineForTest starts the dynamic feature supervisor with a
// test-scale interval and returns a channel closed after shutdown completes.
func StartProcessEngineForTest(stop <-chan struct{}, interval time.Duration) <-chan struct{} {
	return startProcessEngineAtInterval(stop, interval)
}
