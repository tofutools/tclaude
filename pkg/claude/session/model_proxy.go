package session

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
)

type modelProxyBridge struct {
	client          *http.Client
	session, bearer string
}

func newModelProxyBridge(client *http.Client, session, reference string) (*modelProxyBridge, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	bearer := hex.EncodeToString(secret)
	hash := sha256.Sum256([]byte(bearer))
	body, _ := json.Marshal(map[string]string{"reference": reference, "bearer_hash": hex.EncodeToString(hash[:])})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://tclaude/v1/models/bind", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set(agentipc.SessionClaimHeader, session)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("daemon could not bind the launch to its model gateway")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, errors.New("daemon refused model gateway registration")
	}
	// Streaming requests must outlive the ordinary named HTTP client's timeout.
	streaming := *client
	streaming.Timeout = 0
	return &modelProxyBridge{client: &streaming, session: session, bearer: bearer}, nil
}
func (b *modelProxyBridge) revoke() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, "http://tclaude/v1/models/bind", nil)
	req.Header.Set(agentipc.SessionClaimHeader, b.session)
	req.Header.Set("Authorization", "Bearer "+b.bearer)
	if resp, err := b.client.Do(req); err == nil {
		_ = resp.Body.Close()
	}
}
func (b *modelProxyBridge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	auth, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || subtle.ConstantTimeCompare([]byte(auth), []byte(b.bearer)) != 1 || r.Header.Get("Origin") != "" {
		modelBridgeError(w, 403, "model gateway requires this launch's bearer")
		return
	}
	path := strings.TrimPrefix(r.URL.EscapedPath(), "/model/")
	if path != "v1/messages" && path != "v1/messages/count_tokens" && path != "v1/models" {
		modelBridgeError(w, 404, "model gateway endpoint is not supported")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Hour)
	defer cancel()
	target := "http://tclaude/v1/models/request/" + path
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, target, http.MaxBytesReader(w, r.Body, 16<<20))
	if err != nil {
		modelBridgeError(w, 400, "invalid model gateway request")
		return
	}
	for _, name := range []string{"Content-Type", "Accept", "Anthropic-Version", "Anthropic-Beta"} {
		if values := r.Header.Values(name); len(values) > 0 {
			req.Header[name] = append([]string(nil), values...)
		}
	}
	req.Header.Set(agentipc.SessionClaimHeader, b.session)
	req.Header.Set("Authorization", "Bearer "+b.bearer)
	resp, err := b.client.Do(req)
	if err != nil {
		modelBridgeError(w, 503, "model gateway is unavailable; no fallback to local credentials")
		return
	}
	defer resp.Body.Close()
	for name, values := range resp.Header {
		lower := strings.ToLower(name)
		if lower == "content-type" || lower == "request-id" || lower == "retry-after" || lower == "x-should-retry" || strings.HasPrefix(lower, "anthropic-ratelimit-") {
			w.Header()[name] = append([]string(nil), values...)
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(resp.StatusCode)
	buffer := make([]byte, 32<<10)
	for {
		n, e := resp.Body.Read(buffer)
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
func modelBridgeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": map[string]string{"type": "api_error", "message": message}})
}
func modelProxyCompetingEnvironment(name string) bool {
	if _, ok := claudeProviderSettingVariables[name]; ok {
		return true
	}
	switch name {
	case "ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY", "ANTHROPIC_CUSTOM_HEADERS", "ANTHROPIC_API_KEY_HELPER", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC":
		return true
	}
	return false
}

// Settings env overrides exported shell values and can route around the launch
// gateway. Refuse competing settings rather than silently spend a saved login.
func validateModelProxySettings(cwd string, environment []string) error {
	env := map[string]string{}
	for _, pair := range environment {
		k, v, ok := strings.Cut(pair, "=")
		if ok {
			env[k] = v
		}
	}
	for _, path := range claudeProviderSettingsPaths(cwd, env) {
		raw, e := os.ReadFile(path)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return fmt.Errorf("cannot inspect Claude gateway settings: %s", path)
		}
		var settings struct {
			Environment map[string]json.RawMessage `json:"env"`
			Helper      json.RawMessage            `json:"apiKeyHelper"`
		}
		if json.Unmarshal(raw, &settings) != nil {
			return fmt.Errorf("cannot parse Claude gateway settings: %s", path)
		}
		if len(settings.Helper) > 0 && string(settings.Helper) != "null" && string(settings.Helper) != `""` {
			return fmt.Errorf("model gateway conflicts with apiKeyHelper in %s; remove it for this launch", path)
		}
		for key := range settings.Environment {
			_, provider := claudeProviderSettingVariables[key]
			if provider || modelProxyCompetingEnvironment(key) {
				return fmt.Errorf("model gateway conflicts with env.%s in %s; remove it for this launch", key, path)
			}
		}
	}
	return nil
}

func validateModelProxyExtraArgs(args []string) error {
	for _, arg := range args {
		flag, _, _ := strings.Cut(arg, "=")
		if flag == "--settings" {
			return errors.New("--model-proxy cannot combine with pass-through --settings; remove the override so the gateway can verify provider settings")
		}
	}
	return nil
}
