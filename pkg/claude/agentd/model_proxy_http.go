package agentd

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/routebroker"
)

type modelWireResponse struct {
	w       io.Writer
	header  http.Header
	body    io.WriteCloser
	err     error
	started bool
}

func newModelWireResponse(w io.Writer) *modelWireResponse {
	return &modelWireResponse{w: w, header: make(http.Header)}
}
func (w *modelWireResponse) Header() http.Header { return w.header }
func (w *modelWireResponse) WriteHeader(status int) {
	if w.started {
		return
	}
	w.started = true
	_, w.err = fmt.Fprintf(w.w, "HTTP/1.1 %d %s\r\n", status, http.StatusText(status))
	h := w.header.Clone()
	h.Del("Content-Length")
	h.Set("Transfer-Encoding", "chunked")
	h.Set("Connection", "close")
	if w.err == nil {
		w.err = h.Write(w.w)
	}
	if w.err == nil {
		_, w.err = io.WriteString(w.w, "\r\n")
	}
	w.body = httputil.NewChunkedWriter(w.w)
}
func (w *modelWireResponse) Write(p []byte) (int, error) {
	if !w.started {
		w.WriteHeader(200)
	}
	if w.err != nil {
		return 0, w.err
	}
	return w.body.Write(p)
}
func (w *modelWireResponse) Flush() {} // Writes go straight to the encrypted stream.
func (w *modelWireResponse) finish() error {
	if !w.started {
		w.WriteHeader(500)
	}
	if w.err != nil {
		return w.err
	}
	if err := w.body.Close(); err != nil {
		return err
	}
	_, err := io.WriteString(w.w, "\r\n")
	return err
}

