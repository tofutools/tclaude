//go:build linux

package session

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	filteredModelEndpointSmokeEnv = "TCLAUDE_FILTERED_MODEL_ENDPOINT_SMOKE"
	filteredClaudePinnedVersion   = "2.1.220"
	filteredCodexPinnedVersion    = "0.145.0"
)

// TestPinnedFilteredModelEndpointEvidence runs the exact Claude and Codex
// binaries pinned by CI against a local CONNECT auditor. Invalid credentials
// deliberately stop before a billable model turn; the real clients still
// select and attempt their production API/auth origins. Each scenario refuses
// every undeclared origin and requires its expected endpoint to be observed.
func TestPinnedFilteredModelEndpointEvidence(t *testing.T) {
	if os.Getenv(filteredModelEndpointSmokeEnv) != "1" {
		t.Skip("set TCLAUDE_FILTERED_MODEL_ENDPOINT_SMOKE=1 on the pinned Linux CI boundary")
	}
	claude := requirePinnedFilteredHarness(
		t, "claude", filteredClaudePinnedVersion)
	codex := requirePinnedFilteredHarness(
		t, "codex", filteredCodexPinnedVersion)

	t.Run("claude-api-key", func(t *testing.T) {
		t.Parallel()
		runFilteredEndpointEvidence(t, filteredEndpointEvidenceCase{
			binary: claude,
			args: []string{
				"--print", "--model", "sonnet", "Reply with exactly ok.",
			},
			env:      map[string]string{"ANTHROPIC_API_KEY": "invalid-ci-evidence-key"},
			expected: []string{"api.anthropic.com:443"},
		})
	})
	t.Run("codex-api-key", func(t *testing.T) {
		t.Parallel()
		home := t.TempDir()
		codexHome := filepath.Join(home, ".codex")
		require.NoError(t, os.MkdirAll(codexHome, 0o700))
		require.NoError(t, os.WriteFile(
			filepath.Join(codexHome, "auth.json"),
			[]byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"invalid-ci-evidence-key"}`),
			0o600))
		runFilteredEndpointEvidence(t, filteredEndpointEvidenceCase{
			binary: codex,
			args: append(filteredCodexEndpointEvidenceArgs(),
				"exec", "--skip-git-repo-check", "--model", "gpt-5.4",
				"Reply with exactly ok."),
			env:      map[string]string{"CODEX_HOME": codexHome},
			expected: []string{"api.openai.com:443"},
		})
	})
	t.Run("codex-chatgpt", func(t *testing.T) {
		t.Parallel()
		runFilteredCodexChatGPTEndpointEvidence(
			t, codex, time.Now().Add(time.Hour), "chatgpt.com:443")
	})
	t.Run("codex-token-refresh", func(t *testing.T) {
		t.Parallel()
		runFilteredCodexChatGPTEndpointEvidence(
			t, codex, time.Now().Add(-time.Hour), "auth.openai.com:443")
	})
}

type filteredEndpointEvidenceCase struct {
	binary   string
	args     []string
	env      map[string]string
	expected []string
}

func runFilteredEndpointEvidence(
	t *testing.T,
	evidence filteredEndpointEvidenceCase,
) {
	t.Helper()
	proxy := startFilteredEndpointAuditProxy(t)
	home := t.TempDir()
	env := map[string]string{
		"HOME":        home,
		"PATH":        os.Getenv("PATH"),
		"HTTPS_PROXY": "http://" + proxy.address(),
		"HTTP_PROXY":  "http://" + proxy.address(),
		"NO_PROXY":    "",
	}
	for name, value := range evidence.env {
		env[name] = value
	}
	if filepath.Base(evidence.binary) == "codex" && env["CODEX_HOME"] == "" {
		codexHome := filepath.Join(home, ".codex")
		require.NoError(t, os.MkdirAll(codexHome, 0o700))
		env["CODEX_HOME"] = codexHome
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, evidence.binary, evidence.args...)
	command.Dir = home
	command.Env = filteredEndpointEvidenceEnv(env)
	output, _ := command.CombinedOutput()
	time.Sleep(100 * time.Millisecond)
	hosts := proxy.hosts()
	for _, expected := range evidence.expected {
		assert.Containsf(t, hosts, expected,
			"pinned harness did not attempt required endpoint %s; output:\n%s",
			expected, output)
	}
	for _, host := range hosts {
		assert.Containsf(t, evidence.expected, host,
			"pinned harness attempted undeclared endpoint %s; output:\n%s",
			host, output)
	}
}

func runFilteredCodexChatGPTEndpointEvidence(
	t *testing.T,
	codex string,
	accessExpiry time.Time,
	expected string,
) {
	t.Helper()
	home := t.TempDir()
	codexHome := filepath.Join(home, ".codex")
	require.NoError(t, os.MkdirAll(codexHome, 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(codexHome, "auth.json"),
		filteredCodexChatGPTAuth(accessExpiry), 0o600,
	))
	runFilteredEndpointEvidence(t, filteredEndpointEvidenceCase{
		binary: codex,
		args: append(filteredCodexEndpointEvidenceArgs(),
			"exec", "--skip-git-repo-check", "--model", "gpt-5.4",
			"Reply with exactly ok."),
		env: map[string]string{
			// runFilteredEndpointEvidence creates its own isolated HOME, so
			// preserve only this fixture's explicit Codex authority.
			"CODEX_HOME": codexHome,
		},
		expected: []string{expected},
	})
}

func filteredCodexEndpointEvidenceArgs() []string {
	// Plugin startup sync is independently optional: the real client continues
	// when its GitHub/ChatGPT catalog probes fail. Disable it here so this named
	// boundary measures the model/auth origins that can brick a launch, not
	// unrelated marketplace availability.
	return []string{
		"--disable", "plugins",
		"--disable", "remote_plugin",
		"--disable", "plugin_sharing",
	}
}

func filteredCodexChatGPTAuth(accessExpiry time.Time) []byte {
	encodeJWT := func(payload any) string {
		header, _ := json.Marshal(map[string]string{
			"alg": "none", "typ": "JWT",
		})
		body, _ := json.Marshal(payload)
		return base64.RawURLEncoding.EncodeToString(header) + "." +
			base64.RawURLEncoding.EncodeToString(body) + "." +
			base64.RawURLEncoding.EncodeToString([]byte("ci-signature"))
	}
	idToken := encodeJWT(map[string]any{
		"email": "filtered-evidence@example.invalid",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_plan_type":  "plus",
			"chatgpt_user_id":    "ci-user",
			"chatgpt_account_id": "ci-account",
		},
	})
	accessToken := encodeJWT(map[string]any{
		"sub": "ci-user", "exp": accessExpiry.Unix(),
	})
	auth, _ := json.Marshal(map[string]any{
		"auth_mode": "chatgpt",
		"tokens": map[string]any{
			"id_token": idToken, "access_token": accessToken,
			"refresh_token": "invalid-ci-refresh-token",
			"account_id":    "ci-account",
		},
		"last_refresh": time.Now().UTC().Format(time.RFC3339),
	})
	return auth
}

func requirePinnedFilteredHarness(
	t *testing.T,
	name, version string,
) string {
	t.Helper()
	binary, err := exec.LookPath(name)
	require.NoError(t, err)
	output, err := exec.Command(binary, "--version").CombinedOutput()
	require.NoErrorf(t, err, "%s --version:\n%s", name, output)
	require.Contains(t, string(output), version,
		"endpoint evidence must execute the CI-pinned harness version")
	return binary
}

func filteredEndpointEvidenceEnv(values map[string]string) []string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	slices.Sort(names)
	env := make([]string, 0, len(names))
	for _, name := range names {
		env = append(env, name+"="+values[name])
	}
	return env
}

type filteredEndpointAuditProxy struct {
	listener net.Listener
	mu       sync.Mutex
	seen     map[string]struct{}
}

func startFilteredEndpointAuditProxy(t *testing.T) *filteredEndpointAuditProxy {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	proxy := &filteredEndpointAuditProxy{
		listener: listener,
		seen:     make(map[string]struct{}),
	}
	t.Cleanup(func() { _ = listener.Close() })
	go proxy.serve()
	return proxy
}

func (p *filteredEndpointAuditProxy) address() string {
	return p.listener.Addr().String()
}

func (p *filteredEndpointAuditProxy) hosts() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	hosts := make([]string, 0, len(p.seen))
	for host := range p.seen {
		hosts = append(hosts, host)
	}
	slices.Sort(hosts)
	return hosts
}

func (p *filteredEndpointAuditProxy) serve() {
	for {
		connection, err := p.listener.Accept()
		if err != nil {
			return
		}
		go p.handle(connection)
	}
}

func (p *filteredEndpointAuditProxy) handle(connection net.Conn) {
	defer func() { _ = connection.Close() }()
	_ = connection.SetDeadline(time.Now().Add(2 * time.Second))
	line, err := bufio.NewReader(connection).ReadString('\n')
	if err != nil {
		return
	}
	fields := strings.Fields(line)
	if len(fields) == 3 && fields[0] == "CONNECT" {
		host, port, splitErr := net.SplitHostPort(fields[1])
		if splitErr == nil {
			p.mu.Lock()
			p.seen[net.JoinHostPort(strings.ToLower(host), port)] = struct{}{}
			p.mu.Unlock()
		}
	}
	_, _ = fmt.Fprint(connection,
		"HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
}
