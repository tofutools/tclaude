package agentd

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/opencodeapi"
)

func openCodeRequest(method, endpoint string, runtime db.OpenCodeRuntime, body any) (*http.Request, error) {
	return opencodeapi.NewRequest(method, endpoint, runtime, body)
}

func openCodeProcessOwnsEndpoint(rootPID int, endpoint string) bool {
	return opencodeapi.ProcessOwnsEndpoint(rootPID, endpoint)
}

// openCodeRuntimeVerified confirms a recorded runtime's pid is still the managed
// server before that pid is trusted as an identity or a kill target. It is a
// package var so identity/kill tests can stand up a synthetic proc tree without
// also binding a real listening socket (the same seam pattern as procName /
// procParent in identity.go). Production points it at endpoint ownership.
var openCodeRuntimeVerified = openCodeRuntimeOwnsRecordedPID

// openCodeRuntimeOwnsRecordedPID reports whether the pid recorded for runtime is
// still the managed `opencode serve` process — i.e. that pid (or a descendant)
// still owns runtime.ServerURL's listening socket. This is the recovered-PID
// identity gate: a server that died frees its port, so a same-user process that
// later inherits the stale pid cannot pass (it does not own our recorded
// endpoint), while a live managed server always holds its own port. Endpoint
// ownership is a stronger identity signal than start-time/argv because it binds
// the pid to the exact host:port we minted, and it reuses the same proof the
// per-request auth gate already trusts.
//
// NOTE: ownership matches the pid's whole subtree (ProcessOwnsEndpoint walks
// /proc children), and every managed serve is a child of agentd, so passing
// agentd's own pid here would match. Both callers therefore exclude os.Getpid()
// before consulting this gate — the kill path in stopOpenCodeProcess, and the
// identity path in openCodeRuntimeConvByPID (whose parent probe can pass
// agentd's own pid because a managed serve is agentd's direct child).
func openCodeRuntimeOwnsRecordedPID(runtime db.OpenCodeRuntime) bool {
	return runtime.PID > 1 &&
		opencodeapi.RuntimeOwnsEndpoint(runtime)
}

func openCodeHealthy(runtime db.OpenCodeRuntime) bool {
	request, err := openCodeRequest(http.MethodGet,
		runtime.ServerURL+"/global/health", runtime, nil)
	if err != nil {
		return false
	}
	response, err := opencodeapi.Do(openCodeHealthHTTPClient, request, runtime)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return false
	}
	var health struct {
		Healthy bool `json:"healthy"`
	}
	return json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&health) == nil &&
		health.Healthy
}

func openCodeHealthyAfterRetries(runtime db.OpenCodeRuntime, attempts int, delay time.Duration) bool {
	for attempt := 0; attempt < attempts; attempt++ {
		if openCodeHealthy(runtime) {
			return true
		}
		if attempt+1 < attempts {
			time.Sleep(delay)
		}
	}
	return false
}

