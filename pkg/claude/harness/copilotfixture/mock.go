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
// sanitizer converts it into the committable form. Body is nil when the
// request carried no decodable JSON, which is itself recorded rather than
// dropped.
type RecordedRequest struct {
	Method string
	Path   string
	Header http.Header
	Body   map[string]any
}

// overrunText answers a request past the end of a scenario's script.
const overrunText = "MOCK SCENARIO OVERRUN"

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
	// Recorded BEFORE the body is decoded and before any routing decision, for
	// every method and every path. Both halves matter: the route the CLI picks
	// is itself part of the contract under observation (completions posts to
	// /chat/completions, responses posts elsewhere), and a request the mock
	// cannot parse — a GET capability probe, a health check, a future
	// non-JSON body — is exactly the kind of new behavior a fixture must
	// surface. Dropping it here would let the suite report "no change" while
	// the CLI had started doing something new.
	var body map[string]any
	decodeErr := json.NewDecoder(r.Body).Decode(&body)

	m.mu.Lock()
	idx := len(m.requests)
	m.requests = append(m.requests, RecordedRequest{
		Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(), Body: body,
	})
	turn := m.turns[min(idx, len(m.turns)-1)]
	m.mu.Unlock()

	if decodeErr != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}

	// Past the end of the script, never answer with a tool call: the CLI would
	// execute it, ask again, and loop until RunTimeout. Degrading to a
	// terminal text answer turns an over-called scenario into a fast, visible
	// assertion failure on the recorded request count instead of a 90s hang.
	if idx >= len(m.turns) {
		turn = Turn{Text: overrunText}
	}

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
	stream, _ := body["stream"].(bool)

	// The two wires are distinguished by route and by body shape: completions
	// posts messages[] to /chat/completions, responses posts input[] plus a
	// separate instructions field to /responses. Their SSE framings are
	// unrelated, so answering one with the other's shape reads to the CLI as a
	// transient failure and triggers its retry schedule.
	if isResponsesWire(r.URL.Path, body) {
		m.writeResponsesStream(w, model, turn)
		return
	}
	// The CLI sends stream:true by default and treats a plain JSON answer to a
	// streaming request as a transient failure. Honour whichever mode it asked for.
	if stream {
		m.writeStream(w, model, turn)
		return
	}
	m.writeBlocking(w, model, turn)
}

func isResponsesWire(path string, body map[string]any) bool {
	if strings.HasSuffix(path, "/responses") {
		return true
	}
	_, hasInput := body["input"]
	return hasInput
}

// writeResponsesStream emits the OpenAI Responses SSE sequence, which the CLI
// accepts on COPILOT_PROVIDER_WIRE_API=responses.
//
// Two framing details differ from the completions wire and are load-bearing:
// each event carries a named `event:` line matching its `type`, and the stream
// ends at response.completed with NO `data: [DONE]` sentinel.
func (m *MockProvider) writeResponsesStream(w http.ResponseWriter, model string, turn Turn) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	seq := 0
	send := func(eventType string, payload map[string]any) {
		payload["type"] = eventType
		payload["sequence_number"] = seq
		seq++
		enc, err := json.Marshal(payload)
		if err != nil {
			return
		}
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, enc)
		if flusher != nil {
			flusher.Flush()
		}
	}
	response := func(status string, output []any) map[string]any {
		return map[string]any{
			"id": responseID, "object": "response", "created_at": 0,
			"status": status, "model": model, "output": output,
		}
	}

	send("response.created", map[string]any{"response": response("in_progress", []any{})})
	send("response.in_progress", map[string]any{"response": response("in_progress", []any{})})
	send("response.output_item.added", map[string]any{
		"output_index": 0,
		"item": map[string]any{
			"id": messageID, "type": "message", "status": "in_progress",
			"role": "assistant", "content": []any{},
		},
	})
	send("response.content_part.added", map[string]any{
		"item_id": messageID, "output_index": 0, "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
	})
	for _, piece := range splitForStreaming(turn.Text) {
		send("response.output_text.delta", map[string]any{
			"item_id": messageID, "output_index": 0, "content_index": 0, "delta": piece,
		})
	}
	send("response.output_text.done", map[string]any{
		"item_id": messageID, "output_index": 0, "content_index": 0, "text": turn.Text,
	})
	send("response.content_part.done", map[string]any{
		"item_id": messageID, "output_index": 0, "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": turn.Text, "annotations": []any{}},
	})

	item := map[string]any{
		"id": messageID, "type": "message", "status": "completed", "role": "assistant",
		"content": []any{map[string]any{
			"type": "output_text", "text": turn.Text, "annotations": []any{},
		}},
	}
	send("response.output_item.done", map[string]any{"output_index": 0, "item": item})

	completed := response("completed", []any{item})
	completed["usage"] = map[string]any{
		"input_tokens": 11, "output_tokens": 3, "total_tokens": 14,
	}
	send("response.completed", map[string]any{"response": completed})
	// Deliberately no `data: [DONE]`: the Responses wire terminates at
	// response.completed, unlike the completions wire.
}

// chunkID is fixed so a fixture can pin the CLI's apiCallId (the event stream
// echoes the SSE chunk id straight through) without a normalization rule.
const chunkID = "chatcmpl-copilotfixture"

// Responses-wire ids, fixed for the same reason as chunkID.
const (
	responseID = "resp_copilotfixture"
	messageID  = "msg_copilotfixture"
)

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
