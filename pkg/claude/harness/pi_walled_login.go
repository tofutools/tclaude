package harness

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// PiWalledLoginTicket tracks letting an OAuth refresh land from inside
// tclaude's sandbox.
const PiWalledLoginTicket = "tcl-lpbxag"

// PiWalledLoginCheck vets the login a pi launch inside tclaude's sandbox will
// use. The wall floors pi's auth.json read-only (pi runs a `!`-prefixed
// api_key as a shell command, so a writable file would be an escape), and an
// OAuth login therefore cannot refresh inside it. An expired login refuses
// the launch with what to do, rather than failing on the first model call; a
// live one returns a notice naming when it will stop working.
//
// model is the launch's `provider/id` ("" for the agent directory's default
// provider). A provider tclaude cannot name, or one without an OAuth login,
// passes: pi itself reports what it cannot use.
func PiWalledLoginCheck(model string, now time.Time) (notice string, err error) {
	agentDir, err := piAgentDir(nil)
	if err != nil {
		return "", nil
	}
	provider, _, _ := strings.Cut(strings.TrimSpace(model), "/")
	if strings.TrimSpace(model) == "" || !strings.Contains(model, "/") {
		settings, err := readPiSettings(filepath.Join(agentDir, "settings.json"))
		if err != nil {
			return "", nil
		}
		provider = strings.TrimSpace(settings.DefaultProvider)
	}
	if provider == "" {
		return "", nil
	}
	raw, err := os.ReadFile(filepath.Join(agentDir, "auth.json"))
	if err != nil {
		return "", nil
	}
	var credentials map[string]struct {
		Type    string `json:"type"`
		Expires int64  `json:"expires"`
	}
	if json.Unmarshal(raw, &credentials) != nil {
		return "", nil
	}
	login, ok := credentials[provider]
	if !ok || login.Type != "oauth" || login.Expires == 0 {
		return "", nil
	}
	expires := time.UnixMilli(login.Expires)
	if !now.Before(expires) {
		return "", fmt.Errorf("pi's %s login expired at %s and cannot refresh inside tclaude's sandbox, "+
			"where pi's auth.json is read-only: run pi once outside the sandbox to refresh it, or use an "+
			"API key (%s tracks refreshing inside the sandbox)",
			provider, expires.Local().Format(time.RFC3339), PiWalledLoginTicket)
	}
	return fmt.Sprintf("pi's %s login expires at %s and cannot refresh inside tclaude's sandbox; "+
		"after that the agent's model calls fail until you run pi outside the sandbox to refresh it (%s)",
		provider, expires.Local().Format(time.RFC3339), PiWalledLoginTicket), nil
}