func createOpenCodeSession(runtime db.OpenCodeRuntime, title string) (string, error) {
	rules, err := decodeOpenCodePermissionRules(runtime.PermissionJSON)
	if err != nil {
		return "", err
	}
	body := map[string]any{"permission": rules}
	if strings.TrimSpace(title) != "" {
		body["title"] = title
	}
	endpoint := runtime.ServerURL + "/session?directory=" + url.QueryEscape(runtime.Cwd)
	request, err := openCodeRequest(http.MethodPost, endpoint, runtime, body)
	if err != nil {
		return "", err
	}
	response, err := opencodeapi.Do(openCodeHTTPClient, request, runtime)
	if err != nil {
		return "", fmt.Errorf("create OpenCode session: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		detailText := strings.TrimSpace(string(detail))
		if detailText != "" {
			return "", fmt.Errorf(
				"create OpenCode session: HTTP %d: %s", response.StatusCode, detailText)
		}
		return "", fmt.Errorf("create OpenCode session: HTTP %d", response.StatusCode)
	}
	var created struct {
		ID         string                           `json:"id"`
		Permission []harness.OpenCodePermissionRule `json:"permission"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&created); err != nil {
		return "", fmt.Errorf("decode OpenCode session: %w", err)
	}
	if !strings.HasPrefix(created.ID, "ses_") {
		return "", fmt.Errorf("create OpenCode session returned invalid id %q", created.ID)
	}
	if !openCodePermissionHasSuffix(created.Permission, rules) {
		return "", fmt.Errorf("OpenCode session did not retain the permission policy at creation")
	}
	return created.ID, nil
}

func decodeOpenCodePermissionRules(raw string) ([]harness.OpenCodePermissionRule, error) {
	var rules []harness.OpenCodePermissionRule
	if err := json.Unmarshal([]byte(raw), &rules); err != nil {
		return nil, fmt.Errorf("decode OpenCode permission policy: %w", err)
	}
	if len(rules) == 0 {
		return nil, fmt.Errorf("OpenCode permission policy is empty")
	}
	for i, rule := range rules {
		if strings.TrimSpace(rule.Permission) == "" || strings.TrimSpace(rule.Pattern) == "" {
			return nil, fmt.Errorf("OpenCode permission rule %d has an empty permission or pattern", i)
		}
		switch rule.Action {
		case "allow", "ask", "deny":
		default:
			return nil, fmt.Errorf("OpenCode permission rule %d has invalid action %q", i, rule.Action)
		}
	}
	return rules, nil
}

func ensureOpenCodeSessionPermission(runtime db.OpenCodeRuntime) error {
	if strings.TrimSpace(runtime.PermissionJSON) == "" {
		// A v149 runtime row cannot prove what authority its live session has.
		// Reconciliation must fail closed rather than blessing the historical
		// unscoped posture; the reaper will stop it and a current relaunch will
		// compile and persist an explicit policy.
		return fmt.Errorf("OpenCode runtime has no persisted permission policy; relaunch it under current access control")
	}
	expected, err := decodeOpenCodePermissionRules(runtime.PermissionJSON)
	if err != nil {
		return err
	}
	current, err := getOpenCodeSessionPermission(runtime)
	if err != nil {
		return err
	}
	if openCodePermissionHasSuffix(current, expected) {
		return nil
	}
	endpoint := runtime.ServerURL + "/session/" + url.PathEscape(runtime.ConvID) +
		"?directory=" + url.QueryEscape(runtime.Cwd)
	request, err := openCodeRequest(http.MethodPatch, endpoint, runtime,
		map[string]any{"permission": expected})
	if err != nil {
		return err
	}
	response, err := opencodeapi.Do(openCodeHTTPClient, request, runtime)
	if err != nil {
		return fmt.Errorf("reapply OpenCode session permission: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("reapply OpenCode session permission: HTTP %d", response.StatusCode)
	}
	var updated struct {
		Permission []harness.OpenCodePermissionRule `json:"permission"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&updated); err != nil {
		return fmt.Errorf("decode reapplied OpenCode session permission: %w", err)
	}
	if !openCodePermissionHasSuffix(updated.Permission, expected) {
		return fmt.Errorf("OpenCode session did not retain the reapplied permission policy")
	}
	return nil
}

func getOpenCodeSessionPermission(runtime db.OpenCodeRuntime) ([]harness.OpenCodePermissionRule, error) {
	if strings.TrimSpace(runtime.ConvID) == "" {
		return nil, fmt.Errorf("OpenCode conversation id is empty")
	}
	endpoint := runtime.ServerURL + "/session/" + url.PathEscape(runtime.ConvID) +
		"?directory=" + url.QueryEscape(runtime.Cwd)
	request, err := openCodeRequest(http.MethodGet, endpoint, runtime, nil)
	if err != nil {
		return nil, err
	}
	response, err := opencodeapi.Do(openCodeHTTPClient, request, runtime)
	if err != nil {
		return nil, fmt.Errorf("read OpenCode session permission: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("read OpenCode session permission: HTTP %d", response.StatusCode)
	}
	var session struct {
		Permission []harness.OpenCodePermissionRule `json:"permission"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&session); err != nil {
		return nil, fmt.Errorf("decode OpenCode session permission: %w", err)
	}
	return session.Permission, nil
}

func openCodePermissionHasSuffix(current, expected []harness.OpenCodePermissionRule) bool {
	if len(expected) == 0 || len(current) < len(expected) {
		return false
	}
	offset := len(current) - len(expected)
	for i := range expected {
		if current[offset+i] != expected[i] {
			return false
		}
	}
	return true
}
