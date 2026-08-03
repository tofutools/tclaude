package copilotfixture

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// This file is the fixture support for characterizing Copilot's OWN sandbox —
// the "command sandboxing" feature behind `copilot help sandbox` — as opposed
// to sandbox_baseline_smoke_test.go, which characterizes what a Copilot run
// touches so tclaude can confine it from the outside. The two are easy to
// confuse and answer opposite questions.

// NativeSandboxSettings is the `sandbox` object tclaude writes into
// COPILOT_HOME/config.json for a scenario. It mirrors the CLI's own
// SandboxConfig schema (schemas/api.schema.json in the unpacked payload) rather
// than inventing a shape, so a schema change shows up as a scenario that stops
// behaving instead of one that silently writes an ignored key.
type NativeSandboxSettings struct {
	Enabled                    bool                    `json:"enabled"`
	AddCurrentWorkingDirectory bool                    `json:"addCurrentWorkingDirectory"`
	AllowBypass                bool                    `json:"allowBypass"`
	UserPolicy                 NativeSandboxUserPolicy `json:"userPolicy"`
}

// NativeSandboxUserPolicy is the user-managed fragment merged into the CLI's
// auto-discovered base policy.
type NativeSandboxUserPolicy struct {
	Filesystem NativeSandboxFilesystem `json:"filesystem"`
	Network    NativeSandboxNetwork    `json:"network"`
}

// NativeSandboxFilesystem carries the path grants and denials.
type NativeSandboxFilesystem struct {
	ReadWritePaths []string `json:"readwritePaths,omitempty"`
	ReadOnlyPaths  []string `json:"readonlyPaths,omitempty"`
	DeniedPaths    []string `json:"deniedPaths,omitempty"`
}

// NativeSandboxNetwork carries the outbound switches.
type NativeSandboxNetwork struct {
	AllowOutbound     bool `json:"allowOutbound"`
	AllowLocalNetwork bool `json:"allowLocalNetwork"`
}

// WriteNativeSandboxSettings writes COPILOT_HOME/config.json for a scenario.
//
// config.json is the settings file the CLI reads out of COPILOT_HOME; the
// baseline golden records it as the file a plain run creates there. Writing it
// before the run is how a scenario chooses a sandbox posture at all: the
// feature has NO launch flag, which is itself one of the findings this suite
// exists to pin.
func WriteNativeSandboxSettings(t *testing.T, dirs Dirs, sandbox NativeSandboxSettings) {
	t.Helper()
	encoded, err := json.MarshalIndent(map[string]any{"sandbox": sandbox}, "", "  ")
	if err != nil {
		t.Fatalf("copilotfixture: encoding sandbox settings: %v", err)
	}
	path := filepath.Join(dirs.Home, "config.json")
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatalf("copilotfixture: writing %s: %v", path, err)
	}
}

// NativeSandboxBackendAvailable reports whether this host can actually START
// the OS backend Copilot's sandbox uses, together with the evidence for the
// answer.
//
// It exists so the shell scenario can assert something real on BOTH kinds of
// host instead of skipping on one of them. On Linux the backend is bubblewrap,
// and `bwrap` being on PATH is not enough: it also needs permission to create
// an unprivileged user namespace, which a hardened kernel, a container, or an
// AppArmor profile (Ubuntu's apparmor_restrict_unprivileged_userns) can refuse.
// A test that skipped there would report "no coverage" as "fine"; a test that
// asserted enforcement there would fail for the environment rather than for the
// CLI. Measuring the host lets the scenario assert enforcement where the
// backend runs and fail-closed degradation where it does not.
//
// The probe is the mechanism itself rather than a proxy for it — reading
// sysctls would answer a different, weaker question than "can a namespace be
// created right now".
func NativeSandboxBackendAvailable(t *testing.T) (bool, string) {
	t.Helper()
	if runtime.GOOS != "linux" {
		// Seatbelt needs no namespace and `sandbox-exec` ships with macOS, so
		// the Darwin backend is assumed present; a Darwin run that contradicts
		// this fails in the scenario, which is where it should be visible.
		return true, "non-Linux host: Copilot's backend here is Seatbelt, which needs no namespace"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "bwrap",
		"--ro-bind", "/", "/", "--proc", "/proc", "--dev", "/dev",
		"true").CombinedOutput()
	if err != nil {
		return false, "bwrap could not start a namespace: " + string(out)
	}
	return true, "bwrap started a namespace"
}
