package proxy

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

func TestConfigured(t *testing.T) {
	previousAvailable := agent.DaemonAvailableImpl
	previousRequest := agent.DaemonRequestImpl
	t.Cleanup(func() {
		agent.DaemonAvailableImpl = previousAvailable
		agent.DaemonRequestImpl = previousRequest
	})

	t.Run("absent", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		agent.DaemonAvailableImpl = func() bool { return false }
		if Configured() {
			t.Fatal("proxy reported configured without local or daemon policy")
		}
	})

	t.Run("local semantic policy", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		if err := config.Save(&config.Config{Agent: &config.AgentConfig{
			GitProxy: &config.GitProxyConfig{AllowedRemotes: []string{" github.com/acme "}},
		}}); err != nil {
			t.Fatalf("save config: %v", err)
		}
		agent.DaemonAvailableImpl = func() bool { return false }
		if !Configured() {
			t.Fatal("proxy not configured by a non-empty normalized allow-list")
		}
	})

	t.Run("sandboxed agent daemon projection", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		t.Setenv("TCLAUDE_AGENTD_SOCKET", "/agent-reachable/agentd.sock")
		agent.DaemonAvailableImpl = func() bool { return true }
		agent.DaemonRequestImpl = func(method, path string, _, out any, _ agent.DaemonOpts) error {
			if method != http.MethodGet || path != "/v1/info" {
				t.Fatalf("request = %s %s, want GET /v1/info", method, path)
			}
			data, _ := json.Marshal(map[string]bool{"proxy": true})
			return json.Unmarshal(data, out)
		}
		if !Configured() {
			t.Fatal("proxy not configured from daemon capability projection")
		}
	})
}
