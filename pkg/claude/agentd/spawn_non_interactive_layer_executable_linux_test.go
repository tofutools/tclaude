//go:build linux

package agentd

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc/agentipctest"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// oneShotLayerFixture is a host with fake harness executables on PATH and
// the launcher probe stubbed, so wrapOneShotLayer renders its command
// without bwrap installed.
type oneShotLayerFixture struct {
	home, root, bin string
}

func newOneShotLayerFixture(t *testing.T) oneShotLayerFixture {
	t.Helper()
	// Short enough for a Unix socket path: a constructed root needs the
	// canonical agentd socket live.
	f := oneShotLayerFixture{home: agentipctest.ShortSocketDir(t), root: testutil.CanonicalTempDir(t)}
	t.Setenv("HOME", f.home)
	socket := agentipc.CanonicalSocketPath()
	require.NoError(t, os.MkdirAll(filepath.Dir(socket), 0o700))
	listener, err := net.Listen("unix", socket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv(harness.PiAgentDirEnvVar, filepath.Join(f.home, "operator-pi"))
	f.bin = filepath.Join(f.root, "bin")
	require.NoError(t, os.MkdirAll(f.bin, 0o755))
	t.Setenv("PATH", f.bin+string(os.PathListSeparator)+"/usr/bin:/bin")
	prev := resolveOneShotLayerServer
	resolveOneShotLayerServer = func(sandboxpolicy.NetworkPosture, sandboxpolicy.RootPosture,
		sandboxpolicy.NetworkEngine) (string, harness.LaunchOSSandbox, error) {
		return "/usr/bin/bwrap", harness.LaunchOSSandbox{}, nil
	}
	t.Cleanup(func() { resolveOneShotLayerServer = prev })
	return f
}

// claude installs a native Claude: a symlink on PATH to a versioned file.
func (f oneShotLayerFixture) claude(t *testing.T) string {
	t.Helper()
	real := filepath.Join(f.root, "share", "claude", "versions", "9.9.9")
	require.NoError(t, os.MkdirAll(filepath.Dir(real), 0o755))
	require.NoError(t, os.WriteFile(real, []byte("#!/bin/sh\n"), 0o755))
	require.NoError(t, os.Symlink(real, filepath.Join(f.bin, "claude")))
	return real
}

// codex installs a native Codex with its runtime closure.
func (f oneShotLayerFixture) codex(t *testing.T) (native, runtimeRoot string) {
	t.Helper()
	runtimeRoot = filepath.Join(f.root, "codex-native")
	native = filepath.Join(runtimeRoot, "bin", "codex")
	for _, path := range []string{
		filepath.Join(runtimeRoot, "codex-package.json"),
		filepath.Join(runtimeRoot, "codex-path", "rg"),
		filepath.Join(runtimeRoot, "codex-resources", "bwrap"),
	} {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte("x"), 0o755))
	}
	require.NoError(t, os.MkdirAll(filepath.Dir(native), 0o755))
	require.NoError(t, os.WriteFile(native, []byte("\x7fELF"), 0o755))
	require.NoError(t, os.Symlink(native, filepath.Join(f.bin, "codex")))
	return native, runtimeRoot
}

func oneShotLayerSnapshot(t *testing.T, profile sandboxpolicy.Profile) *sandboxpolicy.Snapshot {
	t.Helper()
	effective, err := sandboxpolicy.Resolve(sandboxpolicy.Scopes{Global: &profile})
	require.NoError(t, err)
	snapshot := sandboxpolicy.NewSnapshot(effective, nil)
	return &snapshot
}

// mainOneShotLayerCommand renders a Claude or Codex one-shot exactly as
// wrapOneShotLayer did before it pinned executables through
// session.ResolveTclaudeLayerExecutable: resolve the executable first, build
// the launch spec once with its read path, prepare the harness state, then
// wrap. It is the byte-for-byte reference for the shared-step refactor, built
// from the same session primitives on the same host, so host-specific mounts
// (a constructed root's /opt or resolver config) cancel out.
func mainOneShotLayerCommand(
	t *testing.T, h *harness.Harness, cwd string, snapshot *sandboxpolicy.Snapshot, argv []string,
) string {
	t.Helper()
	argv = append([]string(nil), argv...)
	var readPaths []string
	switch h.Name {
	case harness.DefaultName:
		resolved, err := harness.ResolveClaudeLaunchExecutable()
		require.NoError(t, err)
		argv[0] = resolved.Path
		readPaths = append(readPaths, resolved.Path)
	case harness.CodexName:
		resolved, err := harness.ResolveCodexLaunchExecutable()
		require.NoError(t, err)
		argv[0] = resolved.Path
		readPaths = append(readPaths, resolved.RuntimeRoot)
	default:
		t.Fatalf("no main reference for harness %s", h.Name)
	}
	spec, err := session.BuildTclaudeLayerLaunchSpec(session.TclaudeLayerLaunchInput{
		HarnessName: h.Name, Cwd: cwd, Snapshot: snapshot, HarnessReadPaths: readPaths,
	})
	require.NoError(t, err)
	posture, err := session.TclaudeLayerNetworkPosture(spec.Effective)
	require.NoError(t, err)
	root, err := session.TclaudeLayerRootPosture(posture, spec.Effective)
	require.NoError(t, err)
	require.NoError(t, session.PrepareTclaudeLayerHarnessState(spec))
	binary, _, err := resolveOneShotLayerServer(posture, root, spec.Contract.NetworkEngine)
	require.NoError(t, err)
	command, err := session.WrapTclaudeLayerServerSpec(binary, spec, "exec "+quoteOneShotArgv(argv))
	require.NoError(t, err)
	return command
}

