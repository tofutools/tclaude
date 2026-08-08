package proxy

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

func TestConfigured(t *testing.T) {
	previousRequest := agent.DaemonRequestImpl
	t.Cleanup(func() {
		agent.DaemonRequestImpl = previousRequest
	})

	t.Run("absent", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		t.Setenv("TCLAUDE_AGENTD_SOCKET", "")
		t.Setenv("CODEX_PERMISSION_PROFILE", "")
		if Configured() {
			t.Fatal("proxy reported configured without local or daemon policy")
		}
	})

	t.Run("local semantic policy", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		t.Setenv("TCLAUDE_AGENTD_SOCKET", "")
		t.Setenv("CODEX_PERMISSION_PROFILE", "")
		if err := config.Save(&config.Config{Agent: &config.AgentConfig{
			GitProxy: &config.GitProxyConfig{AllowedRemotes: []string{" github.com/acme "}},
		}}); err != nil {
			t.Fatalf("save config: %v", err)
		}
		if !Configured() {
			t.Fatal("proxy not configured by a non-empty normalized allow-list")
		}
	})

	t.Run("sandboxed agent daemon projection", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		t.Setenv("TCLAUDE_AGENTD_SOCKET", "/agent-reachable/agentd.sock")
		agent.DaemonRequestImpl = func(method, path string, _, out any, opts agent.DaemonOpts) error {
			if method != http.MethodGet || path != "/v1/info" {
				t.Fatalf("request = %s %s, want GET /v1/info", method, path)
			}
			if !opts.NoRetry || opts.Timeout != 250*time.Millisecond {
				t.Fatalf("discovery opts = %+v, want no retry and 250ms timeout", opts)
			}
			data, _ := json.Marshal(map[string]bool{"proxy": true})
			return json.Unmarshal(data, out)
		}
		if !Configured() {
			t.Fatal("proxy not configured from daemon capability projection")
		}
	})

	t.Run("legacy managed Codex agent and old daemon", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		t.Setenv("TCLAUDE_AGENTD_SOCKET", "")
		t.Setenv("CODEX_PERMISSION_PROFILE", "tclaude-agent")
		agent.DaemonRequestImpl = func(_, _ string, _, out any, _ agent.DaemonOpts) error {
			return json.Unmarshal([]byte(`{"idempotency":"v1"}`), out)
		}
		if !Configured() {
			t.Fatal("old daemon's historically visible proxy tree was hidden")
		}
	})

	t.Run("daemon reports disabled", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		t.Setenv("TCLAUDE_AGENTD_SOCKET", "/agent-reachable/agentd.sock")
		agent.DaemonRequestImpl = func(_, _ string, _, out any, _ agent.DaemonOpts) error {
			return json.Unmarshal([]byte(`{"proxy":false}`), out)
		}
		if Configured() {
			t.Fatal("proxy reported configured after daemon explicitly disabled it")
		}
	})
}
