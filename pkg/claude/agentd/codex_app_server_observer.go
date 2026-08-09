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
	codexAppServerContextFreshness   = 30 * time.Second
	codexAppServerRateReadCoalesce   = time.Second
)

type codexAppServerObservation struct {
	sync.RWMutex
	status       string
	statusDetail string
	statusAt     time.Time
	context      harness.ContextTelemetry
	contextAt    time.Time
	usageAt      time.Time
}

type codexAppServerObservationSnapshot struct {
	Status       string
	StatusDetail string
	StatusAt     time.Time
	Context      harness.ContextTelemetry
	ContextAt    time.Time
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
		Context: o.context, ContextAt: o.contextAt, UsageAt: o.usageAt,
	}
}

// runCodexAppServerObserver consumes only notifications app-server naturally
// sends to this control connection. It deliberately never calls thread/resume:
// Codex 0.147.0 broadcasts approval and user-input requests to every subscribed
// client, so subscribing a non-answering daemon would make it a second request
// owner. Gaps are repaired with stable, non-subscribing thread/read snapshots.
func runCodexAppServerObserver(handle *codexAppServerHandle) (state, detail string) {
	statusTicker := time.NewTicker(codexAppServerStatusPollInterval)
	usageTicker := time.NewTicker(codexAppServerUsagePollInterval)
	defer statusTicker.Stop()
	defer usageTicker.Stop()

	refreshCodexAppServerRateLimits(handle, "app-server-read")
	for {
		select {
		case request := <-handle.client.ServerRequests():
			_ = handle.client.Close()
			return db.CodexAppServerUnavailable,
				"unexpected server request delivered to non-subscribing observer: " + request.Method
		case notification := <-handle.client.Notifications():
			handleCodexAppServerNotification(handle, notification)
		case <-statusTicker.C:
			refreshCodexAppServerThreadSnapshot(handle)
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
	case codexappserver.NotificationThreadStatusChanged:
		var event codexappserver.ThreadStatusChangedNotification
		if json.Unmarshal(notification.Params, &event) == nil && event.ThreadID == handle.runtime.ThreadID {
			projectCodexAppServerStatus(handle, event.Status, now, "app-server event")
		}
	case codexappserver.NotificationThreadTokenUsageUpdated:
		var event codexappserver.ThreadTokenUsageUpdatedNotification
		if json.Unmarshal(notification.Params, &event) == nil && event.ThreadID == handle.runtime.ThreadID {
			projectCodexAppServerContext(handle, event.TokenUsage, now)
		}
	case codexappserver.NotificationAccountRateLimitsUpdated:
		// The notification is explicitly sparse in the 0.147 schema. Re-read
		// the complete snapshot instead of treating absent fields as clears.
		if now.Sub(handle.observation.snapshot().UsageAt) >= codexAppServerRateReadCoalesce {
			refreshCodexAppServerRateLimits(handle, "app-server-event")
		}
	case codexappserver.NotificationTurnStarted:
		var event codexappserver.ThreadScopedNotification
		if json.Unmarshal(notification.Params, &event) == nil && event.ThreadID == handle.runtime.ThreadID {
			projectCodexAppServerStatus(handle, codexappserver.ThreadStatus{Type: "active"},
				now, "app-server "+notification.Method)
		}
	case codexappserver.NotificationItemStarted:
		var event codexappserver.ThreadScopedNotification
		current := handle.observation.snapshot().Status
		if json.Unmarshal(notification.Params, &event) == nil && event.ThreadID == handle.runtime.ThreadID &&
			current != session.StatusAwaitingPermission && current != session.StatusAwaitingInput {
			projectCodexAppServerStatus(handle, codexappserver.ThreadStatus{Type: "active"},
				now, "app-server "+notification.Method)
		}
	case codexappserver.NotificationTurnCompleted:
		// Completion variants are additive and can represent interruption,
		// failure, compaction, or a normal item. Read the authoritative current
		// thread state instead of inferring idle from the method name.
		var event codexappserver.ThreadScopedNotification
		if json.Unmarshal(notification.Params, &event) == nil && event.ThreadID == handle.runtime.ThreadID {
			refreshCodexAppServerThreadSnapshot(handle)
		}
	case codexappserver.NotificationItemCompleted:
		// Item completion is useful sequencing evidence but does not by itself
		// imply a thread state: another item, an approval, or compaction may
		// immediately follow. The enclosing thread/turn events and bounded
		// snapshot own the normalized state.
	}
}

func refreshCodexAppServerThreadSnapshot(handle *codexAppServerHandle) {
	if handle == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), codexAppServerReadTimeout)
	defer cancel()
	thread, err := handle.client.ReadThread(ctx, codexappserver.ThreadReadParams{
		ThreadID: handle.runtime.ThreadID,
	})
	if err != nil {
		slog.Debug("Codex app-server observer snapshot failed",
			"generation", handle.runtime.Generation, "error", err)
		return
	}
	projectCodexAppServerRawStatus(handle, thread.Status, time.Now().UTC(), "app-server snapshot")
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

func projectCodexAppServerContext(
	handle *codexAppServerHandle,
	usage codexappserver.ThreadTokenUsage,
	at time.Time,
) {
	if handle == nil || usage.ModelContextWindow == nil || *usage.ModelContextWindow <= 0 ||
		usage.Last.InputTokens < 0 || usage.Last.OutputTokens < 0 || usage.Last.TotalTokens < 0 {
		return
	}
	used := usage.Last.TotalTokens
	if used == 0 {
		used = usage.Last.InputTokens + usage.Last.OutputTokens
	}
	context := harness.ContextTelemetry{
		Pct:         float64(used) / float64(*usage.ModelContextWindow) * 100,
		TokensInput: usage.Last.InputTokens, TokensOutput: usage.Last.OutputTokens,
		WindowSize: *usage.ModelContextWindow,
	}
	handle.observation.Lock()
	if at.Before(handle.observation.contextAt) {
		handle.observation.Unlock()
		return
	}
	handle.observation.context = context
	handle.observation.contextAt = at
	handle.observation.Unlock()

	row, err := db.FindSessionByConvID(handle.runtime.ConvID)
	if err != nil || row == nil || row.Harness != harness.CodexName {
		return
	}
	if _, err := db.UpdateContextSnapshotForCodexAppServerGeneration(
		row.ID, row.ConvID, row.CreatedAt, handle.runtime.Generation,
		context.Pct, context.TokensInput, context.TokensOutput, context.WindowSize); err != nil {
		slog.Warn("Codex app-server observer context projection failed",
			"generation", handle.runtime.Generation, "error", err)
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

func freshCodexAppServerContext(sess *db.SessionRow, now time.Time) (*harness.ContextTelemetry, bool) {
	if sess == nil || sess.Harness != harness.CodexName {
		return nil, false
	}
	observation, ok := codexAppServerObservationForConv(sess.ConvID)
	if !ok || observation.ContextAt.IsZero() || now.Sub(observation.ContextAt) > codexAppServerContextFreshness {
		return nil, false
	}
	context := observation.Context
	return &context, true
}
