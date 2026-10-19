package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
)

// A Claude subscription launch runs Claude Code on a peer operator's own
// subscription token. The pane wrapper fetches the token once from agentd
// and passes it through an inherited pipe, so it is never in the
// environment, argv or on disk; Claude Code reads it at startup.

// claudeSubscriptionCompetingEnvironment drops every inherited variable that
// would make Claude Code authenticate otherwise or send the token elsewhere.
// Network proxies stay: the token goes to Anthropic directly.
func claudeSubscriptionCompetingEnvironment(name string) bool {
	switch name {
	case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "all_proxy", "no_proxy":
		return false
	}
	if _, ok := claudeProviderSettingVariables[name]; ok {
		return true
	}
	switch name {
	case "ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY", "ANTHROPIC_CUSTOM_HEADERS", "ANTHROPIC_API_KEY_HELPER", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR", "CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY":
		return true
	}
	return false
}

func fetchClaudeSubscriptionToken(client *http.Client, sessionID string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://tclaude/v1/models/claude-subscription", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set(agentipc.SessionClaimHeader, sessionID)
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("agentd unreachable")
	}
	defer resp.Body.Close()
	var out struct {
		Token string `json:"token"`
		Error string `json:"error"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 16<<10)).Decode(&out)
	if resp.StatusCode != http.StatusOK || out.Token == "" {
		if out.Error == "" {
			out.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return nil, errors.New(out.Error)
	}
	return []byte(out.Token + "\n"), nil
}

// claudeSubscriptionPipe returns the read end of a pipe already holding the
// token, with the write end closed: Claude Code reads it once to EOF.
func claudeSubscriptionPipe(token []byte) (*os.File, error) {
	read, write, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	// A setup-token is far below the pipe buffer, so this never blocks.
	_, err = write.Write(token)
	if cerr := write.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = read.Close()
		return nil, err
	}
	return read, nil
}

var claudeCodeVersion = regexp.MustCompile(`([0-9]+)\.([0-9]+)\.([0-9]+)`)

// claudeSubscriptionMinVersion is the oldest Claude Code verified to read
// CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR. An older one would ignore it and
// fall back to this node's own login, so it is refused.
var claudeSubscriptionMinVersion = [3]int{2, 1, 303}

func validateClaudeSubscriptionVersion(binary string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, binary, "--version").CombinedOutput()
	if err != nil {
		return errors.New("cannot verify the Claude Code version for a subscription launch; launch refused")
	}
	m := claudeCodeVersion.FindSubmatch(bytes.TrimSpace(output))
	if len(m) != 4 {
		return errors.New("unrecognized Claude Code version; a subscription launch needs Claude Code 2.1.303 or newer")
	}
	got := [3]int{}
	for i := range got {
		got[i], _ = strconv.Atoi(string(m[i+1]))
	}
	for i := range got {
		if got[i] != claudeSubscriptionMinVersion[i] {
			if got[i] < claudeSubscriptionMinVersion[i] {
				return fmt.Errorf("a subscription launch needs Claude Code %d.%d.%d or newer (found %d.%d.%d); update Claude Code on this node", claudeSubscriptionMinVersion[0], claudeSubscriptionMinVersion[1], claudeSubscriptionMinVersion[2], got[0], got[1], got[2])
			}
			break
		}
	}
	return nil
}
