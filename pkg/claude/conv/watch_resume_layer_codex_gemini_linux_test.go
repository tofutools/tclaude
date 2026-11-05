//go:build linux

package conv

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc/agentipctest"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

const resumeConvGeminiLayer = "6b2d8f00-1111-4222-8333-444455556666"

// resumeBehindTclaudeLayer resumes convID's recorded harness behind tclaude's
// sandbox with a stub harness binary on PATH, and returns the pane command,
// the profile the resume would clean up, and the path tclaude pins the
// harness by (Gemini's Node bundle entry, the stub itself otherwise).
func resumeBehindTclaudeLayer(t *testing.T, harnessName, convID string, profile *sandboxpolicy.Profile) (cmd, cleanup, stub string) {
	t.Helper()
	// A constructed root needs the canonical agentd socket live; keep HOME
	// short enough for a Unix socket path.
	home := agentipctest.ShortSocketDir(t)
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	db.ResetForTest()
	t.Cleanup(db.ResetForTest)
	clearAmbientResumeOverride(t)
	socket := agentipc.CanonicalSocketPath()
	require.NoError(t, os.MkdirAll(filepath.Dir(socket), 0o700))
	listener, err := net.Listen("unix", socket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	prevResolve := resolveResumeTclaudeLayer
	resolveResumeTclaudeLayer = func(sandboxpolicy.NetworkPosture, sandboxpolicy.RootPosture,
		sandboxpolicy.NetworkEngine, bool) (string, harness.LaunchOSSandbox, error) {
		return "/usr/bin/bwrap", harness.LaunchOSSandbox{}, nil
	}
	t.Cleanup(func() { resolveResumeTclaudeLayer = prevResolve })
	root := testutil.CanonicalTempDir(t)
	bin := filepath.Join(root, "bin")
	require.NoError(t, os.MkdirAll(bin, 0o755))
	stub = filepath.Join(bin, harnessName)
	require.NoError(t, os.WriteFile(stub, []byte("#!/bin/sh\n"), 0o755))
	if harnessName == harness.GeminiName {
		// An npm install: bin/gemini links to the package's Node bundle, and
		// node is on PATH.
		pkg := filepath.Join(root, "lib", "node_modules", "@google", "gemini-cli")
		require.NoError(t, os.MkdirAll(filepath.Join(pkg, "bundle"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(pkg, "package.json"), []byte(`{
			"name": "@google/gemini-cli", "bin": {"gemini": "bundle/gemini.js"},
			"optionalDependencies": {"@lydell/node-pty": "1"}}`), 0o644))
		require.NoError(t, os.MkdirAll(filepath.Join(root, "lib", "node_modules", "@lydell", "node-pty"), 0o755))
		entry := filepath.Join(pkg, "bundle", "gemini.js")
		require.NoError(t, os.WriteFile(entry, []byte("#!/usr/bin/env node\nimport './x.js';\n"), 0o755))
		require.NoError(t, os.Remove(stub))
		require.NoError(t, os.Symlink(entry, stub))
		require.NoError(t, os.WriteFile(filepath.Join(bin, "node"), []byte("#!/bin/sh\n"), 0o755))
		stub = entry
	}
	if harnessName == harness.CodexName {
		// The shape tclaude pins a Codex install by: a native executable in
		// bin/ with its runtime closure beside it.
		require.NoError(t, os.WriteFile(stub, []byte("\x7fELF stub"), 0o755))
		for _, relative := range []string{"codex-package.json", "codex-path/rg", "codex-resources/bwrap"} {
			path := filepath.Join(root, relative)
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
			require.NoError(t, os.WriteFile(path, nil, 0o755))
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if profile != nil {
		effective, err := sandboxpolicy.Resolve(sandboxpolicy.Scopes{Explicit: profile})
		require.NoError(t, err)
		snapshot := sandboxpolicy.NewSnapshot(effective, nil)
		agentID, _, err := db.EnsureAgentForConv(convID, "test")
		require.NoError(t, err)
		require.NoError(t, db.SetAgentEffectiveSandboxConfig(agentID, &snapshot))
	}
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: convID[:8], ConvID: convID, Cwd: root, Harness: harnessName,
		HarnessBuiltinMode:    harness.SandboxManagedProfile,
		SandboxImplementation: string(sandboxpolicy.ImplementationTclaudeLayer),
	}))
	var resumeErr error
	cmd, cleanup, _, resumeErr = resumeLaunchCmd(harnessName, convID[:8], convID, nil)
	require.NoError(t, resumeErr)
	return cmd, cleanup, stub
}

// A Codex resumed behind tclaude's sandbox runs as `session new` runs it:
// the executable tclaude pinned, with Codex's own sandbox switched off, since
// tclaude's sandbox is the one confining it. Its state root is the
// conversation's CODEX_HOME, mounted writable, and no managed Codex
// permission profile is written or left to clean up.
func TestResumeLaunchCmd_TclaudeLayerCodex(t *testing.T) {
	cmd, cleanup, stub := resumeBehindTclaudeLayer(t, harness.CodexName, resumeConvCodex, nil)
	codexHome := os.Getenv("CODEX_HOME")
	// The mount names the canonical path; the short socket HOME is an alias.
	canonicalHome, err := filepath.EvalSymlinks(os.Getenv("HOME"))
	require.NoError(t, err)
	mounted := filepath.Join(canonicalHome, ".codex")
	assert.Contains(t, cmd, "/usr/bin/bwrap ", "the pane runs behind tclaude's sandbox")
	assert.Contains(t, cmd, "--bind "+mounted+" "+mounted, "the state root is mounted writable")
	assert.Contains(t, cmd, "export CODEX_HOME="+codexHome+";")
	assert.Contains(t, cmd, "; "+stub+" resume "+resumeConvCodex+" --sandbox "+harness.SandboxDangerFull+" ",
		"the pinned executable runs, with Codex's own sandbox off")
	assert.NotContains(t, cmd, " -p "+harness.CodexAgentProfile, "no managed profile behind the layer")
	assert.Empty(t, cleanup, "no managed profile to clean up")
	entries, err := os.ReadDir(mounted)
	if err == nil {
		for _, entry := range entries {
			assert.NotContains(t, entry.Name(), harness.CodexAgentProfile+"-", "no generated profile file")
		}
	}
}

// A Gemini resumed behind tclaude's sandbox runs with Gemini's own sandbox
// off and, as `session new` does, by bare name on the host root and by the
// pinned executable under a constructed root, which cannot see an install
// under the home directory.
func TestResumeLaunchCmd_TclaudeLayerGemini(t *testing.T) {
	t.Run("host root", func(t *testing.T) {
		cmd, cleanup, _ := resumeBehindTclaudeLayer(t, harness.GeminiName, resumeConvGeminiLayer, nil)
		assert.Contains(t, cmd, "/usr/bin/bwrap ")
		assert.Contains(t, cmd, "export GEMINI_SANDBOX=false;", "Gemini's own sandbox is off")
		assert.Contains(t, cmd, "; gemini --resume "+resumeConvGeminiLayer+" ")
		assert.Empty(t, cleanup)
	})
	t.Run("constructed root", func(t *testing.T) {
		cmd, cleanup, stub := resumeBehindTclaudeLayer(t, harness.GeminiName, resumeConvGeminiLayer,
			&sandboxpolicy.Profile{Name: "separate-root", FilesystemRoot: sandboxpolicy.FilesystemRootSeparate})
		assert.Contains(t, cmd, "export GEMINI_SANDBOX=false;")
		assert.Contains(t, cmd, stub+" --resume "+resumeConvGeminiLayer+" ", "the pinned executable runs")
		assert.Empty(t, cleanup)
	})
}
