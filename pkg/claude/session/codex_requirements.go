package session

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// A Codex launch with `--sandbox danger-full-access` is only a request. When
// a managed requirements layer (requirements.toml, MDM or the enterprise cloud
// bundle) disallows that mode, Codex does not refuse the launch: it falls back
// to a requirement-compliant sandbox (read-only) and warns
// (codex-rs/core/src/config/mod.rs, apply_requirement_constrained_value, at
// codex-cli 0.166.0). The mode recorded for the agent then claims a posture
// Codex is not running.
//
// The app-server's `configRequirements/read` reports the managed requirements
// as Codex resolved them. When they apply, the launch's verdict is recorded as
// "on": Codex's sandbox confines the agent, or may. It is verified only when
// the allowed modes leave danger-full-access out, so Codex must fall back.
// Any other requirement may force the fallback too (a filesystem deny_read
// does, and is not on the wire), as may one tclaude failed to read, so those
// launches record an unverified "on". A launch is taken as unconfined only
// when Codex reports no managed requirements at all; its verdict then stays
// unrecorded, as for any Codex launch, and its mode states its posture.
//
// The verdict is for the record (the dashboard's badge, a live page's
// read-back), not for access enforcement, which is planned from the mode.

// codexRequirementsReader is the process seam. Production runs the real
// app-server; tests swap it with SetCodexRequirementsProbeForTest.
var codexRequirementsReader = readCodexRequirements

// readCodexRequirements runs `configRequirements/read` (no params) on a
// short-lived app-server for the launch.
func readCodexRequirements(cwd string, environment []sandboxpolicy.EnvironmentEntry) (json.RawMessage, error) {
	return readCodexAppServerJSON(cwd, environment, "", "configRequirements/read", nil)
}

// SetCodexRequirementsProbeForTest swaps the Codex `configRequirements/read`
// probe, returning a restore func. read returns the raw result.
func SetCodexRequirementsProbeForTest(read func(cwd string, environment []sandboxpolicy.EnvironmentEntry) (json.RawMessage, error)) func() {
	previous := codexRequirementsReader
	codexRequirementsReader = read
	return func() { codexRequirementsReader = previous }
}

// maxCodexRequirementsDetail bounds the probe error folded into the recorded
// source, which is persisted and shown.
const maxCodexRequirementsDetail = 200

// VerifyCodexSandboxOff checks a harness-builtin Codex launch that asks for no
// sandbox against Codex's managed requirements, and records an "on" verdict
// when they keep Codex's sandbox on, or may. Any other launch is returned
// unchanged, without running Codex.
func VerifyCodexSandboxOff(
	h *harness.Harness,
	implementation sandboxpolicy.Implementation,
	mode, cwd string,
	environment []sandboxpolicy.EnvironmentEntry,
	verdict harness.LaunchOSSandbox,
) harness.LaunchOSSandbox {
	if h == nil || h.Name != harness.CodexName || implementation != sandboxpolicy.ImplementationHarnessBuiltin {
		return verdict
	}
	if off, err := harness.SandboxOffMode(h); err != nil || mode != off {
		return verdict
	}
	note, applies, certain := codexRequirementsVerdict(cwd, environment)
	if !applies {
		return verdict
	}
	return harness.LaunchOSSandbox{State: "on", Source: note, Unverified: !certain}
}

// codexRequirementsVerdict says whether Codex's managed requirements keep its
// sandbox on for a danger-full-access launch (applies), whether that is
// certain, and why.
func codexRequirementsVerdict(cwd string, environment []sandboxpolicy.EnvironmentEntry) (note string, applies, certain bool) {
	raw, err := codexRequirementsReader(cwd, environment)
	if err != nil {
		detail := err.Error()
		if len(detail) > maxCodexRequirementsDetail {
			detail = detail[:maxCodexRequirementsDetail] + "…"
		}
		return "Codex managed requirements could not be read: " + detail, true, false
	}
	var reply struct {
		Requirements *struct {
			AllowedSandboxModes []string `json:"allowedSandboxModes"`
		} `json:"requirements"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return "Codex managed requirements could not be parsed", true, false
	}
	if reply.Requirements == nil {
		return "", false, false
	}
	modes := reply.Requirements.AllowedSandboxModes
	if modes == nil {
		return "Codex managed requirements apply and may keep its sandbox on", true, false
	}
	shown := make([]string, 0, len(modes))
	allowed := false
	for _, m := range modes {
		allowed = allowed || m == "danger-full-access"
		if len(shown) == 4 || len(m) > 32 || strings.Trim(m, "abcdefghijklmnopqrstuvwxyz-") != "" {
			continue
		}
		shown = append(shown, m)
	}
	return fmt.Sprintf("Codex managed requirements (allowed sandbox modes [%s])",
		strings.Join(shown, ", ")), true, !allowed
}
