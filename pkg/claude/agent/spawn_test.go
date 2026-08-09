package agent

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func TestRunSpawn_AskHumanDoesNotClaimPendingPopupBeforeLineageDenial(t *testing.T) {
	prevAvail, prevReq := DaemonAvailableImpl, DaemonRequestImpl
	t.Cleanup(func() { DaemonAvailableImpl, DaemonRequestImpl = prevAvail, prevReq })
	DaemonAvailableImpl = func() bool { return true }
	DaemonRequestImpl = func(_, _ string, _, _ any, opts DaemonOpts) error {
		assert.Equal(t, time.Minute, opts.AskHuman)
		return &DaemonError{
			Status: http.StatusForbidden, Code: "approval_restricted",
			Msg: "approval lineage cannot be overridden by an authorization popup",
		}
	}

	stdout, stderr := new(bytes.Buffer), new(bytes.Buffer)
	resp, rc := RunSpawn(&SpawnParams{
		Group: "alpha", Name: "worker", Harness: "codex", AskHuman: "60s",
	}, stdout, stderr, new(bytes.Buffer))
	assert.Nil(t, resp)
	assert.NotEqual(t, rcOK, rc)
	assert.NotContains(t, stdout.String(), "Waiting", "no popup was known to be pending")
	assert.Contains(t, stdout.String(), "may be requested")
	assert.Contains(t, stderr.String(), "approval lineage")
}

func TestRunSpawn_ExplicitFastModeInheritStaysOnWire(t *testing.T) {
	prevAvail, prevReq := DaemonAvailableImpl, DaemonRequestImpl
	t.Cleanup(func() { DaemonAvailableImpl, DaemonRequestImpl = prevAvail, prevReq })
	DaemonAvailableImpl = func() bool { return true }
	var captured SpawnRequest
	DaemonRequestImpl = func(_, _ string, body, out any, _ DaemonOpts) error {
		captured = body.(SpawnRequest)
		*out.(*SpawnResponse) = SpawnResponse{Group: "alpha", ConvID: "conv-fast-inherit"}
		return nil
	}

	resp, rc := RunSpawn(&SpawnParams{
		Group: "alpha", Name: "worker", Harness: harness.CodexName, FastMode: "inherit",
	}, new(bytes.Buffer), new(bytes.Buffer), new(bytes.Buffer))
	require.Equal(t, rcOK, rc)
	require.NotNil(t, resp)
	assert.Equal(t, "inherit", captured.FastMode,
		"explicit inherit must not serialize like an omitted --fast-mode")
}

func TestRunSpawn_ExplicitCodexSendKeysStaysOnWire(t *testing.T) {
	prevAvail, prevReq := DaemonAvailableImpl, DaemonRequestImpl
	t.Cleanup(func() { DaemonAvailableImpl, DaemonRequestImpl = prevAvail, prevReq })
	DaemonAvailableImpl = func() bool { return true }
	var captured SpawnRequest
	DaemonRequestImpl = func(_, _ string, body, out any, _ DaemonOpts) error {
		captured = body.(SpawnRequest)
		*out.(*SpawnResponse) = SpawnResponse{Group: "alpha", ConvID: "conv-sendkeys"}
		return nil
	}

	resp, rc := RunSpawn(&SpawnParams{
		Group: "alpha", Name: "worker", Harness: harness.CodexName,
		codexAppServerSpecified: true, CodexAppServer: false,
	}, new(bytes.Buffer), new(bytes.Buffer), new(bytes.Buffer))
	require.Equal(t, rcOK, rc)
	require.NotNil(t, resp)
	require.NotNil(t, captured.CodexAppServer)
	assert.False(t, *captured.CodexAppServer,
		"--codex-app-server=false must override an opted-in profile instead of serializing as unset")
}

func TestSpawnCommandRecordsCodexAppServerFlagPresenceAfterParsing(t *testing.T) {
	for _, tc := range []struct {
		name      string
		args      []string
		wantValue bool
		wantSet   bool
	}{
		{name: "unset"},
		{name: "true", args: []string{"--codex-app-server=true"}, wantValue: true, wantSet: true},
		{name: "false", args: []string{"--codex-app-server=false"}, wantSet: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := spawnCmd()
			require.NoError(t, cmd.ParseFlags(tc.args))
			var p SpawnParams
			value, err := cmd.Flags().GetBool("codex-app-server")
			require.NoError(t, err)
			p.CodexAppServer = value
			recordSpawnFlagPresence(&p, cmd)
			assert.Equal(t, tc.wantValue, p.CodexAppServer)
			assert.Equal(t, tc.wantSet, p.codexAppServerSpecified)
		})
	}
}

