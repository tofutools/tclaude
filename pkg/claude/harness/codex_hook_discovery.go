package harness

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const codexHookDiscoveryTimeout = 10 * time.Second
const codexHookDiscoveryStderrLimit = 32 << 10

var discoverCodexHookTrustEntries = discoverCodexHookTrustEntriesViaAppServer

type codexHookRPCResponse struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type codexHooksListResult struct {
	Data []codexHooksListEntry `json:"data"`
}

type codexHooksListEntry struct {
	Hooks  []codexDiscoveredHook `json:"hooks"`
	Errors []codexHookListError  `json:"errors"`
}

type codexDiscoveredHook struct {
	Key         string  `json:"key"`
	EventName   string  `json:"eventName"`
	Command     *string `json:"command"`
	SourcePath  string  `json:"sourcePath"`
	CurrentHash string  `json:"currentHash"`
}

type codexHookListError struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

type codexHookDiscoveryStderr struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	remaining int
}

func newCodexHookDiscoveryStderr() *codexHookDiscoveryStderr {
	return &codexHookDiscoveryStderr{remaining: codexHookDiscoveryStderrLimit}
}

func (b *codexHookDiscoveryStderr) Write(p []byte) (int, error) {
	n := len(p)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.remaining > 0 {
		write := p
		if len(write) > b.remaining {
			write = write[:b.remaining]
		}
		_, _ = b.buf.Write(write)
		b.remaining -= len(write)
	}
	return n, nil
}

func (b *codexHookDiscoveryStderr) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

var codexHookEventRPCNames = map[string]string{
	"PreToolUse":        "preToolUse",
	"PermissionRequest": "permissionRequest",
	"PostToolUse":       "postToolUse",
	"PreCompact":        "preCompact",
	"PostCompact":       "postCompact",
	"SessionStart":      "sessionStart",
	"SessionEnd":        "sessionEnd",
	"UserPromptSubmit":  "userPromptSubmit",
	"SubagentStart":     "subagentStart",
	"SubagentStop":      "subagentStop",
	"Stop":              "stop",
}

func discoverCodexHookTrustEntriesViaAppServer(
	hooksPath, want string,
) ([]codexHookTrustEntry, error) {
	binary, err := exec.LookPath("codex")
	if err != nil {
		return nil, fmt.Errorf("locate Codex for authoritative hook trust: %w", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("resolve cwd for Codex hook discovery: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), codexHookDiscoveryTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "app-server", "--stdio")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("open Codex app-server stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("open Codex app-server stdout: %w", err)
	}
	stderr := newCodexHookDiscoveryStderr()
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start Codex app-server for hook discovery: %w", err)
	}
	defer func() {
		_ = stdin.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}()

	encoder := json.NewEncoder(stdin)
	decoder := json.NewDecoder(bufio.NewReader(stdout))
	if err := encoder.Encode(map[string]any{
		"id": 1, "method": "initialize",
		"params": map[string]any{"clientInfo": map[string]any{
			"name": "tclaude-setup", "version": "1",
		}},
	}); err != nil {
		return nil, fmt.Errorf("initialize Codex hook discovery: %w", err)
	}
	if _, err := readCodexHookRPCResult(decoder, 1, stderr); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("initialize Codex hook discovery: %w", ctx.Err())
		}
		return nil, err
	}
	if err := encoder.Encode(map[string]any{
		"method": "initialized", "params": map[string]any{},
	}); err != nil {
		return nil, fmt.Errorf("notify Codex hook discovery initialized: %w", err)
	}
	if err := encoder.Encode(map[string]any{
		"id": 2, "method": "hooks/list",
		"params": map[string]any{"cwds": []string{cwd}},
	}); err != nil {
		return nil, fmt.Errorf("request Codex hooks/list: %w", err)
	}
	raw, err := readCodexHookRPCResult(decoder, 2, stderr)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("request Codex hooks/list: %w", ctx.Err())
		}
		return nil, err
	}
	var result codexHooksListResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("decode Codex hooks/list result: %w", err)
	}
	return selectCodexHookTrustEntries(result, hooksPath, want)
}

