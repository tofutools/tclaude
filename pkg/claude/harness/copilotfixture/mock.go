package copilotfixture

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Turn scripts the mock's answer to one provider request. A scenario supplies
// one Turn per expected request; requests past the end of the script reuse the
// last Turn, so a CLI that asks one more time than predicted degrades into a
// visible assertion failure instead of a hang.
type Turn struct {
	// Text is streamed back as assistant content, split across several deltas
	// so the fixture also exercises incremental decoding rather than a single
	// whole-message chunk.
	Text string

	// ToolCall, when set, makes this turn answer with a function call instead
	// of text. The CLI executes the tool and issues a follow-up request, which
	// the next Turn answers.
	ToolCall *ToolCall

	// FailStatus, when non-zero, answers with that HTTP status and an
	// OpenAI-shaped error body instead of a completion.
	//
	// Use 400. Copilot retries on its own (not via the OpenAI SDK, whose
	// x-stainless-retry-count stays 0), and the observed cost is severe:
	// 400 fails fast with no retry, 401 retries ~3x, 500 retries 5x for ~30s
	// and 429 retries 5x for ~100s. A negative-path fixture on 500 or 429
	// would spend its entire runtime in backoff.
	FailStatus int
}

// ToolCall is the function call a Turn emits.
type ToolCall struct {
	ID   string
	Name string
	// Args is the raw JSON arguments object handed to the tool.
	Args string
}

// MockProvider is a deterministic OpenAI-compatible provider endpoint.
//
// It answers exactly one route, POST {base}/chat/completions, because that is
// the only route the CLI contacts on the completions wire — it never lists
// models. It requires no credential: with COPILOT_PROVIDER_API_KEY unset the
// CLI still sends an Authorization header, but with an empty bearer value, so
// a mock that enforced auth would reject the very path this suite proves is
// credential-free.
type MockProvider struct {
	server *httptest.Server

	mu       sync.Mutex
	turns    []Turn
	requests []RecordedRequest
}

// RecordedRequest is one observed provider request, kept raw here; the
// sanitizer converts it into the committable form.
type RecordedRequest struct {
	Path   string
	Header http.Header
	Body   map[string]any
}

// NewMockProvider starts a mock on loopback that replays turns in order. The
// server is closed via t.Cleanup.
func NewMockProvider(t *testing.T, turns []Turn) *MockProvider {
	t.Helper()
	if len(turns) == 0 {
		t.Fatal("copilotfixture: scenario needs at least one turn")
	}
	m := &MockProvider{turns: turns}
	m.server = httptest.NewServer(http.HandlerFunc(m.handle))
	t.Cleanup(m.server.Close)
	return m
}

// BaseURL is the value for COPILOT_PROVIDER_BASE_URL. The /v1 suffix matches
// the documented BYOK examples; the CLI appends /chat/completions to it.
func (m *MockProvider) BaseURL() string { return m.server.URL + "/v1" }

// Requests returns the provider requests observed so far.
func (m *MockProvider) Requests() []RecordedRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]RecordedRequest(nil), m.requests...)
}

func (m *MockProvider) handle(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}

	// Recorded BEFORE any routing decision, and for every path. The route the
	// CLI chooses is itself part of the contract under observation — the
	// completions wire posts to /chat/completions while the responses wire
	// posts elsewhere — so a mock that 404'd an unexpected path before
	// recording would hide exactly the evidence a wire fixture is after.
	m.mu.Lock()
	idx := len(m.requests)
	m.requests = append(m.requests, RecordedRequest{
		Path: r.URL.Path, Header: r.Header.Clone(), Body: body,
	})
	turn := m.turns[min(idx, len(m.turns)-1)]
	m.mu.Unlock()

	if turn.FailStatus != 0 {
		writeJSON(w, turn.FailStatus, map[string]any{
			"error": map[string]any{
				"message": "deterministic mock provider failure",
				"type":    "invalid_request_error",
				"code":    "copilotfixture_failure",
			},
		})
		return
	}

	model, _ := body["model"].(string)
	if model == "" {
		model = MockModel
	}
	// The CLI sends stream:true by default and treats a plain JSON answer to a
	// streaming request as a transient failure, which triggers the retry
	// storm. Honour whichever mode it actually asked for.
	if stream, _ := body["stream"].(bool); stream {
		m.writeStream(w, model, turn)
		return
	}
	m.writeBlocking(w, model, turn)
}

// chunkID is fixed so a fixture can pin the CLI's apiCallId (the event stream
// echoes the SSE chunk id straight through) without a normalization rule.
const chunkID = "chatcmpl-copilotfixture"

func (m *MockProvider) writeStream(w http.ResponseWriter, model string, turn Turn) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	send := func(payload map[string]any) {
		enc, err := json.Marshal(payload)
		if err != nil {
			return
		}
		fmt.Fprintf(w, "data: %s\n\n", enc)
		if flusher != nil {
			flusher.Flush()
		}
	}
	chunk := func(choices []any) map[string]any {
		return map[string]any{
			"id": chunkID, "object": "chat.completion.chunk",
			"created": 0, "model": model, "choices": choices,
		}
	}
	choice := func(delta map[string]any, finish any) []any {
		return []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}
	}

	if tc := turn.ToolCall; tc != nil {
		send(chunk(choice(map[string]any{
			"role": "assistant", "content": "",
			"tool_calls": []any{map[string]any{
				"index": 0, "id": tc.ID, "type": "function",
				"function": map[string]any{"name": tc.Name, "arguments": ""},
			}},
		}, nil)))
		// Arguments arrive as a separate delta, matching how real providers
		// stream them and exercising the CLI's accumulation path.
		send(chunk(choice(map[string]any{
			"tool_calls": []any{map[string]any{
				"index":    0,
				"function": map[string]any{"arguments": tc.Args},
			}},
		}, nil)))
		send(chunk(choice(map[string]any{}, "tool_calls")))
	} else {
		send(chunk(choice(map[string]any{"role": "assistant", "content": ""}, nil)))
		for _, piece := range splitForStreaming(turn.Text) {
			send(chunk(choice(map[string]any{"content": piece}, nil)))
		}
		send(chunk(choice(map[string]any{}, "stop")))
	}

	// A trailing usage-only chunk is what stream_options.include_usage asks
	// for, and it is what populates the CLI's token line. Fixed counts keep
	// the resulting event stream deterministic.
	usage := chunk(nil)
	usage["choices"] = []any{}
	usage["usage"] = mockUsage()
	send(usage)

	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

func (m *MockProvider) writeBlocking(w http.ResponseWriter, model string, turn Turn) {
	message := map[string]any{"role": "assistant", "content": turn.Text}
	finish := "stop"
	if tc := turn.ToolCall; tc != nil {
		message = map[string]any{
			"role": "assistant", "content": nil,
			"tool_calls": []any{map[string]any{
				"id": tc.ID, "type": "function",
				"function": map[string]any{"name": tc.Name, "arguments": tc.Args},
			}},
		}
		finish = "tool_calls"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": chunkID, "object": "chat.completion", "created": 0, "model": model,
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}},
		"usage":   mockUsage(),
	})
}

func mockUsage() map[string]any {
	return map[string]any{"prompt_tokens": 11, "completion_tokens": 3, "total_tokens": 14}
}

// splitForStreaming breaks text on spaces so multi-delta accumulation is
// exercised, while keeping the reassembled string byte-identical to the input.
func splitForStreaming(text string) []string {
	if text == "" {
		return nil
	}
	parts := strings.SplitAfter(text, " ")
	out := parts[:0]
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