// A one-shot Claude or Codex behind tclaude's sandbox starts the resolved
// executable, with its runtime readable, and its whole rendered command (the
// frozen launch spec included) stays byte-identical across refactors of how
// the executable is pinned.
func TestWrapOneShotLayer_ClaudeAndCodexPinTheResolvedExecutable(t *testing.T) {
	for _, tc := range []struct {
		name    string
		harness string
		profile sandboxpolicy.Profile
	}{
		{"claude-host", harness.DefaultName, sandboxpolicy.Profile{Name: "host"}},
		{"claude-constructed", harness.DefaultName,
			sandboxpolicy.Profile{Name: "separate", FilesystemRoot: sandboxpolicy.FilesystemRootSeparate}},
		{"codex-host", harness.CodexName, sandboxpolicy.Profile{Name: "host"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newOneShotLayerFixture(t)
			var entry, readPath string
			if tc.harness == harness.CodexName {
				entry, readPath = f.codex(t)
			} else {
				entry = f.claude(t)
				readPath = entry
			}
			h, ok := harness.Get(tc.harness)
			require.True(t, ok)
			cwd := filepath.Join(f.root, "work")
			require.NoError(t, os.MkdirAll(cwd, 0o755))
			argv := []string{h.Spawn.Binary(), "-p", "hello"}

			snapshot := oneShotLayerSnapshot(t, tc.profile)
			command, err := wrapOneShotLayer(h, cwd, snapshot, nil, append([]string(nil), argv...), "")
			require.NoError(t, err)
			assert.Contains(t, command, entry, "the resolved executable is the exec token")
			assert.Contains(t, command, readPath, "its runtime is readable inside the sandbox")
			assert.Equal(t, mainOneShotLayerCommand(t, h, cwd, snapshot, argv), command,
				"the whole rendered command, and the frozen launch spec it carries, must stay byte-identical")
		})
	}
}

// nativeStub puts a native build of a harness CLI on PATH.
func (f oneShotLayerFixture) nativeStub(t *testing.T, name string) string {
	t.Helper()
	real := filepath.Join(f.root, "opt", name, name)
	require.NoError(t, os.MkdirAll(filepath.Dir(real), 0o755))
	require.NoError(t, os.WriteFile(real, []byte("\x7fELF"), 0o755))
	require.NoError(t, os.Symlink(real, filepath.Join(f.bin, name)))
	return real
}

// pi and Gemini are pinned only under a constructed root, which omits the
// host's PATH; a host-inherited root keeps running the bare name, so the
// pane's own PATH and pre-launch still pick the binary.
func TestWrapOneShotLayer_PiAndGeminiPinOnlyUnderAConstructedRoot(t *testing.T) {
	for _, name := range []string{harness.PiName, harness.GeminiName} {
		for _, constructed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/constructed=%t", name, constructed), func(t *testing.T) {
				f := newOneShotLayerFixture(t)
				entry := f.nativeStub(t, name)
				h, ok := harness.Get(name)
				require.True(t, ok)
				cwd := filepath.Join(f.root, "work")
				require.NoError(t, os.MkdirAll(cwd, 0o755))
				profile := sandboxpolicy.Profile{Name: "host"}
				if constructed {
					profile = sandboxpolicy.Profile{Name: "separate", FilesystemRoot: sandboxpolicy.FilesystemRootSeparate}
				}
				argv := []string{h.Spawn.Binary(), "-p", "hello"}

				command, err := wrapOneShotLayer(h, cwd, oneShotLayerSnapshot(t, profile), nil, argv, "")
				require.NoError(t, err)
				if constructed {
					assert.Contains(t, command, entry+" -p hello", "the resolved executable is the exec token")
					assert.Contains(t, command, "--ro-bind "+entry+" "+entry, "and it is mounted read-only")
				} else {
					assert.Contains(t, command, " "+h.Spawn.Binary()+" -p hello", "the bare name resolves on the host's PATH")
					assert.NotContains(t, command, entry)
				}
			})
		}
	}
}