func relayModelRequest(w http.ResponseWriter, r *http.Request, conn *routebroker.FlowStream) {
	path := r.PathValue("path")
	if path == "" {
		path = strings.TrimPrefix(r.URL.Path, "/v1/models/request/")
	}
	if !modelEndpointAllowed(r.Method, "/"+path, r.URL.RawQuery) {
		modelError(w, 400, "model gateway supports only Messages, token counting and filtered model discovery")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<20))
	if err != nil {
		modelError(w, 413, "model gateway request exceeds byte limit")
		return
	}
	target := "http://model/" + path
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, bytes.NewReader(body))
	if err != nil {
		modelError(w, 400, "invalid model gateway request")
		return
	}
	copyModelRequestHeaders(req.Header, r.Header)
	if err = req.Write(conn); err != nil {
		modelError(w, 502, "model gateway stream interrupted before response")
		return
	}
	if err = conn.CloseWrite(); err != nil {
		modelError(w, 502, "model gateway stream interrupted before response")
		return
	}
	header, br, err := readModelHTTPHeader(conn)
	if err != nil {
		modelError(w, 502, "model gateway response headers are incomplete or too large")
		return
	}
	response, err := http.ReadResponse(bufio.NewReader(io.MultiReader(bytes.NewReader(header), br)), req)
	if err != nil {
		modelError(w, 502, "model gateway did not return a complete HTTP response")
		return
	}
	defer response.Body.Close()
	copyModelResponseHeaders(w.Header(), response.Header)
	w.WriteHeader(response.StatusCode)
	// Flush each read, including SSE pings; no full-response buffer or ordinary
	// HTTP proxy's 60-second timeout is involved.
	buffer := make([]byte, 32<<10)
	for {
		n, e := response.Body.Read(buffer)
		if n > 0 {
			if _, err = w.Write(buffer[:n]); err != nil {
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		if e != nil {
			return
		}
	}
}
func modelEndpointAllowed(method, path, query string) bool {
	if query != "" && query != "beta=true" {
		return false
	}
	return method == http.MethodPost && (path == "/v1/messages" || path == "/v1/messages/count_tokens" || path == "/v1/responses") || method == http.MethodGet && path == "/v1/models"
}
func copyModelRequestHeaders(dst, src http.Header) {
	// The launch bearer, user cookies, OAuth credentials and arbitrary routing
	// headers never cross the fleet or reach the operator-pinned provider.
	for _, name := range []string{"Content-Type", "Accept", "Anthropic-Version", "Anthropic-Beta"} {
		if values := src.Values(name); len(values) > 0 {
			dst[name] = append([]string(nil), values...)
		}
	}
}
func copyModelResponseHeaders(dst, src http.Header) {
	for name, values := range src {
		lower := strings.ToLower(name)
		if lower == "content-type" || lower == "request-id" || lower == "x-request-id" || strings.HasPrefix(lower, "x-ratelimit-") || lower == "retry-after" || lower == "x-should-retry" || strings.HasPrefix(lower, "anthropic-ratelimit-") {
			dst[name] = append([]string(nil), values...)
		}
	}
	dst.Set("Cache-Control", "no-store")
}
func modelByteBounds(p *config.ModelProxyPolicy) (request, response int64, event int) {
	request = p.MaxRequestBytes
	if request == 0 {
		request = 4 << 20
	}
	response = p.MaxResponseBytes
	if response == 0 {
		response = 64 << 20
	}
	event = p.MaxEventBytes
	if event == 0 {
		event = 1 << 20
	}
	return
}
func serveModelUpstream(w http.ResponseWriter, r *http.Request, peer, session, name, id string) {
	w = modelDialectWriter{w, r.URL.Path == "/v1/responses"}
	instance, err := modelProxyPolicy(name)
	if err != nil {
		modelError(w, 503, err.Error())
		return
	}
	w = modelDialectWriter{w, instance.ModelPolicy.Dialect == "openai"}
	if !fedPeerModelAllows(peer, name) {
		modelError(w, 403, "models.proxy grant was revoked for this named gateway")
		return
	}
	if r.URL.IsAbs() || r.URL.Host != "" || !modelEndpointAllowed(r.Method, r.URL.EscapedPath(), r.URL.RawQuery) {
		modelError(w, 400, "unsupported model gateway endpoint")
		return
	}
	p := instance.ModelPolicy
	openai := p.Dialect == "openai"
	if (openai && r.URL.RawQuery != "") || (r.Method == http.MethodPost && (openai != (r.URL.Path == "/v1/responses"))) {
		modelError(w, 400, "endpoint does not match this gateway's configured dialect")
		return
	}
	if r.Method == http.MethodGet {
		data := []map[string]string{}
		for _, model := range p.Models {
			if strings.HasSuffix(model, "*") {
				continue // Patterns authorize requests, but are not concrete model IDs.
			}
			data = append(data, map[string]string{"type": "model", "id": model, "display_name": model, "created_at": "1970-01-01T00:00:00Z"})
		}
		if openai {
			models := []map[string]any{}
			for _, model := range p.Models {
				if !strings.HasSuffix(model, "*") {
					models = append(models, map[string]any{"id": model, "object": "model", "created": 0, "owned_by": "gateway"})
				}
			}
			writeJSON(w, 200, map[string]any{"object": "list", "data": models})
			return
		}
		first, last := "", ""
		if len(data) > 0 {
			first, last = data[0]["id"], data[len(data)-1]["id"]
		}
		writeJSON(w, 200, map[string]any{"data": data, "has_more": false, "first_id": first, "last_id": last})
		return
	}
	requestCap, responseCap, eventCap := modelByteBounds(p)
	body, err := io.ReadAll(io.LimitReader(r.Body, requestCap+1))
	if err != nil || int64(len(body)) > requestCap {
		modelError(w, 413, "model gateway request exceeds configured byte limit")
		return
	}
	var data struct {
		Model     string `json:"model"`
		MaxTokens int64  `json:"max_tokens"`
		Stream    bool   `json:"stream"`
	}
	if json.Unmarshal(body, &data) != nil {
		modelError(w, 400, "model gateway request must be a JSON object")
		return
	}
	allowed := false
	for _, m := range p.Models {
		if m == data.Model || strings.HasSuffix(m, "*") && strings.HasPrefix(data.Model, strings.TrimSuffix(m, "*")) {
			allowed = true
		}
	}
	if !allowed {
		modelError(w, 403, "model is not in this gateway's allowlist")
		return
	}
	if openai {
		var fields map[string]json.RawMessage
		if json.Unmarshal(body, &fields) != nil || fields == nil {
			modelError(w, 400, "Responses request must be an object")
			return
		}
		if raw, exists := fields["max_output_tokens"]; exists {
			if string(raw) == "null" || json.Unmarshal(raw, &data.MaxTokens) != nil {
				modelError(w, 400, "max_output_tokens must be a positive integer")
				return
			}
		} else {
			data.MaxTokens = p.MaxOutputTokens
			fields["max_output_tokens"], _ = json.Marshal(data.MaxTokens)
			body, _ = json.Marshal(fields)
		}
	}
	if int64(len(body)) > requestCap {
		modelError(w, 413, "model gateway bounded request exceeds byte limit")
		return
	}
	counting := r.URL.Path == "/v1/messages/count_tokens"
	if !counting && (data.MaxTokens < 1 || data.MaxTokens > p.MaxOutputTokens) {
		modelError(w, 400, "max_tokens exceeds this gateway's configured output bound")
		return
	}
	if counting {
		data.MaxTokens = 0
		data.Stream = false
	}
	u := db.ModelProxyUsage{ID: id, Day: time.Now().UTC().Format("2006-01-02"), Proxy: name, Peer: peer, Session: session, Model: data.Model, ChargedTokens: p.MaxInputTokens + data.MaxTokens, RequestBytes: int64(len(body))}
	budget := db.ModelProxyBudget{Requests: p.DailyRequests, Tokens: p.DailyTokens, PeerRequests: p.PeerDailyRequests, PeerTokens: p.PeerDailyTokens, SessionRequests: p.SessionDailyRequests, SessionTokens: p.SessionDailyTokens}
	if err = db.ReserveModelProxyRequest(u, budget); err != nil {
		modelError(w, 429, "model gateway daily request or token budget exhausted; incomplete requests remain charged until the next UTC day")
		return
	}
	started := time.Now()
	defer func() {
		u.ChargedTokens = max(u.ChargedTokens, u.InputTokens+u.OutputTokens+u.CacheReadTokens+u.CacheWriteTokens)
		u.DurationMS = time.Since(started).Milliseconds()
		_ = db.FinishModelProxyRequest(u)
		recordFederationAudit("models.proxy.request", peer, "", name, fmt.Sprintf("request=%s session=%s model=%s status=%d complete=%t charged_tokens=%d", id, session, data.Model, u.Status, u.Complete, u.ChargedTokens), u.Status)
	}()
	credential, err := httpProxyCredential(instance)
	if err != nil {
		u.Status = 503
		modelError(w, 503, "model gateway provider credential is unavailable")
		return
	}
	target, err := httpProxyURL(instance.URL, r.URL.RequestURI())
	if err != nil {
		u.Status = 400
		modelError(w, 400, "invalid model gateway path")
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), bytes.NewReader(body))
	if err != nil {
		u.Status = 400
		modelError(w, 400, "invalid model gateway request")
		return
	}
	copyModelRequestHeaders(req.Header, r.Header)
	req.Header.Set(instance.Header, credential)
	client := modelUpstreamClient(name, instance.URL)
	if !counting && p.PrecountInput {
		countBody, e := modelCountBody(body)
		if e != nil {
			u.Status = 400
			modelError(w, 400, "model gateway could not prepare token counting")
			return
		}
		countURL, e := httpProxyURL(instance.URL, "/v1/messages/count_tokens")
		if e != nil {
			u.Status = 502
			modelError(w, 502, "model gateway token counting unavailable")
			return
		}
		countReq, e := http.NewRequestWithContext(r.Context(), http.MethodPost, countURL.String(), bytes.NewReader(countBody))
		if e != nil {
			u.Status = 502
			modelError(w, 502, "model gateway token counting unavailable")
			return
		}
		copyModelRequestHeaders(countReq.Header, r.Header)
		countReq.Header.Set(instance.Header, credential)
		countResp, e := client.Do(countReq)
		if e != nil {
			u.Status = 502
			modelError(w, 502, "model gateway requires provider token counting before generation")
			return
		}
		if countResp.StatusCode < 200 || countResp.StatusCode >= 300 {
			u.Status = countResp.StatusCode
			writeModelProviderError(w, countResp, credential)
			_ = countResp.Body.Close()
			return
		}
		countResult, e := io.ReadAll(io.LimitReader(countResp.Body, 32<<10))
		_ = countResp.Body.Close()
		var count struct {
			InputTokens *int64 `json:"input_tokens"`
		}
		if e != nil || countResp.StatusCode != 200 || json.Unmarshal(countResult, &count) != nil || count.InputTokens == nil || *count.InputTokens < 0 {
			u.Status = 502
			modelError(w, 502, "model gateway requires provider token counting before generation")
			return
		}
		if *count.InputTokens > p.MaxInputTokens {
			u.Status = 413
			modelError(w, 413, "input tokens exceed this gateway's configured bound")
			return
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		u.Status = 502
		modelError(w, 502, "model gateway provider is unavailable or the request was interrupted")
		return
	}
	defer resp.Body.Close()
	u.Status = resp.StatusCode
	// Preserve the provider signals Claude uses for retry and compaction, while
	// discarding unrecognized fields and credential reflections.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		writeModelProviderError(w, resp, credential)
		return
	}
	copyModelResponseHeaders(w.Header(), resp.Header)
	if data.Stream {
		if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
			u.Status = 502
			modelError(w, 502, "model gateway provider did not return an event stream")
			return
		}
		observer := modelUsageObserver{usage: &u, maxInput: p.MaxInputTokens, maxOutput: data.MaxTokens, credential: credential, openai: openai}
		w.WriteHeader(resp.StatusCode)
		err = relayModelEvents(w, resp.Body, responseCap, eventCap, &observer)
		if err != nil {
			u.Status = 502
			if openai {
				_, _ = io.WriteString(w, "event: error\ndata: {\"type\":\"error\",\"code\":\"gateway_error\",\"message\":\"model gateway stream interrupted or exceeded its bounds\"}\n\n")
			} else {
				_, _ = io.WriteString(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"model gateway stream interrupted or exceeded its bounds\"}}\n\n")
			}
		}
		u.Complete = err == nil && observer.stopped && observer.sawInput && observer.sawOutput && !observer.invalid
	} else {
		result, err := io.ReadAll(io.LimitReader(resp.Body, responseCap+1))
		u.ResponseBytes = int64(len(result))
		if err != nil || u.ResponseBytes > responseCap {
			u.Status = 502
			modelError(w, 502, "model gateway provider response is incomplete or exceeds the byte limit")
			return
		}
		var resultData struct {
			Usage       json.RawMessage `json:"usage"`
			InputTokens *int64          `json:"input_tokens"`
			Type        string          `json:"type"`
			Object      string          `json:"object"`
			Status      string          `json:"status"`
		}
		observer := modelUsageObserver{usage: &u, maxInput: p.MaxInputTokens, maxOutput: data.MaxTokens, credential: credential, openai: openai}
		if json.Unmarshal(result, &resultData) == nil {
			if counting && resultData.InputTokens != nil {
				observer.apply(map[string]*int64{"input_tokens": resultData.InputTokens})
				observer.sawOutput = true
				u.Complete = !observer.invalid
			}
			if !counting && ((!openai && resultData.Type == "message") || (openai && resultData.Object == "response" && resultData.Status == "completed")) {
				observer.parse(resultData.Usage)
				u.Complete = observer.sawInput && observer.sawOutput && !observer.invalid
			}
		}
		if observer.outOfBounds() || reflectsModelCredential(result, credential) {
			u.Status = 502
			u.Complete = false
			modelError(w, 502, "model gateway provider response violates configured bounds")
			return
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(result)
	}
	if u.Complete {
		u.ChargedTokens = u.InputTokens + u.OutputTokens + u.CacheReadTokens + u.CacheWriteTokens
	}
}

