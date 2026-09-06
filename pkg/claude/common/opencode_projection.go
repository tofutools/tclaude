package common

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

const openCodeLaunchProjectionMarkerPrefix = "tclaude-opencode-launch-"

// OpenCodeLaunchProjectionMarker derives a non-credential argv marker from the
// private server authority handed to session new. It lets agentd later prove
// that an exact pane was constructed for that server without exposing the
// password in argv or tmux metadata.
func OpenCodeLaunchProjectionMarker(serverURL, password string) string {
	serverURL = strings.TrimSpace(serverURL)
	if serverURL == "" || password == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(serverURL + "\x00" + password))
	return openCodeLaunchProjectionMarkerPrefix + hex.EncodeToString(sum[:16])
}
