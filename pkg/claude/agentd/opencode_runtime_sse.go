package agentd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/opencodeapi"
)

func ensureOpenCodeSSE(runtime db.OpenCodeRuntime) {
	openCodeProcesses.Lock()
	process := openCodeProcesses.bySession[runtime.SessionID]
	if process == nil {
		process = &openCodeProcess{}
		openCodeProcesses.bySession[runtime.SessionID] = process
	}
	process.convID = runtime.ConvID
	if process.cancel != nil {
		openCodeProcesses.Unlock()
		return
	}
	if process.exited || process.stopping {
		// A dead server would spin the reconnect loop until reaping; a stopping
		// server must retain its registry tombstone until the old projector is
		// joined. In either state, starting a consumer is forbidden.
		openCodeProcesses.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	ctx = context.WithValue(ctx, openCodeSSEGenerationKey{}, process)
	sseDone := make(chan struct{})
	process.cancel = cancel
	process.sseDone = sseDone
	openCodeProcesses.Unlock()
	go func() {
		defer close(sseDone)
		consumeOpenCodeSSE(ctx, runtime)
	}()
}

func openCodeProjectorCurrent(ctx context.Context, sessionID string) bool {
	if ctx.Err() != nil {
		return false
	}
	generation, hasGeneration := ctx.Value(openCodeSSEGenerationKey{}).(*openCodeProcess)
	if !hasGeneration {
		// Direct unit/backfill callers are still bounded by their context.
		return true
	}
	openCodeProcesses.Lock()
	current := openCodeProcesses.bySession[sessionID]
	ok := current == generation && !generation.stopping && !generation.exited
	openCodeProcesses.Unlock()
	return ok
}

func openCodeProjectorApplyKey(runtime db.OpenCodeRuntime) string {
	if runtime.ConvID != "" {
		return "conv:" + runtime.ConvID
	}
	return "session:" + runtime.SessionID
}

func withOpenCodeProjectorApplyLock(
	ctx context.Context,
	runtime db.OpenCodeRuntime,
	apply func(),
) bool {
	key := openCodeProjectorApplyKey(runtime)
	openCodeProjectorApplyLocksMu.Lock()
	lock := openCodeProjectorApplyLocks[key]
	if lock == nil {
		lock = newOpenCodeProjectorApplyLock()
		openCodeProjectorApplyLocks[key] = lock
	}
	lock.users++
	openCodeProjectorApplyLocksMu.Unlock()
	defer func() {
		openCodeProjectorApplyLocksMu.Lock()
		lock.users--
		openCodeProjectorApplyLocksMu.Unlock()
	}()
	timer := time.NewTimer(openCodeProcessStopWait)
	select {
	case <-ctx.Done():
		timer.Stop()
		return false
	case <-timer.C:
		slog.Warn("OpenCode projector apply lock timed out",
			"session", runtime.SessionID, "conv_id", runtime.ConvID,
			"timeout", openCodeProcessStopWait)
		return false
	case <-lock.token:
		timer.Stop()
	}
	defer func() { lock.token <- struct{}{} }()
	apply()
	return true
}

func deleteOpenCodeProjectorApplyLockIfIdle(runtime db.OpenCodeRuntime) {
	key := openCodeProjectorApplyKey(runtime)
	openCodeProjectorApplyLocksMu.Lock()
	if lock := openCodeProjectorApplyLocks[key]; lock != nil && lock.users == 0 {
		delete(openCodeProjectorApplyLocks, key)
	}
	openCodeProjectorApplyLocksMu.Unlock()
}

// closeOpenCodeSSEBodyOnCancel actively interrupts Scanner.Scan when request
// context cancellation alone does not wake a custom or wedged transport. The
// Close runs in this watcher goroutine, so a pathological Close implementation
// cannot itself block process teardown.
func closeOpenCodeSSEBodyOnCancel(ctx context.Context, body io.ReadCloser) func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			if err := body.Close(); err != nil {
				slog.Debug("OpenCode SSE response close failed", "error", err)
			}
		case <-done:
		}
	}()
	return func() { close(done) }
}

func consumeOpenCodeSSE(ctx context.Context, runtime db.OpenCodeRuntime) {
	consumeOpenCodeSSEWithRetry(ctx, runtime, openCodeSSERetryDelay)
}

