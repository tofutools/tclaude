package standingorders

import (
	"bytes"
	"encoding/json"
	"strings"
)

// NormalizeToolName collapses the one known cross-harness alias. Claude Code
// and OpenCode call the general shell tool Bash; Codex emits shell.
func NormalizeToolName(name string) string {
	name = strings.TrimSpace(name)
	switch strings.ToLower(name) {
	case "bash", "shell":
		return "Bash"
	}
	return name
}

// NormalizeToolInput removes insignificant JSON whitespace so equivalent hook
// payloads do not miss a matcher merely because two harnesses serialize them
// differently. Invalid input fails closed to its trimmed raw representation;
// production hook JSON is validated before it reaches this seam.
func NormalizeToolInput(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var out bytes.Buffer
	if err := json.Compact(&out, raw); err != nil {
		return strings.TrimSpace(string(raw))
	}
	return out.String()
}