func TestRunSpawn_ExplicitFalseOverridesOptedInNamedProfile(t *testing.T) {
	prevAvail, prevReq := DaemonAvailableImpl, DaemonRequestImpl
	t.Cleanup(func() { DaemonAvailableImpl, DaemonRequestImpl = prevAvail, prevReq })
	DaemonAvailableImpl = func() bool { return true }
	var captured SpawnRequest
	DaemonRequestImpl = func(method, path string, body, out any, _ DaemonOpts) error {
		if method == http.MethodGet {
			profile := out.(*profileJSON)
			selected := true
			*profile = profileJSON{Name: "ab-opted-in", Harness: harness.CodexName, CodexAppServer: &selected}
			return nil
		}
		captured = body.(SpawnRequest)
		*out.(*SpawnResponse) = SpawnResponse{Group: "alpha", ConvID: "conv-sendkeys-profile"}
		return nil
	}

	resp, rc := RunSpawn(&SpawnParams{
		Group: "alpha", Name: "worker", Profile: "ab-opted-in",
		codexAppServerSpecified: true, CodexAppServer: false,
	}, new(bytes.Buffer), new(bytes.Buffer), new(bytes.Buffer))
	require.Equal(t, rcOK, rc)
	require.NotNil(t, resp)
	require.NotNil(t, captured.CodexAppServer)
	assert.False(t, *captured.CodexAppServer)
	assert.Equal(t, "ab-opted-in", captured.Profile)
}

// A --file brief over the 16384-byte cap is rejected with the same
// error as an oversize --initial-message: the file-input path enforces
// the cap, it is not a way to smuggle a larger brief past it. The
// rejection lands before the daemon is contacted, so this needs no
// running agentd.
func TestRunSpawn_FileBriefRejectedOverCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "huge.txt")
	oversize := strings.Repeat("a", MaxInitialMessageBytes+1)
	require.NoError(t, os.WriteFile(path, []byte(oversize), 0o600))

	stderr := new(bytes.Buffer)
	resp, rc := RunSpawn(
		&SpawnParams{Group: "alpha", File: path},
		new(bytes.Buffer), stderr, new(bytes.Buffer),
	)
	assert.Nil(t, resp)
	assert.Equal(t, rcInvalidArg, rc)
	assert.Contains(t, stderr.String(), "at most", "must surface the cap error")
}

// --initial-message and --file together is a usage error, surfaced
// before any spawn happens.
func TestRunSpawn_FileAndFlagMutuallyExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "brief.txt")
	require.NoError(t, os.WriteFile(path, []byte("file brief"), 0o600))

	stderr := new(bytes.Buffer)
	resp, rc := RunSpawn(
		&SpawnParams{Group: "alpha", InitialMessage: "inline brief", File: path},
		new(bytes.Buffer), stderr, new(bytes.Buffer),
	)
	assert.Nil(t, resp)
	assert.Equal(t, rcInvalidArg, rc)
	assert.Contains(t, stderr.String(), "not both")
}

func TestRunSpawn_SandboxProfileAndOmissionMutuallyExclusive(t *testing.T) {
	stderr := new(bytes.Buffer)
	resp, rc := RunSpawn(
		&SpawnParams{
			Group: "alpha", SandboxProfile: "strict", OmitSandboxProfiles: true,
		},
		new(bytes.Buffer), stderr, new(bytes.Buffer),
	)
	assert.Nil(t, resp)
	assert.Equal(t, rcInvalidArg, rc)
	assert.Contains(t, stderr.String(), "mutually exclusive")
}

func TestValidateSpawnSandboxImplementation_RejectsOpenCodeBuiltinOnlyWhenSet(t *testing.T) {
	opencode, ok := harness.Get(harness.OpenCodeName)
	require.True(t, ok)

	_, err := validateSpawnSandboxImplementation(
		opencode, string(sandboxpolicy.ImplementationHarnessBuiltin))
	require.EqualError(t, err,
		`sandbox implementation "harness-builtin" is invalid for OpenCode: `+
			`OpenCode has no built-in OS sandbox; its access-control mode is a command filter, `+
			`not confinement; use tclaude-layer or spawn with the sandbox off`)

	got, err := validateSpawnSandboxImplementation(opencode, "")
	require.NoError(t, err)
	assert.Empty(t, got, "unset must remain unset for daemon-side tier resolution")
}

