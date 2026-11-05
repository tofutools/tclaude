package agentd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
	tclcommon "github.com/tofutools/tclaude/pkg/common"
)

// These pin openCodeRuntimeSandboxSpec's verdicts on a persisted runtime: the
// accept cases, and each refusal reached by breaking exactly one thing in an
// otherwise valid tclaude-layer runtime. Reconcile and restart both refuse to
// relaunch on any of these, so the message is the operator-visible contract.

// openCodePinnedLayerRuntime is a valid, freshly persisted tclaude-layer
// runtime and the spec it carries.
func openCodePinnedLayerRuntime(t *testing.T) (db.OpenCodeRuntime, session.TclaudeLayerLaunchSpec) {
	t.Helper()
	setupTestDB(t)
	home, err := filepath.EvalSymlinks(os.Getenv("HOME"))
	require.NoError(t, err)
	t.Setenv("HOME", home)
	cwd := filepath.Join(home, "project")
	require.NoError(t, os.MkdirAll(cwd, 0o700))
	_, _, err = tclcommon.PrepareSpawnAttachmentsPrivateDir("spwn-opencode-pin")
	require.NoError(t, err)
	agentID := db.NewAgentID()
	_, err = allocatePrivateOpenCodeState(agentID)
	require.NoError(t, err)
	spec, err := openCodeTclaudeLayerLaunchSpec(
		string(sandboxpolicy.ImplementationTclaudeLayer), cwd, nil, nil,
		agentID, "spwn-opencode-pin")
	require.NoError(t, err)
	require.NotNil(t, spec)
	implementation, encoded, err := openCodeSandboxRecord(
		string(sandboxpolicy.ImplementationTclaudeLayer), spec)
	require.NoError(t, err)
	return db.OpenCodeRuntime{
		Cwd:                   cwd,
		SandboxImplementation: implementation,
		SandboxLaunchSpecJSON: encoded,
		Transport:             db.OpenCodeTransportLoopbackTCP,
	}, *spec
}

func TestOpenCodeRuntimeSandboxSpec_AcceptsAValidPersistedRuntime(t *testing.T) {
	runtime, spec := openCodePinnedLayerRuntime(t)

	decoded, err := openCodeRuntimeSandboxSpec(runtime)

	require.NoError(t, err)
	require.NotNil(t, decoded)
	assert.Equal(t, spec.Version, decoded.Version)
	assert.Equal(t, spec.Contract, decoded.Contract)
}

func TestOpenCodeRuntimeSandboxSpec_NonLayerImplementationsCarryNoSpec(t *testing.T) {
	for _, implementation := range []sandboxpolicy.Implementation{
		sandboxpolicy.ImplementationHarnessBuiltin,
		sandboxpolicy.ImplementationOff,
		sandboxpolicy.ImplementationResourceOnly,
	} {
		t.Run(string(implementation), func(t *testing.T) {
			decoded, err := openCodeRuntimeSandboxSpec(db.OpenCodeRuntime{
				SandboxImplementation: string(implementation),
				Transport:             db.OpenCodeTransportLoopbackTCP,
			})
			require.NoError(t, err)
			assert.Nil(t, decoded)
		})
	}
}

