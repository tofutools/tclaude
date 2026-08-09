package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/codexappserver"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/notify"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

const (
	codexAppServerStatusPollInterval = 2 * time.Second
	codexAppServerUsagePollInterval  = 3 * time.Minute
	codexAppServerReadTimeout        = 2 * time.Second
	codexAppServerRateReadCoalesce   = time.Second
)

type codexAppServerObservation struct {
	sync.RWMutex
	status       string
	statusDetail string
	statusAt     time.Time
	usageAt      time.Time
}

type codexAppServerObservationSnapshot struct {
	Status       string
	StatusDetail string
	StatusAt     time.Time
	UsageAt      time.Time
}

func (o *codexAppServerObservation) snapshot() codexAppServerObservationSnapshot {
	if o == nil {
		return codexAppServerObservationSnapshot{}
	}
	o.RLock()
	defer o.RUnlock()
	return codexAppServerObservationSnapshot{
		Status: o.status, StatusDetail: o.statusDetail, StatusAt: o.statusAt,
		UsageAt: o.usageAt,
	}
}

// runCodexAppServerObserver polls stable, non-subscribing snapshots. Bootstrap
// does not initialize this connection until a validated TUI hook proves the
// thread exists: Codex 0.147.0 auto-subscribes every connection initialized
// before a fresh thread is created. Notifications are drained, but only the
// account-scoped rate-limit hint is actionable; thread-scoped notifications
// are unreachable by design.
func runCodexAppServerObserver(handle *codexAppServerHandle) (state, detail string) {
	statusTicker := time.NewTicker(codexAppServerStatusPollInterval)
	usageTicker := time.NewTicker(codexAppServerUsagePollInterval)
	defer statusTicker.Stop()
	defer usageTicker.Stop()

	refreshCodexAppServerRateLimits(handle, "app-server-read")
	consecutiveSnapshotFailures := 0
	for {
		select {
		case request := <-handle.client.ServerRequests():
			_ = handle.client.Close()
			return db.CodexAppServerUnavailable,
				"unexpected server request delivered to non-subscribing observer: " + request.Method
		case notification := <-handle.client.Notifications():
			handleCodexAppServerNotification(handle, notification)
		case <-statusTicker.C:
			if refreshCodexAppServerThreadSnapshot(handle) {
				consecutiveSnapshotFailures = 0
			} else {
				consecutiveSnapshotFailures++
				if consecutiveSnapshotFailures >= 3 {
					_ = handle.client.Close()
					return db.CodexAppServerUnavailable,
						"Codex app-server stopped answering verified thread snapshots"
				}
			}
		case <-usageTicker.C:
			refreshCodexAppServerRateLimits(handle, "app-server-read")
		case <-handle.client.Done():
			var unexpected *codexappserver.UnexpectedServerRequestError
			if errors.As(handle.client.Err(), &unexpected) {
				return db.CodexAppServerUnavailable,
					"unexpected server request delivered to non-subscribing observer: " + unexpected.Request.Method
			}
			return db.CodexAppServerDead, fmt.Sprint(handle.client.Err())
		}
	}
}

func handleCodexAppServerNotification(handle *codexAppServerHandle, notification codexappserver.Notification) {
	if handle == nil {
		return
	}
	now := time.Now().UTC()
	switch notification.Method {
	case codexappserver.NotificationAccountRateLimitsUpdated:
		// The notification is explicitly sparse in the 0.147 schema. Re-read
		// the complete snapshot instead of treating absent fields as clears.
		if now.Sub(handle.observation.snapshot().UsageAt) >= codexAppServerRateReadCoalesce {
			refreshCodexAppServerRateLimits(handle, "app-server-event")
		}
	}
}

func refreshCodexAppServerThreadSnapshot(handle *codexAppServerHandle) bool {
	if handle == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), codexAppServerReadTimeout)
	defer cancel()
	thread, err := handle.client.ReadThread(ctx, codexappserver.ThreadReadParams{
		ThreadID: handle.runtime.ThreadID,
	})
	if err != nil {
		slog.Debug("Codex app-server observer snapshot failed",
			"generation", handle.runtime.Generation, "error", err)
		return false
	}
	projectCodexAppServerRawStatus(handle, thread.Status, time.Now().UTC(), "app-server snapshot")
	return true
}

func projectCodexAppServerRawStatus(
	handle *codexAppServerHandle,
	raw json.RawMessage,
	at time.Time,
	source string,
) {
	var status codexappserver.ThreadStatus
	if err := json.Unmarshal(raw, &status); err != nil || status.Type == "" {
		var legacy string
		if json.Unmarshal(raw, &legacy) != nil {
			return
		}
		status.Type = legacy
	}
	projectCodexAppServerStatus(handle, status, at, source)
}

