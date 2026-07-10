package agent

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// spawnCwdProofPrefix mirrors agentd's marker prefix. The agent package cannot
// import agentd (agentd already imports agent), so keep this wire-level string
// in sync with pkg/claude/agentd/spawn_cwd_proof.go.
const spawnCwdProofPrefix = ".tclaude-spawn-cwd-proof-"

type spawnCwdProofResponse struct {
	Required   bool   `json:"required"`
	Proof      string `json:"proof,omitempty"`
	Cwd        string `json:"cwd,omitempty"`
	MarkerPath string `json:"marker_path,omitempty"`
}

// prepareSpawnCwdWriteProof asks agentd for a caller-bound challenge and, when
// the caller is an agent, creates the requested empty marker in cwd. Creating
// that file is the capability test: it happens in this CLI process, inside the
// parent's sandbox, before the daemon (outside that sandbox) launches a child.
//
// The returned cleanup is always safe to call. Agentd normally removes the
// marker while consuming the proof; cleanup covers pre-spawn failures.
func prepareSpawnCwdWriteProof(cwd string) (proof, canonicalCwd string, cleanup func(), err error) {
	cleanup = func() {}
	var resp spawnCwdProofResponse
	if err := DaemonRequest(http.MethodPost, "/v1/spawn-cwd-proof",
		map[string]string{"cwd": cwd}, &resp, DaemonOpts{}); err != nil {
		return "", "", cleanup, fmt.Errorf("prepare spawn cwd write proof: %w", err)
	}
	if !resp.Required {
		return "", "", cleanup, nil
	}

	resp.Proof = strings.TrimSpace(resp.Proof)
	resp.Cwd = filepath.Clean(strings.TrimSpace(resp.Cwd))
	resp.MarkerPath = filepath.Clean(strings.TrimSpace(resp.MarkerPath))
	expected := filepath.Join(resp.Cwd, spawnCwdProofPrefix+resp.Proof)
	if resp.Proof == "" || !filepath.IsAbs(resp.Cwd) || resp.MarkerPath != expected {
		return "", "", cleanup, fmt.Errorf("agentd returned an invalid spawn cwd proof challenge")
	}

	f, openErr := os.OpenFile(resp.MarkerPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if openErr != nil {
		return "", "", cleanup, fmt.Errorf(
			"spawn working directory %q is not writable by this agent's sandbox: %w",
			resp.Cwd, openErr)
	}
	if closeErr := f.Close(); closeErr != nil {
		_ = os.Remove(resp.MarkerPath)
		return "", "", cleanup, fmt.Errorf("close spawn cwd proof marker in %q: %w", resp.Cwd, closeErr)
	}
	cleanup = func() { _ = os.Remove(resp.MarkerPath) }
	return resp.Proof, resp.Cwd, cleanup, nil
}
