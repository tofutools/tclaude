package session

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

const autoNameRequestTimeout = 500 * time.Millisecond
const autoNamePromptRunes = 2048

var freeFloatingAgentNameRe = regexp.MustCompile(
	`^(?:(?:session-)?[0-9]{8}-[0-9]{6}|[0-9]{8}-[0-9]{4})-[a-zA-Z0-9]{1,8}$`,
)

// AutoNameRequest asks agentd to refine a free-floating session's generated
// name from its first prompt. ConvID is only a cross-check; the daemon resolves
// the calling session from process ancestry before accepting the request.
type AutoNameRequest struct {
	ConvID string `json:"conv_id"`
	Prompt string `json:"prompt"`
}

// AutoNamePromptExcerpt bounds the untrusted prompt before it crosses the
// small auto-name handoff and before it is embedded in a model question.
func AutoNamePromptExcerpt(prompt string) string {
	runes := []rune(strings.TrimSpace(prompt))
	if len(runes) > autoNamePromptRunes {
		runes = runes[:autoNamePromptRunes]
	}
	return string(runes)
}

// FreeFloatingAgentName returns the deterministic, cheap display-name fallback
// for a newly enrolled tclaude session. The actor suffix disambiguates sessions
// started in the same minute without introducing random state of its own.
func FreeFloatingAgentName(created time.Time, agentID string) string {
	if created.IsZero() {
		created = time.Now()
	}
	uniq := strings.TrimPrefix(strings.TrimSpace(agentID), "agt_")
	if len(uniq) > 8 {
		uniq = uniq[:8]
	}
	if uniq == "" {
		uniq = "unknown"
	}
	return fmt.Sprintf("%s-%s", created.UTC().Format("20060102-1504"), uniq)
}

// IsFreeFloatingAgentName reports whether name is one of this package's
// generated fallbacks. Background refinement uses it as a compare-and-swap
// guard so it cannot replace an explicit or managed-spawn name.
func IsFreeFloatingAgentName(name string) bool {
	return freeFloatingAgentNameRe.MatchString(strings.TrimSpace(name))
}

// MaybeRequestAutoName makes the best-effort daemon handoff after the prompt's
// ordinary hook work has completed. It is opt-in, has a sub-second transport
// bound, and the daemon answers before starting the model call.
func MaybeRequestAutoName(input HookCallbackInput) {
	if input.HookEventName != "UserPromptSubmit" || strings.TrimSpace(input.Prompt) == "" {
		return
	}
	cfg, err := config.Load()
	if err != nil || !cfg.AutoNameFromPromptEnabled() {
		return
	}
	body, err := json.Marshal(AutoNameRequest{
		ConvID: input.ConvID,
		Prompt: AutoNamePromptExcerpt(input.Prompt),
	})
	if err != nil {
		return
	}
	if err := postAutoNameToDaemon(body); err != nil {
		slog.Debug("automatic session naming handoff failed",
			"conv_id", input.ConvID, "error", err, "module", "hooks")
	}
}

func postAutoNameToDaemon(body []byte) error {
	socks := agentipc.ClientSocketPaths()
	if len(socks) == 0 {
		return fmt.Errorf("no agentd socket path resolved")
	}
	client := &http.Client{
		Timeout: autoNameRequestTimeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var lastErr error
				for _, sock := range socks {
					conn, err := (&net.Dialer{}).DialContext(ctx, "unix", sock)
					if err == nil {
						return conn, nil
					}
					lastErr = err
				}
				return nil, lastErr
			},
		},
	}
	req, err := http.NewRequest(http.MethodPost, "http://tclaude/v1/whoami/auto-name", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusNoContent {
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("agentd returned %d: %s", resp.StatusCode, bytes.TrimSpace(preview))
	}
	return nil
}