func consumeOpenCodeSSEWithRetry(ctx context.Context, runtime db.OpenCodeRuntime, retryDelay time.Duration) {
	endpoint := runtime.ServerURL + "/event?directory=" + url.QueryEscape(runtime.Cwd)
	projector := newOpenCodeEventProjector(runtime.ConvID, runtime.Cwd)
	for ctx.Err() == nil {
		request, err := openCodeRequest(http.MethodGet, endpoint, runtime, nil)
		if err == nil {
			request = request.WithContext(ctx)
			var response *http.Response
			response, err = opencodeapi.Do(openCodeSSEHTTPClient, request, runtime)
			if err == nil && response.StatusCode == http.StatusOK {
				stopBodyCancellation := closeOpenCodeSSEBodyOnCancel(ctx, response.Body)
				reconciled := withOpenCodeProjectorApplyLock(ctx, runtime, func() {
					if !openCodeProjectorCurrent(ctx, runtime.SessionID) {
						return
					}
					// The stream is live before snapshots are read, so events that
					// race reconciliation remain buffered on this response and are
					// applied afterward in server order. Serializing snapshots with
					// event application also makes a replacement generation run
					// after any timed-out predecessor finishes its current write.
					if reconcileErr := reconcileOpenCodeSSE(ctx, runtime, projector); reconcileErr != nil {
						slog.Debug("OpenCode SSE reconciliation failed",
							"session", runtime.SessionID, "error", reconcileErr)
					}
					// TCL-673: seed context from message history so a resumed session
					// or a daemon restart is authoritative before its next live turn.
					if openCodeProjectorCurrent(ctx, runtime.SessionID) {
						backfillOpenCodeContextUsage(ctx, runtime)
					}
				})
				if !reconciled {
					stopBodyCancellation()
					_ = response.Body.Close()
					err = errors.New("OpenCode SSE reconciliation apply lock unavailable")
				} else {
					scanner := bufio.NewScanner(response.Body)
					scanner.Buffer(make([]byte, 64<<10), openCodeMaxSSEEventBytes)
					applyLockUnavailable := false
					for scanner.Scan() {
						line := scanner.Text()
						if strings.HasPrefix(line, "data:") {
							if !consumeOpenCodeEvent(ctx, runtime, projector,
								json.RawMessage(strings.TrimSpace(strings.TrimPrefix(line, "data:")))) {
								applyLockUnavailable = true
								break
							}
						}
					}
					stopBodyCancellation()
					_ = response.Body.Close()
					err = scanner.Err()
					if applyLockUnavailable {
						err = errors.New("OpenCode SSE event apply lock unavailable")
					}
				}
			} else if response != nil {
				err = fmt.Errorf("OpenCode SSE returned HTTP %d", response.StatusCode)
				_ = response.Body.Close()
			}
		}
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			slog.Debug("OpenCode SSE disconnected; retrying",
				"session", runtime.SessionID, "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(retryDelay):
		}
	}
}

func reconcileOpenCodeSSE(
	ctx context.Context,
	runtime db.OpenCodeRuntime,
	projector *openCodeEventProjector,
) error {
	statuses := make(map[string]openCodeSessionStatus)
	var questions []openCodeQuestionRequest
	var permissions []openCodePermissionRequest
	for _, snapshot := range []struct {
		path   string
		target any
	}{
		{path: "/session/status", target: &statuses},
		{path: "/question", target: &questions},
		{path: "/permission", target: &permissions},
	} {
		endpoint := runtime.ServerURL + snapshot.path +
			"?directory=" + url.QueryEscape(runtime.Cwd)
		request, err := openCodeRequest(http.MethodGet, endpoint, runtime, nil)
		if err != nil {
			return err
		}
		request = request.WithContext(ctx)
		response, err := opencodeapi.Do(openCodeHTTPClient, request, runtime)
		if err != nil {
			return err
		}
		if response.StatusCode != http.StatusOK {
			_ = response.Body.Close()
			return fmt.Errorf("OpenCode snapshot %s returned HTTP %d",
				snapshot.path, response.StatusCode)
		}
		err = json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(snapshot.target)
		_ = response.Body.Close()
		if err != nil {
			return fmt.Errorf("decode OpenCode snapshot %s: %w", snapshot.path, err)
		}
	}
	projector.resetToolsForSnapshot()

	// Attention snapshots override "busy". OpenCode reports a session waiting
	// on a question or permission as busy, so applying status first would
	// briefly erase the more useful state and re-notify on every reconnect.
	for _, permission := range permissions {
		if projected := projector.projectPermission(permission); len(projected) > 0 {
			applyOpenCodeHooks(ctx, runtime, projector, projected)
			return nil
		}
	}
	for _, question := range questions {
		if projected := projector.projectQuestion(question); len(projected) > 0 {
			applyOpenCodeHooks(ctx, runtime, projector, projected)
			return nil
		}
	}
	projector.pendingAttention = false
	if status, ok := statuses[runtime.ConvID]; ok {
		// Force the authoritative snapshot through even when its OpenCode
		// status equals the pre-disconnect value. The tclaude state may still
		// be awaiting a permission/question that was answered while offline.
		if projected := projector.projectStatus(status, true); len(projected) > 0 {
			applyOpenCodeHooks(ctx, runtime, projector, projected)
			return nil
		}
	}
	// OpenCode 1.18.4 may omit an idle session from /session/status. Empty
	// attention snapshots plus no usable status therefore mean "not blocked
	// and not known busy": settle to idle. The SSE stream is already open, so
	// genuine concurrent work is buffered and immediately reasserts busy.
	applyOpenCodeHooks(ctx, runtime, projector,
		projector.projectStatus(openCodeSessionStatus{Type: "idle"}, true))
	return nil
}