// The spawn command's long help must state the default-resolution chain once,
// and the --harness flag help must warn that an unset value is NOT forced to
// claude — the TCL-304 documentation fix.
func TestSpawnHelp_DefaultResolutionDocumented(t *testing.T) {
	cmd := spawnCmd()
	long := cmd.Long
	assert.Contains(t, long, "Default resolution")
	assert.Contains(t, long, "group's default spawn profile")
	assert.Contains(t, long, "global (dashboard) default spawn profile")
	assert.Contains(t, long, "harness's own default")
	assert.Contains(t, long, "full chain FIRST")
	assert.Contains(t, long, "incompatible explicit")
	assert.Contains(t, long, "disclosed in the resolved-shape echo")
	assert.Contains(t, long, "tclaude agent profiles default show")

	harnessFlag := cmd.Flags().Lookup("harness")
	require.NotNil(t, harnessFlag)
	assert.Contains(t, harnessFlag.Usage, "never infer or pin")
	assert.Contains(t, harnessFlag.Usage, "--profile")

	approvalFlag := cmd.Flags().Lookup("ask-for-approval")
	require.NotNil(t, approvalFlag)
	assert.Contains(t, approvalFlag.Usage, "Claude: auto")
	assert.Contains(t, approvalFlag.Usage, "caller")
	assert.NotContains(t, approvalFlag.Usage, "Claude: inherit")
	assert.Contains(t, long, "narrowed from the harness default")
	assert.Contains(t, long, "never silently narrowed")
}

// The spawn command's long help must lead with the profile-first guidance —
// spawning with an operator-preconfigured spawn profile is the primary path,
// and the --profile flag help must say so too.
func TestSpawnHelp_ProfileFirstDocumented(t *testing.T) {
	cmd := spawnCmd()
	assert.True(t, strings.HasPrefix(cmd.Long, "Prefer a spawn profile"),
		"the profile-first guidance must open the long help, not trail it")
	assert.Contains(t, cmd.Long, "preconfigured by the operator")

	profileFlag := cmd.Flags().Lookup("profile")
	require.NotNil(t, profileFlag)
	assert.Contains(t, profileFlag.Usage, "RECOMMENDED")
	assert.Contains(t, profileFlag.Usage, "preconfigured by the operator")
}

// formatResolvedField renders "value (source)" for a pinned field and a bare
// "(harness default)" for an unpinned one.
func TestFormatResolvedField(t *testing.T) {
	assert.Equal(t, `codex (global default profile "x")`,
		formatResolvedField(ResolvedField{Value: "codex", Source: `global default profile "x"`}))
	assert.Equal(t, "(harness default)",
		formatResolvedField(ResolvedField{Value: "", Source: ProvHarnessDefault}))
	// A whitespace-only value is still treated as unpinned.
	assert.Equal(t, "(harness default)",
		formatResolvedField(ResolvedField{Value: "  ", Source: "explicit"}))
}

func TestPrintResolvedLaunchSeparatesInfoFromWarnings(t *testing.T) {
	stdout := new(bytes.Buffer)
	printResolvedLaunch(stdout, &ResolvedLaunch{
		Harness:  ResolvedField{Value: "opencode", Source: "explicit"},
		Info:     []string{"server is confined"},
		Warnings: []string{"action needed"},
	})
	assert.Contains(t, stdout.String(), "Info:    server is confined")
	assert.Contains(t, stdout.String(), "Warning: action needed")
	assert.NotContains(t, stdout.String(), "Warning: server is confined")
}

// A missing --file is rejected before the daemon is even contacted —
// nothing is spawned.
func TestRunSpawn_MissingFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.txt")

	stderr := new(bytes.Buffer)
	resp, rc := RunSpawn(
		&SpawnParams{Group: "alpha", File: missing},
		new(bytes.Buffer), stderr, new(bytes.Buffer),
	)
	assert.Nil(t, resp)
	assert.Equal(t, rcIOFailure, rc)
	assert.Contains(t, stderr.String(), missing, "error must name the unreadable file")
}
