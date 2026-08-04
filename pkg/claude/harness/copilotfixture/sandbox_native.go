package copilotfixture

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This file is the fixture support for characterizing Copilot's OWN sandbox —
// the "command sandboxing" feature behind `copilot help sandbox` — as opposed
// to sandbox_baseline_smoke_test.go, which characterizes what a Copilot run
// touches so tclaude can confine it from the outside. The two are easy to
// confuse and answer opposite questions.

// NativeSandboxSettings is the `sandbox` object a scenario writes into the
// settings under COPILOT_HOME — settings.json by default; see
// WriteNativeSandboxSettingsTo for the legacy file. It mirrors the CLI's own
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

// Settings file names under COPILOT_HOME. BOTH are live inputs to the sandbox
// posture and they are NOT interchangeable — see
// TestCopilotNativeSandboxSettingsSourcesAndPrecedence, which measures that
// config.json wins and is migrated into settings.json on startup. Anything that
// inspects a Copilot sandbox posture has to know both names.
const (
	// NativeSettingsFile is the canonical live settings file.
	NativeSettingsFile = "settings.json"
	// NativeLegacySettingsFile is the legacy file the CLI migrates FROM. It
	// takes precedence for the launch that consumes it and rewrites the
	// canonical file, which makes it the bypass-relevant one.
	//
	// The migration is a SHALLOW merge, and the shallowness matters to anyone
	// modelling the precedence: top-level keys the legacy file never mentions
	// survive from the canonical file, but a top-level key it does mention has
	// its whole VALUE replaced. So a legacy file carrying any `sandbox` object
	// at all discards the canonical file's `sandbox` object entirely — a
	// canonical `sandbox.enabled: true` does not survive a legacy `sandbox`
	// block that only sets some other field. Merging the two objects key by key
	// would model a posture the CLI does not produce.
	NativeLegacySettingsFile = "config.json"
)

// WriteNativeSandboxSettings writes the scenario's posture into the canonical
// settings file under COPILOT_HOME.
//
// Writing a file before the run is how a scenario chooses a sandbox posture at
// all: the feature has NO launch flag, which is itself one of the findings this
// suite exists to pin.
func WriteNativeSandboxSettings(t *testing.T, dirs Dirs, sandbox NativeSandboxSettings) {
	t.Helper()
	WriteNativeSandboxSettingsTo(t, dirs, NativeSettingsFile, sandbox)
}

// WriteNativeSandboxSettingsTo writes the posture into a named settings file
// under COPILOT_HOME, so a scenario can exercise either source or both.
func WriteNativeSandboxSettingsTo(
	t *testing.T, dirs Dirs, fileName string, sandbox NativeSandboxSettings,
) {
	t.Helper()
	encoded, err := json.MarshalIndent(map[string]any{"sandbox": sandbox}, "", "  ")
	if err != nil {
		t.Fatalf("copilotfixture: encoding sandbox settings: %v", err)
	}
	path := filepath.Join(dirs.Home, fileName)
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatalf("copilotfixture: writing %s: %v", path, err)
	}
}

// nativeSandboxBackendFailureMarkers are substrings Copilot emits as a shell
// tool RESULT when its sandbox backend could not be started at all, as opposed
// to when a policy refused a particular command.
//
// They are matched on the run's own output rather than on a host probe; see
// ClassifyNativeSandboxBackend for why that distinction is load-bearing.
var nativeSandboxBackendFailureMarkers = []string{
	// Linux, bubblewrap: no permission to create a user namespace.
	"bwrap:",
	"Bubblewrap",
	"new namespace",
	// Either platform: the backend binary is absent.
	"not installed or not on PATH",
	// macOS, Seatbelt.
	"sandbox-exec",
}

// NativeSandboxBackendVerdict is what a scenario could establish about
// Copilot's OS sandbox backend FROM THE RUN IT JUST PERFORMED.
type NativeSandboxBackendVerdict struct {
	// Up reports that the backend started and ran the shell command.
	Up bool
	// Evidence is the observation behind the verdict, for the log line CI
	// greps and for a human reading a failure.
	Evidence string
}

// ClassifyNativeSandboxBackend decides whether Copilot's OS sandbox backend
// started, from a probe shell command the scenario itself ran inside a granted
// path.
//
// It replaced an out-of-band `bwrap` probe, and the reason is worth stating
// because the earlier design looked more direct: a separate `bwrap` invocation
// runs a DIFFERENT policy from the one Copilot builds, so the two can disagree.
// Copilot's own start additionally configures netfilter inside the namespace
// under a closed-network policy, which a bare `bwrap --ro-bind / / true` never
// exercises — so the probe could report a usable backend for a run that then
// failed closed, and the scenario would assert enforcement against an
// environment failure. The probe was also a STUB on macOS, returning "available"
// without measuring anything, which made the CI gate that requires a usable
// backend tautologically true on exactly the platform the matrix was added for.
//
// Deriving the verdict from the run under test fixes both, and works the same
// way on every platform.
//
// Three outcomes, and the third is why this returns an error rather than a
// bool: a granted write that neither landed nor reported a backend failure is
// UNCLASSIFIABLE, and a scenario must fail on it instead of silently choosing
// an arm. That case means the sandbox refused a path the policy grants, which
// is a finding in its own right and must not be laundered into "backend down".
func ClassifyNativeSandboxBackend(
	grantedWriteLanded bool, shellResult string,
) (NativeSandboxBackendVerdict, error) {
	backendFailure := ""
	for _, marker := range nativeSandboxBackendFailureMarkers {
		if strings.Contains(shellResult, marker) {
			backendFailure = marker
			break
		}
	}
	switch {
	case grantedWriteLanded:
		return NativeSandboxBackendVerdict{
			Up:       true,
			Evidence: "a shell write inside a granted path landed, so the backend started",
		}, nil
	case backendFailure != "":
		return NativeSandboxBackendVerdict{
			Up: false,
			Evidence: fmt.Sprintf(
				"the shell tool reported a backend-start failure (%q): %s",
				backendFailure, strings.TrimSpace(shellResult)),
		}, nil
	default:
		return NativeSandboxBackendVerdict{}, fmt.Errorf(
			"cannot classify Copilot's sandbox backend: a shell write inside a GRANTED "+
				"path neither landed nor reported a backend-start failure. The sandbox "+
				"refused a path its own policy grants, which is neither of the two arms "+
				"this suite knows how to assert. Tool result: %s",
			strings.TrimSpace(shellResult))
	}
}