func TestOpenCodeRuntimeSandboxSpec_RefusesEachBrokenPiece(t *testing.T) {
	type breakage struct {
		name    string
		runtime func(*db.OpenCodeRuntime)
		spec    func(*session.TclaudeLayerLaunchSpec)
		raw     func(string) string
		refusal string
	}
	for _, tc := range []breakage{
		{name: "transport authority", runtime: func(r *db.OpenCodeRuntime) {
			r.ControlSocketPath = "/run/stray.sock"
		}, refusal: "OpenCode runtime transport authority: loopback OpenCode runtime unexpectedly carries Unix socket authority"},
		{name: "unknown implementation", runtime: func(r *db.OpenCodeRuntime) {
			r.SandboxImplementation = "no-such-sandbox"
		}, refusal: "OpenCode runtime sandbox implementation: "},
		{name: "a non-layer runtime carrying a spec", runtime: func(r *db.OpenCodeRuntime) {
			r.SandboxImplementation = string(sandboxpolicy.ImplementationOff)
		}, refusal: "OpenCode off runtime unexpectedly carries a tclaude sandbox launch spec"},
		{name: "no persisted spec", raw: func(string) string { return "  " },
			refusal: "has no persisted launch spec; refusing an unwrapped restart"},
		{name: "oversized spec", raw: func(string) string {
			return `{"pad":"` + strings.Repeat("x", openCodeSandboxSpecMax) + `"}`
		}, refusal: "sandbox launch spec exceeds 4194304 bytes"},
		{name: "unknown field", raw: func(raw string) string {
			return `{"no_such_field":1,` + strings.TrimPrefix(raw, "{")
		}, refusal: "decode OpenCode with tclaude’s sandbox launch spec: json: unknown field"},
		{name: "trailing value", raw: func(raw string) string { return raw + " {}" },
			refusal: "sandbox launch spec trailer: multiple JSON values"},
		{name: "unsupported version", spec: func(s *session.TclaudeLayerLaunchSpec) {
			s.Version = 99
		}, refusal: "unsupported OpenCode with tclaude’s sandbox launch spec version 99"},
		{name: "another harness", spec: func(s *session.TclaudeLayerLaunchSpec) {
			s.Contract.HarnessName = harness.CodexName
		}, refusal: `launch spec names harness "codex"`},
		{name: "relative state root", spec: func(s *session.TclaudeLayerLaunchSpec) {
			s.Contract.StateRoot = "relative/state"
		}, refusal: `launch spec state root "relative/state" is not absolute`},
		{name: "no state directories", spec: func(s *session.TclaudeLayerLaunchSpec) {
			s.Contract.StateDirs = nil
		}, refusal: "launch spec has no mutable state directories"},
		{name: "relative state directory", spec: func(s *session.TclaudeLayerLaunchSpec) {
			s.Contract.StateDirs = append(s.Contract.StateDirs, "relative/dir")
		}, refusal: "launch spec has a non-absolute state directory"},
		{name: "state directory outside the write contract", spec: func(s *session.TclaudeLayerLaunchSpec) {
			s.Contract.StateDirs = append(s.Contract.StateDirs, "/pin/not-writable")
		}, refusal: `sandbox state directory "/pin/not-writable" is not in the writable launch contract`},
		{name: "executable state unprotected", spec: func(s *session.TclaudeLayerLaunchSpec) {
			s.Contract.ReadOnlyStateDirs = nil
			s.Contract.ReadOnlyBinds = nil
		}, refusal: "launch spec does not protect its executable state"},
		{name: "read-only state is the state root", spec: func(s *session.TclaudeLayerLaunchSpec) {
			s.Contract.ReadOnlyStateDirs = []string{s.Contract.StateRoot}
		}, refusal: "launch spec has invalid read-only state directory"},
		{name: "read-only state not read-only in the policy", spec: func(s *session.TclaudeLayerLaunchSpec) {
			s.Contract.ReadOnlyStateDirs = append(s.Contract.ReadOnlyStateDirs,
				filepath.Join(s.Contract.StateDirs[0], "pin-unprotected"))
		}, refusal: "is not protected in the rendered contract"},
		{name: "cwd not a writable contract path", runtime: func(r *db.OpenCodeRuntime) {
			r.Cwd = "/pin/elsewhere"
		}, refusal: `launch spec does not preserve runtime cwd "/pin/elsewhere" as a writable contract path`},
		{name: "Unix-relay version on a loopback runtime", spec: func(s *session.TclaudeLayerLaunchSpec) {
			s.Version = session.TclaudeLayerUnixRelaySpecVersion
		}, refusal: "sandbox v4 runtime does not match its Unix-relay authority"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtime, spec := openCodePinnedLayerRuntime(t)
			if tc.spec != nil {
				tc.spec(&spec)
				encoded, err := json.Marshal(spec)
				require.NoError(t, err)
				runtime.SandboxLaunchSpecJSON = string(encoded)
			}
			if tc.raw != nil {
				runtime.SandboxLaunchSpecJSON = tc.raw(runtime.SandboxLaunchSpecJSON)
			}
			if tc.runtime != nil {
				tc.runtime(&runtime)
			}

			decoded, err := openCodeRuntimeSandboxSpec(runtime)

			require.ErrorContains(t, err, tc.refusal)
			assert.Nil(t, decoded)
		})
	}
}
