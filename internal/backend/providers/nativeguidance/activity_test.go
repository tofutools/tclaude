package nativeguidance

import (
	"context"
	"testing"
	"time"

	"github.com/tofutools/tclaude/internal/backend/ports"
)

type activityResponse struct{ status int }

func (r *activityResponse) Respond(_ context.Context, response ports.RawNativeCallbackResponse) (ports.EffectDisposition, error) {
	r.status = response.StatusCode
	return ports.EffectAccepted, nil
}

func TestActivityCallbackRetainsOriginalFreshnessAndResets(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	runtime := &Runtime{Correlation: func(s string) bool { return s == "primary" }, Now: func() time.Time { return now }}
	state, at := runtime.ActivityObservation()
	if state != ports.AgentActivityUnknown || !at.IsZero() {
		t.Fatal("new runtime manufactured activity")
	}
	handler := &CallbackHandler{Normalize: func(raw ports.RawNativeCallback) (ports.NormalizedNativeEvent, string, error) {
		return ports.NormalizedNativeEvent{Kind: "activity_idle", NativeCorrelation: "primary", ObservedAt: raw.ReceivedAt}, "Notification", nil
	}, Encode: func(string, string) ([]byte, error) { t.Fatal("activity generated guidance"); return nil, nil }}
	if err := handler.Bind(runtime); err != nil {
		t.Fatal(err)
	}
	response := &activityResponse{}
	if err := handler.HandleNativeCallback(context.Background(), ports.RawNativeCallback{ReceivedAt: now}, response); err != nil {
		t.Fatal(err)
	}
	if response.status != 204 {
		t.Fatalf("unexpected response %d", response.status)
	}
	original := now
	now = now.Add(10 * time.Minute)
	state, at = runtime.ActivityObservation()
	if state != ports.AgentActivityIdle || !at.Equal(original) {
		t.Fatal("poll refreshed old native activity")
	}
	runtime.InvalidateActivity()
	state, at = runtime.ActivityObservation()
	if state != ports.AgentActivityUnknown || !at.Equal(now) {
		t.Fatal("input did not invalidate old activity")
	}
	runtime.observeActivity(ports.NormalizedNativeEvent{Kind: "activity_idle", NativeCorrelation: "primary", ObservedAt: original})
	state, _ = runtime.ActivityObservation()
	if state != ports.AgentActivityUnknown {
		t.Fatal("out-of-order callback restored stale idle")
	}
	runtime.observeActivity(ports.NormalizedNativeEvent{Kind: "activity_idle", NativeCorrelation: "other", ObservedAt: now})
	state, _ = runtime.ActivityObservation()
	if state != ports.AgentActivityUnknown {
		t.Fatal("other session changed activity")
	}
	runtime.observeActivity(ports.NormalizedNativeEvent{Kind: "activity_awaiting_input", NativeCorrelation: "primary", ObservedAt: now})
	state, _ = runtime.ActivityObservation()
	if state != ports.AgentActivityAwaitingInput {
		t.Fatal("native waiting observation missing")
	}
	runtime.observeActivity(ports.NormalizedNativeEvent{Kind: "user_prompt", NativeCorrelation: "primary", ObservedAt: now.Add(time.Second)})
	state, _ = runtime.ActivityObservation()
	if state != ports.AgentActivityActive {
		t.Fatal("new prompt did not break waiting episode")
	}
}