func projectCodexAppServerStatus(
	handle *codexAppServerHandle,
	status codexappserver.ThreadStatus,
	at time.Time,
	source string,
) {
	if handle == nil || at.IsZero() {
		return
	}
	normalized := ""
	switch status.Type {
	case "idle":
		normalized = session.StatusIdle
	case "active":
		normalized = session.StatusWorking
		for _, flag := range status.ActiveFlags {
			switch flag {
			case "waitingOnUserInput":
				normalized = session.StatusAwaitingInput
			case "waitingOnApproval":
				if normalized != session.StatusAwaitingInput {
					normalized = session.StatusAwaitingPermission
				}
			}
		}
	case "systemError":
		normalized = session.StatusError
	case "notLoaded":
		return
	default:
		return
	}
	detail := source
	handle.observation.Lock()
	if at.Before(handle.observation.statusAt) {
		handle.observation.Unlock()
		return
	}
	handle.observation.status = normalized
	handle.observation.statusDetail = detail
	handle.observation.statusAt = at
	handle.observation.Unlock()

	row, err := db.FindSessionByConvID(handle.runtime.ConvID)
	if err != nil || row == nil || row.Harness != harness.CodexName {
		return
	}
	previous := row.Status
	changed, err := db.SetSessionStatusForCodexAppServerGeneration(
		row.ID, row.ConvID, row.CreatedAt, handle.runtime.Generation,
		row.Status, row.UpdatedAt, normalized, detail, at)
	if err != nil {
		slog.Warn("Codex app-server observer status projection failed",
			"generation", handle.runtime.Generation, "error", err)
		return
	}
	if changed && previous != normalized {
		notify.OnStateTransition(row.ID, row.ConvID, previous, normalized,
			row.Cwd, "", row.Harness)
	}
}

func refreshCodexAppServerRateLimits(handle *codexAppServerHandle, source string) {
	if handle == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), codexAppServerReadTimeout)
	defer cancel()
	result, err := handle.client.ReadAccountRateLimits(ctx)
	if err != nil {
		slog.Debug("Codex app-server observer rate-limit read failed",
			"generation", handle.runtime.Generation, "error", err)
		return
	}
	snapshot, ok := result.RateLimitsByLimitID["codex"]
	if !ok {
		snapshot = result.RateLimits
		if snapshot.LimitID == nil || *snapshot.LimitID != "codex" {
			return
		}
	}
	observed := time.Now().UTC()
	usage := codexUsageFromAppServer(snapshot, observed)
	if usage.FiveHour == nil && usage.Weekly == nil {
		return
	}
	saveCodexUsageSnapshot(usage, source)
	handle.observation.Lock()
	if observed.After(handle.observation.usageAt) {
		handle.observation.usageAt = observed
	}
	handle.observation.Unlock()
}

func codexUsageFromAppServer(snapshot codexappserver.RateLimitSnapshot, observed time.Time) *harness.CodexUsage {
	usage := &harness.CodexUsage{Observed: observed}
	if snapshot.LimitID != nil {
		usage.LimitID = *snapshot.LimitID
	}
	if snapshot.LimitName != nil {
		usage.LimitName = *snapshot.LimitName
	}
	if snapshot.PlanType != nil {
		usage.PlanType = *snapshot.PlanType
	}
	for _, window := range []*codexappserver.RateLimitWindow{snapshot.Primary, snapshot.Secondary} {
		if window == nil || window.WindowDurationMins == nil || *window.WindowDurationMins <= 0 {
			continue
		}
		item := &harness.CodexRateLimitWindow{UsedPercent: float64(window.UsedPercent)}
		if window.ResetsAt != nil && *window.ResetsAt > 0 {
			item.ResetsAt = time.Unix(*window.ResetsAt, 0)
		}
		switch {
		case codexWindowNear(*window.WindowDurationMins, 300):
			usage.FiveHour = item
		case codexWindowNear(*window.WindowDurationMins, 10080):
			usage.Weekly = item
		}
	}
	return usage
}

func codexWindowNear(got, want int64) bool {
	tolerance := want / 20
	return got >= want-tolerance && got <= want+tolerance
}

func codexAppServerObservationForConv(convID string) (codexAppServerObservationSnapshot, bool) {
	handle := codexAppServerHandleForConv(strings.TrimSpace(convID))
	if handle == nil {
		return codexAppServerObservationSnapshot{}, false
	}
	return handle.observation.snapshot(), true
}