func readCodexHookRPCResult(
	decoder *json.Decoder, id int, stderr *codexHookDiscoveryStderr,
) (json.RawMessage, error) {
	for {
		var response codexHookRPCResponse
		if err := decoder.Decode(&response); err != nil {
			detail := strings.TrimSpace(stderr.String())
			if detail != "" {
				return nil, fmt.Errorf("read Codex app-server response %d: %w: %s", id, err, detail)
			}
			return nil, fmt.Errorf("read Codex app-server response %d: %w", id, err)
		}
		var got int
		if len(response.ID) == 0 || json.Unmarshal(response.ID, &got) != nil || got != id {
			continue
		}
		if response.Error != nil {
			return nil, fmt.Errorf("Codex app-server request failed (%d): %s",
				response.Error.Code, response.Error.Message)
		}
		return response.Result, nil
	}
}

func selectCodexHookTrustEntries(
	result codexHooksListResult, hooksPath, want string,
) ([]codexHookTrustEntry, error) {
	wantSource := canonicalHookSourcePath(hooksPath)
	wantEvents := make(map[string]bool, len(desiredCodexHookEvents()))
	for _, event := range desiredCodexHookEvents() {
		name, ok := codexHookEventRPCNames[event]
		if !ok {
			return nil, fmt.Errorf("unknown Codex hook event %q", event)
		}
		wantEvents[name] = true
	}
	entries := make([]codexHookTrustEntry, 0, len(wantEvents))
	seen := make(map[string]bool, len(wantEvents))
	seenKeys := make(map[string]bool, len(wantEvents))
	for _, group := range result.Data {
		for _, discoveryErr := range group.Errors {
			return nil, fmt.Errorf("Codex hooks/list failed for %s: %s",
				discoveryErr.Path, discoveryErr.Message)
		}
		for _, hook := range group.Hooks {
			if hook.Command == nil || *hook.Command != want ||
				canonicalHookSourcePath(hook.SourcePath) != wantSource ||
				!wantEvents[hook.EventName] {
				continue
			}
			if seen[hook.EventName] {
				return nil, fmt.Errorf("multiple current tclaude hooks discovered for Codex event %s", hook.EventName)
			}
			if err := validateCodexHookHash(hook.CurrentHash); err != nil {
				return nil, fmt.Errorf("Codex event %s: %w", hook.EventName, err)
			}
			if strings.TrimSpace(hook.Key) == "" {
				return nil, fmt.Errorf("Codex event %s returned an empty hook trust key", hook.EventName)
			}
			if seenKeys[hook.Key] {
				return nil, fmt.Errorf("Codex returned duplicate hook trust key %q", hook.Key)
			}
			seen[hook.EventName] = true
			seenKeys[hook.Key] = true
			entries = append(entries, codexHookTrustEntry{Key: hook.Key, Hash: hook.CurrentHash})
		}
	}
	for event := range wantEvents {
		if !seen[event] {
			return nil, fmt.Errorf("current tclaude hook not returned by Codex hooks/list for event %s", event)
		}
	}
	sortCodexHookTrustEntries(entries)
	return entries, nil
}

func canonicalHookSourcePath(path string) string {
	abs, err := filepath.Abs(path)
	if err == nil {
		path = abs
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return filepath.Clean(path)
}

func validateCodexHookHash(hash string) error {
	const prefix = "sha256:"
	if !strings.HasPrefix(hash, prefix) || len(hash) != len(prefix)+64 {
		return fmt.Errorf("invalid authoritative hook hash %q", hash)
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(hash, prefix)); err != nil {
		return fmt.Errorf("invalid authoritative hook hash %q", hash)
	}
	return nil
}
