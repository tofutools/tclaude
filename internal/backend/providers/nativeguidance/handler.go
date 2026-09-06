package nativeguidance

import (
	"context"
	"fmt"
	"sync"

	"github.com/tofutools/tclaude/internal/backend/ports"
)

type NormalizeFunc func(ports.RawNativeCallback) (ports.NormalizedNativeEvent, string, error)
type EncodeFunc func(string, string) ([]byte, error)

// CallbackHandler parses only its provider's native schema. It is registered
// during preparation and bound to the cohesive runtime before workload release.
type CallbackHandler struct {
	Normalize NormalizeFunc
	Encode    EncodeFunc

	mu      sync.RWMutex
	runtime *Runtime
}

func (h *CallbackHandler) Bind(runtime *Runtime) error {
	if runtime == nil {
		return fmt.Errorf("native guidance runtime is required")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.runtime == runtime {
		return nil
	}
	if h.runtime != nil {
		return fmt.Errorf("native guidance handler is already bound")
	}
	h.runtime = runtime
	return nil
}

func (h *CallbackHandler) HandleNativeCallback(ctx context.Context, raw ports.RawNativeCallback, sink ports.RawNativeCallbackResponder) error {
	h.mu.RLock()
	runtime := h.runtime
	h.mu.RUnlock()
	if runtime == nil {
		return fmt.Errorf("native guidance runtime is not released")
	}
	if h.Normalize == nil || h.Encode == nil || sink == nil {
		return fmt.Errorf("native guidance callback codec or response sink is incomplete")
	}
	event, nativeKind, err := h.Normalize(raw)
	if err != nil {
		return err
	}
	responder := &callbackResponder{nativeKind: nativeKind, encode: h.Encode, sink: sink}
	settlement, err := runtime.HandleNativeEvent(ctx, event, responder)
	if err != nil {
		return err
	}
	if settlement.Disposition == ports.EffectRefused || settlement.Disposition == ports.EffectUnsupported {
		_, err = sink.Respond(ctx, ports.RawNativeCallbackResponse{StatusCode: 204})
		return err
	}
	if !responder.responded {
		return fmt.Errorf("native guidance response was not produced")
	}
	return nil
}

type callbackResponder struct {
	nativeKind string
	encode     EncodeFunc
	sink       ports.RawNativeCallbackResponder
	responded  bool
}

func (r *callbackResponder) RespondNativeGuidance(ctx context.Context, guidance string) (ports.EffectDisposition, error) {
	body, err := r.encode(r.nativeKind, guidance)
	if err != nil {
		return ports.EffectRefused, err
	}
	disposition, err := r.sink.Respond(ctx, ports.RawNativeCallbackResponse{StatusCode: 200, ContentType: "application/json", Body: body})
	r.responded = true
	return disposition, err
}
