package nativeguidance

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

func TestRuntimeConsumesImmediatelyBeforeResponseAndSettlesAfter(t *testing.T) {
	order := []string{}
	permit := &testPermit{order: &order}
	evaluator := &testEvaluator{order: &order, admission: ports.NativeGuidanceAdmission{
		IssuanceID: "issuance-1", Guidance: "stay focused", Deadline: time.Now().Add(time.Minute), Permit: permit,
	}}
	runtime := Runtime{Evaluator: evaluator, Evidence: func() (model.ProviderEvidence, error) {
		return model.NewProviderEvidence("test", 1, []byte(`{"exact":true}`))
	}, Kinds: map[string]struct{}{"session_start": {}}, Correlation: func(value string) bool { return value == "native-1" }}
	settlement, err := runtime.HandleNativeEvent(context.Background(), ports.NormalizedNativeEvent{
		EventID: "event-1", Kind: "session_start", ObservedAt: time.Now(), NativeCorrelation: "native-1",
		Payload: []byte(`{"session_id":"native-1"}`), Timing: model.StandingOrderSameContinuation,
	}, testGuidanceResponder{order: &order})
	require.NoError(t, err)
	require.Equal(t, ports.EffectAccepted, settlement.Disposition)
	require.Equal(t, []string{"evaluate", "consume", "respond", "settle"}, order)
}

func TestRuntimeDoesNotRespondWhenPermitFails(t *testing.T) {
	order := []string{}
	evaluator := &testEvaluator{order: &order, admission: ports.NativeGuidanceAdmission{
		IssuanceID: "issuance-1", Guidance: "guidance", Deadline: time.Now().Add(time.Minute),
		Permit: &testPermit{order: &order, err: errors.New("stale")},
	}}
	runtime := Runtime{Evaluator: evaluator, Kinds: map[string]struct{}{"user_prompt": {}}, Correlation: func(value string) bool { return value == "native" }}
	settlement, err := runtime.HandleNativeEvent(context.Background(), ports.NormalizedNativeEvent{
		EventID: "event", Kind: "user_prompt", ObservedAt: time.Now(), NativeCorrelation: "native",
		Payload: []byte(`{}`), Timing: model.StandingOrderSameContinuation,
	}, testGuidanceResponder{order: &order})
	require.Error(t, err)
	require.Equal(t, ports.EffectRefused, settlement.Disposition)
	require.Equal(t, []string{"evaluate", "consume"}, order)
}

func TestCallbackHandlerSettlesAfterRawResponseSink(t *testing.T) {
	order := []string{}
	evaluator := &testEvaluator{order: &order, admission: ports.NativeGuidanceAdmission{
		IssuanceID: "issuance-1", Guidance: "native", Deadline: time.Now().Add(time.Minute),
		Permit: &testPermit{order: &order},
	}}
	runtime := &Runtime{Evaluator: evaluator, Evidence: func() (model.ProviderEvidence, error) {
		return model.NewProviderEvidence("test", 1, []byte(`{}`))
	}, Kinds: map[string]struct{}{"session_start": {}}, Correlation: func(value string) bool { return value == "native" }}
	handler := &CallbackHandler{
		Normalize: func(raw ports.RawNativeCallback) (ports.NormalizedNativeEvent, string, error) {
			return ports.NormalizedNativeEvent{EventID: "event", Kind: "session_start", ObservedAt: raw.ReceivedAt, NativeCorrelation: "native", Payload: raw.Body, Timing: model.StandingOrderSameContinuation}, "SessionStart", nil
		},
		Encode: func(_, guidance string) ([]byte, error) { return []byte(guidance), nil },
	}
	require.NoError(t, handler.Bind(runtime))
	require.NoError(t, handler.HandleNativeCallback(context.Background(), ports.RawNativeCallback{Body: []byte(`{}`), ReceivedAt: time.Now()}, testRawResponder{order: &order}))
	require.Equal(t, []string{"evaluate", "consume", "raw_response", "settle"}, order)
}

type testEvaluator struct {
	order     *[]string
	admission ports.NativeGuidanceAdmission
}

func (e *testEvaluator) EvaluateNativeGuidance(context.Context, ports.NormalizedNativeEvent) (ports.NativeGuidanceAdmission, error) {
	*e.order = append(*e.order, "evaluate")
	return e.admission, nil
}
func (e *testEvaluator) SettleNativeGuidance(_ context.Context, _ ports.NativeGuidanceSettlement) error {
	*e.order = append(*e.order, "settle")
	return nil
}

type testPermit struct {
	order *[]string
	err   error
}

func (*testPermit) IssuanceID() model.WorkIssuanceID { return "issuance-1" }
func (*testPermit) OperationID() model.OperationID   { return "operation-1" }
func (p *testPermit) Consume(context.Context) error {
	*p.order = append(*p.order, "consume")
	return p.err
}

type testGuidanceResponder struct{ order *[]string }

func (r testGuidanceResponder) RespondNativeGuidance(context.Context, string) (ports.EffectDisposition, error) {
	*r.order = append(*r.order, "respond")
	return ports.EffectAccepted, nil
}

type testRawResponder struct{ order *[]string }

func (r testRawResponder) Respond(context.Context, ports.RawNativeCallbackResponse) (ports.EffectDisposition, error) {
	*r.order = append(*r.order, "raw_response")
	return ports.EffectAccepted, nil
}
