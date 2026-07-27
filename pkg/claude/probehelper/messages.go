package probehelper

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

type messagesHandler struct {
	rootFD  int
	secret  string
	command string
	marker  string
	success string
}

func (handler *messagesHandler) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	if !strings.HasPrefix(request.URL.Path, "/"+handler.secret+"/") {
		http.NotFound(w, request)
		return
	}
	switch request.Method {
	case http.MethodHead:
		w.Header().Set("content-length", "0")
		w.WriteHeader(http.StatusOK)
	case http.MethodPost:
		handler.post(w, request)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (handler *messagesHandler) post(w http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(w, request.Body, maxMessagesRequestBytes)
	defer request.Body.Close()
	var body map[string]any
	decoder := json.NewDecoder(request.Body)
	if err := decoder.Decode(&body); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	results := collectToolResults(body)
	var content []map[string]any
	stopReason := "tool_use"
	if len(results) == 0 {
		content = []map[string]any{{
			"type": "tool_use",
			"id":   "toolu_tclaude_stacked",
			"name": "Bash",
			"input": map[string]any{
				"command": handler.command,
			},
		}}
	} else {
		valid := false
		for _, result := range results {
			isError, _ := result["is_error"].(bool)
			if !isError && strings.Contains(fmt.Sprint(result["content"]), handler.marker) {
				valid = true
				break
			}
		}
		text := "probe refused"
		if valid {
			text = handler.success
		} else {
			_ = publishAt(
				handler.rootFD,
				InnerPolicyFileName,
				InnerPolicyFailureValue,
				0o600,
			)
		}
		content = []map[string]any{{"type": "text", "text": text}}
		stopReason = "end_turn"
	}
	response := map[string]any{
		"id":            "msg_tclaude_stacked",
		"type":          "message",
		"role":          "assistant",
		"model":         "claude-sonnet-4-5-20250929",
		"content":       content,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage":         map[string]any{"input_tokens": 1, "output_tokens": 1},
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		http.Error(w, "encode response", http.StatusInternalServerError)
		return
	}
	w.Header().Set("content-type", "application/json")
	w.Header().Set("content-length", fmt.Sprint(len(encoded)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encoded)
}

func collectToolResults(body map[string]any) []map[string]any {
	messages, _ := body["messages"].([]any)
	var results []map[string]any
	for _, rawMessage := range messages {
		message, _ := rawMessage.(map[string]any)
		blocks, _ := message["content"].([]any)
		for _, rawBlock := range blocks {
			block, _ := rawBlock.(map[string]any)
			if block["type"] == "tool_result" {
				results = append(results, block)
			}
		}
	}
	return results
}