type modelUsageObserver struct {
	openai                                bool
	usage                                 *db.ModelProxyUsage
	maxInput, maxOutput                   int64
	credential                            string
	sawInput, sawOutput, stopped, invalid bool
}

func (o *modelUsageObserver) apply(values map[string]*int64) {
	fields := map[string]*int64{"input_tokens": &o.usage.InputTokens, "output_tokens": &o.usage.OutputTokens, "cache_read_input_tokens": &o.usage.CacheReadTokens, "cache_creation_input_tokens": &o.usage.CacheWriteTokens}
	for name, target := range fields {
		if v := values[name]; v != nil {
			if *v < 0 || *v > 1000000000 {
				o.invalid = true
				continue
			}
			*target = *v
			if name == "input_tokens" {
				o.sawInput = true
			}
			if name == "output_tokens" {
				o.sawOutput = true
			}
		}
	}
}
func (o *modelUsageObserver) parse(data json.RawMessage) {
	var values map[string]*int64
	if o.openai {
		var v struct {
			Input  *int64 `json:"input_tokens"`
			Output *int64 `json:"output_tokens"`
		}
		if json.Unmarshal(data, &v) == nil {
			o.apply(map[string]*int64{"input_tokens": v.Input, "output_tokens": v.Output})
		}
		return
	}
	if json.Unmarshal(data, &values) == nil {
		o.apply(values)
	}
}
func (o *modelUsageObserver) event(raw []byte) {
	var data []byte
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		line = bytes.TrimSuffix(line, []byte{'\r'})
		if bytes.HasPrefix(line, []byte("data:")) {
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, bytes.TrimPrefix(line[5:], []byte{' '})...)
		}
	}
	var event struct {
		Type     string          `json:"type"`
		Usage    json.RawMessage `json:"usage"`
		Response struct {
			Usage  json.RawMessage `json:"usage"`
			Status string          `json:"status"`
		} `json:"response"`
		Message struct {
			Usage json.RawMessage `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(data, &event) != nil {
		return
	}
	if o.openai {
		switch event.Type {
		case "response.completed":
			if o.stopped || event.Response.Status != "completed" {
				o.invalid = true
				return
			}
			o.parse(event.Response.Usage)
			o.stopped = true
		case "error", "response.failed", "response.incomplete":
			o.invalid = true
		}
		return
	}
	switch event.Type {
	case "message_start":
		o.parse(event.Message.Usage)
	case "message_delta":
		o.parse(event.Usage)
	case "message_stop":
		o.stopped = true
	case "error":
		o.invalid = true
	}
}
func relayModelEvents(w http.ResponseWriter, r io.Reader, limit int64, eventLimit int, observer *modelUsageObserver) error {
	br := bufio.NewReaderSize(r, 32<<10)
	event := make([]byte, 0, 4096)
	for {
		line, err := br.ReadSlice('\n')
		if len(event)+len(line) > eventLimit {
			return errors.New("model SSE event limit")
		}
		event = append(event, line...)
		if err == bufio.ErrBufferFull {
			continue
		}
		if len(line) == 1 && line[0] == '\n' || len(line) == 2 && line[0] == '\r' && line[1] == '\n' {
			observer.usage.ResponseBytes += int64(len(event))
			if observer.usage.ResponseBytes > limit {
				return errors.New("model response byte limit")
			}
			observer.event(event)
			if observer.outOfBounds() || reflectsModelCredential(event, observer.credential) {
				return errors.New("model response violates bounds")
			}
			if _, e := w.Write(event); e != nil {
				return e
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			event = event[:0]
		}
		if err != nil {
			if err == io.EOF && len(event) == 0 {
				return nil
			}
			return err
		}
	}
}

// Preserve unknown input fields so new provider features cannot bypass token
// counting. A provider that cannot count them must refuse before generation.
func modelCountBody(body []byte) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, err
	}
	for _, key := range []string{"max_tokens", "stream", "temperature", "top_p", "top_k", "stop_sequences", "metadata", "service_tier"} {
		delete(fields, key)
	}
	return json.Marshal(fields)
}
func (o *modelUsageObserver) outOfBounds() bool {
	return o.invalid || o.usage.InputTokens+o.usage.CacheReadTokens+o.usage.CacheWriteTokens > o.maxInput || o.usage.OutputTokens > o.maxOutput
}
func reflectsModelCredential(body []byte, credential string) bool {
	if credential == "" {
		return false
	}
	for _, value := range []string{credential, strings.TrimPrefix(credential, "Bearer ")} {
		if value == "" {
			continue
		}
		encoded, _ := json.Marshal(value)
		if bytes.Contains(body, []byte(value)) || bytes.Contains(body, encoded[1:len(encoded)-1]) {
			return true
		}
	}
	return false
}

var modelUpstreamClients = struct {
	sync.Mutex
	entries map[string]modelUpstreamEntry
}{entries: make(map[string]modelUpstreamEntry)}

type modelUpstreamEntry struct {
	url    string
	client *http.Client
}

func modelUpstreamClient(name, url string) *http.Client {
	modelUpstreamClients.Lock()
	defer modelUpstreamClients.Unlock()
	if entry, ok := modelUpstreamClients.entries[name]; ok {
		if entry.url == url {
			return entry.client
		}
		entry.client.CloseIdleConnections()
	}
	transport := &http.Transport{ResponseHeaderTimeout: 30 * time.Second, MaxResponseHeaderBytes: 32 << 10, DisableCompression: true, IdleConnTimeout: 90 * time.Second, MaxIdleConns: 32, MaxIdleConnsPerHost: 16}
	client := &http.Client{Transport: transport, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	modelUpstreamClients.entries[name] = modelUpstreamEntry{url: url, client: client}
	return client
}

func writeModelProviderError(w http.ResponseWriter, resp *http.Response, credential string) {
	copyModelResponseHeaders(w.Header(), resp.Header)
	const bound = 32 << 10
	body, err := io.ReadAll(io.LimitReader(resp.Body, bound+1))
	var result struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err != nil || len(body) > bound || json.Unmarshal(body, &result) != nil || (result.Type != "error" && !modelWriterOpenAI(w)) || result.Error.Type == "" || len(result.Error.Type) > 128 || result.Error.Message == "" || reflectsModelCredential(body, credential) {
		modelError(w, resp.StatusCode, "model gateway provider refused the request")
		return
	}
	if len(result.Error.Message) > 2048 {
		result.Error.Message = result.Error.Message[:2048]
	}
	copyModelResponseHeaders(w.Header(), resp.Header)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	if modelWriterOpenAI(w) {
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"type": result.Error.Type, "message": result.Error.Message, "param": nil, "code": nil}})
	} else {
		_ = json.NewEncoder(w).Encode(result)
	}
}
