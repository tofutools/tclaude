package harness

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// envMap turns a fixed map into the Getenv function the baseline takes, so a
// test never depends on the developer's real environment.
func envMap(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

func entryByID(t *testing.T, entries []CopilotBaselineEntry, id string) CopilotBaselineEntry {
	t.Helper()
	for _, e := range entries {
		if e.ID == id {
			return e
		}
	}
	t.Fatalf("baseline has no entry %q", id)
	return CopilotBaselineEntry{}
}

func hasEntry(entries []CopilotBaselineEntry, id string) bool {
	for _, e := range entries {
		if e.ID == id {
			return true
		}
	}
	return false
}

// TestCopilotSandboxBaselineLinuxDefaults pins the resolution an operator gets
// with no Copilot variables set at all — the common case, and the one whose
// paths the operator decision names.
func TestCopilotSandboxBaselineLinuxDefaults(t *testing.T) {
	home := t.TempDir()
	entries, err := CopilotSandboxBaseline(CopilotBaselineInput{
		GOOS:   "linux",
		Home:   home,
		Getenv: envMap(nil),
	})
	require.NoError(t, err)

	state := entryByID(t, entries, CopilotBaselineStateDir)
	assert.Equal(t, filepath.Join(home, ".copilot"), state.Path)
	assert.Equal(t, CopilotGrantMandatory, state.Necessity)
	assert.Equal(t, "rw", state.Access.String())

	cache := entryByID(t, entries, CopilotBaselinePackageCache)
	assert.Equal(t, filepath.Join(home, ".cache", "copilot"), cache.Path,
		"the operator decision names exactly this directory")
	assert.Equal(t, CopilotGrantMandatory, cache.Necessity)
	assert.Equal(t, "rwx", cache.Access.String(),
		"the CLI runs the bundled ripgrep and prebuilt native modules out of the cache")

	device := entryByID(t, entries, CopilotBaselineDeviceIDCache)
	assert.Equal(t, filepath.Join(home, ".cache", "Microsoft", "DeveloperTools"), device.Path)
	assert.Equal(t, CopilotGrantBestEffort, device.Necessity,
		"a read-only device-id cache still launches")

	// Feature rows are absent unless the caller supplies their inputs: a
	// launch that coordinates with no daemon must not carry a socket grant.
	assert.False(t, hasEntry(entries, CopilotBaselineAgentdSocket))
	assert.False(t, hasEntry(entries, CopilotBaselineTclaudeBinary))
	assert.False(t, hasEntry(entries, CopilotBaselineTempDir))
	assert.False(t, hasEntry(entries, CopilotBaselineExecutable))
}

// TestCopilotSandboxBaselineDarwinSplit pins the platform difference that is
// easiest to get wrong: on macOS the two caches land in two DIFFERENT Library
// trees and neither is XDG-shaped. The package cache follows Copilot's own
// resolver to ~/Library/Caches/copilot; the device-id file follows the
// Microsoft device-id convention to ~/Library/Application Support.
//
// The XDG_CACHE_HOME below is the point of the case rather than noise: the
// real darwin fixture run sets it, and the runtime wrote to Application
// Support anyway. A resolver that honored it here would grant a directory the
// CLI never touches and leave the one it does touch denied.
func TestCopilotSandboxBaselineDarwinSplit(t *testing.T) {
	home := t.TempDir()
	entries, err := CopilotSandboxBaseline(CopilotBaselineInput{
		GOOS: "darwin",
		Home: home,
		Getenv: envMap(map[string]string{
			"XDG_CACHE_HOME": filepath.Join(home, "xdg"),
		}),
	})
	require.NoError(t, err)

	assert.Equal(t, filepath.Join(home, "Library", "Caches", "copilot"),
		entryByID(t, entries, CopilotBaselinePackageCache).Path)
	assert.Equal(t,
		filepath.Join(home, "Library", "Application Support", "Microsoft", "DeveloperTools"),
		entryByID(t, entries, CopilotBaselineDeviceIDCache).Path)
	assert.Equal(t, filepath.Join(home, ".copilot"),
		entryByID(t, entries, CopilotBaselineStateDir).Path)
}

// TestCopilotSandboxBaselineEnvOverrides proves each override is honored and
// reported through its own Source, including the undocumented cache variable
// the fixture lab depends on.
func TestCopilotSandboxBaselineEnvOverrides(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	state := filepath.Join(root, "state")
	cache := filepath.Join(root, "cache")
	xdg := filepath.Join(root, "xdg")

	entries, err := CopilotSandboxBaseline(CopilotBaselineInput{
		GOOS: "linux",
		Home: home,
		Getenv: envMap(map[string]string{
			CopilotHomeEnvVar:      state,
			CopilotCacheHomeEnvVar: cache,
			"XDG_CACHE_HOME":       xdg,
		}),
	})
	require.NoError(t, err)

	assert.Equal(t, state, entryByID(t, entries, CopilotBaselineStateDir).Path)
	assert.Equal(t, CopilotHomeEnvVar, entryByID(t, entries, CopilotBaselineStateDir).Source)
	assert.Equal(t, cache, entryByID(t, entries, CopilotBaselinePackageCache).Path)
	assert.Equal(t, filepath.Join(xdg, "Microsoft", "DeveloperTools"),
		entryByID(t, entries, CopilotBaselineDeviceIDCache).Path,
		"COPILOT_CACHE_HOME does not move the device-id cache; XDG_CACHE_HOME does")
}

// TestCopilotSandboxBaselineDarwinCacheOverride pins that the explicit cache
// variable wins over the macOS default, which is what lets a fixture run stay
// hermetic on a developer's Mac.
func TestCopilotSandboxBaselineDarwinCacheOverride(t *testing.T) {
	root := t.TempDir()
	entries, err := CopilotSandboxBaseline(CopilotBaselineInput{
		GOOS: "darwin",
		Home: filepath.Join(root, "home"),
		Getenv: envMap(map[string]string{
			CopilotCacheHomeEnvVar: filepath.Join(root, "cache"),
		}),
	})
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(root, "cache"),
		entryByID(t, entries, CopilotBaselinePackageCache).Path)
}

// TestCopilotSandboxBaselineFeatureRows proves the conditional rows appear
// with their feature recorded when the caller supplies the inputs.
func TestCopilotSandboxBaselineFeatureRows(t *testing.T) {
	root := t.TempDir()
	socket := filepath.Join(root, ".tclaude", "api", "agentd.sock")
	entries, err := CopilotSandboxBaseline(CopilotBaselineInput{
		GOOS:              "linux",
		Home:              filepath.Join(root, "home"),
		Getenv:            envMap(nil),
		TempDir:           filepath.Join(root, "tmp"),
		AgentdSockets:     []string{socket, "", "  ", socket, socket + "/"},
		TclaudeExecutable: filepath.Join(root, "bin", "tclaude"),
		CopilotExecutable: filepath.Join(root, "bin", "copilot"),
	})
	require.NoError(t, err)

	sock := entryByID(t, entries, CopilotBaselineAgentdSocket)
	assert.Equal(t, socket, sock.Path)
	assert.Equal(t, CopilotGrantFeatureConditional, sock.Necessity)
	assert.NotEmpty(t, sock.Feature)
	assert.Equal(t, "rw", sock.Access.String(), "connect(2) on a Unix socket needs both")

	assert.Equal(t, CopilotGrantFeatureConditional,
		entryByID(t, entries, CopilotBaselineTempDir).Necessity)
	assert.Equal(t, "rx", entryByID(t, entries, CopilotBaselineTclaudeBinary).Access.String())
	assert.Equal(t, CopilotGrantMandatory,
		entryByID(t, entries, CopilotBaselineExecutable).Necessity)

	// Blank socket entries are dropped rather than turned into empty grants,
	// and a repeated endpoint yields ONE row: the list is
	// canonical-plus-retained-legacy, so a caller assembling it from separate
	// resolvers can legitimately hand over the same path twice — including in
	// a different spelling, which is why the dedup key is the cleaned path.
	count := 0
	for _, e := range entries {
		if e.ID == CopilotBaselineAgentdSocket {
			count++
		}
	}
	assert.Equal(t, 1, count)

	for _, e := range entries {
		if e.Necessity == CopilotGrantFeatureConditional {
			assert.NotEmpty(t, e.Feature, "entry %q is conditional on an unnamed feature", e.ID)
		} else {
			assert.Empty(t, e.Feature, "entry %q names a feature but is not conditional", e.ID)
		}
	}
}

// TestCopilotSandboxBaselineRefusesBroadGrants is the fail-closed proof. Every
// case here is reachable from an operator's own environment, and every one of
// them would silently turn a confined launch into an open one.
func TestCopilotSandboxBaselineRefusesBroadGrants(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")

	cases := []struct {
		name      string
		in        CopilotBaselineInput
		wantKind  string
		wantInMsg string
	}{
		{
			name: "COPILOT_HOME set to the home directory",
			in: CopilotBaselineInput{GOOS: "linux", Home: home,
				Getenv: envMap(map[string]string{CopilotHomeEnvVar: home})},
			wantKind:  "copilot-sandbox-baseline-too-broad",
			wantInMsg: "covers the home directory",
		},
		{
			name: "COPILOT_CACHE_HOME set to an ancestor of home",
			in: CopilotBaselineInput{GOOS: "linux", Home: home,
				Getenv: envMap(map[string]string{CopilotCacheHomeEnvVar: root})},
			wantKind:  "copilot-sandbox-baseline-too-broad",
			wantInMsg: "covers the home directory",
		},
		{
			name: "COPILOT_CACHE_HOME set to the shared XDG cache base",
			in: CopilotBaselineInput{GOOS: "linux", Home: home,
				Getenv: envMap(map[string]string{
					CopilotCacheHomeEnvVar: filepath.Join(home, ".cache"),
				})},
			wantKind:  "copilot-sandbox-baseline-too-broad",
			wantInMsg: "XDG cache base",
		},
		{
			name: "macOS caches base instead of the copilot subdirectory",
			in: CopilotBaselineInput{GOOS: "darwin", Home: home,
				Getenv: envMap(map[string]string{
					CopilotCacheHomeEnvVar: filepath.Join(home, "Library", "Caches"),
				})},
			wantKind:  "copilot-sandbox-baseline-too-broad",
			wantInMsg: "macOS caches base",
		},
		{
			name: "a grant covering the workspace",
			in: CopilotBaselineInput{GOOS: "linux", Home: home,
				Getenv: envMap(map[string]string{
					CopilotHomeEnvVar: filepath.Join(root, "repo"),
				}),
				Workspace: filepath.Join(root, "repo", "worktree")},
			wantKind:  "copilot-sandbox-baseline-too-broad",
			wantInMsg: "covers the workspace",
		},
		{
			name: "a grant equal to the repository root",
			in: CopilotBaselineInput{GOOS: "linux", Home: home,
				Getenv: envMap(map[string]string{
					CopilotCacheHomeEnvVar: filepath.Join(root, "repo"),
				}),
				Workspace: filepath.Join(root, "repo")},
			wantKind:  "copilot-sandbox-baseline-too-broad",
			wantInMsg: "covers the workspace",
		},
		{
			name: "COPILOT_CACHE_HOME set to an ancestor of a relocated XDG base",
			in: CopilotBaselineInput{GOOS: "linux", Home: home,
				Getenv: envMap(map[string]string{
					"XDG_CACHE_HOME":       filepath.Join(root, "shared", "cache", "me"),
					CopilotCacheHomeEnvVar: filepath.Join(root, "shared", "cache"),
				})},
			wantKind:  "copilot-sandbox-baseline-too-broad",
			wantInMsg: "XDG cache base",
		},
		{
			name: "COPILOT_HOME set to a top-level system directory",
			in: CopilotBaselineInput{GOOS: "linux", Home: home,
				Getenv: envMap(map[string]string{CopilotHomeEnvVar: "/etc"})},
			wantKind:  "copilot-sandbox-baseline-too-broad",
			wantInMsg: "top-level system directory",
		},
		{
			name: "COPILOT_HOME set to a macOS firmlinked system directory",
			in: CopilotBaselineInput{GOOS: "darwin", Home: home,
				Getenv: envMap(map[string]string{CopilotHomeEnvVar: "/etc"})},
			wantKind:  "copilot-sandbox-baseline-too-broad",
			wantInMsg: "top-level system directory",
		},
		{
			name: "COPILOT_HOME set to the resolved form of a firmlinked directory",
			in: CopilotBaselineInput{GOOS: "darwin", Home: home,
				Getenv: envMap(map[string]string{CopilotHomeEnvVar: "/private/etc"})},
			wantKind:  "copilot-sandbox-baseline-too-broad",
			wantInMsg: "top-level system directory",
		},
		{
			name: "COPILOT_CACHE_HOME set to a top-level system directory",
			in: CopilotBaselineInput{GOOS: "linux", Home: home,
				Getenv: envMap(map[string]string{CopilotCacheHomeEnvVar: "/opt"})},
			wantKind:  "copilot-sandbox-baseline-too-broad",
			wantInMsg: "top-level system directory",
		},
		{
			name: "a relative COPILOT_HOME",
			in: CopilotBaselineInput{GOOS: "linux", Home: home,
				Getenv: envMap(map[string]string{CopilotHomeEnvVar: "relative/state"})},
			wantKind:  "copilot-sandbox-baseline-unresolved-path",
			wantInMsg: "absolute path",
		},
		{
			name: "a relative agentd socket",
			in: CopilotBaselineInput{GOOS: "linux", Home: home, Getenv: envMap(nil),
				AgentdSockets: []string{"relative/agentd.sock"}},
			wantKind:  "copilot-sandbox-baseline-unresolved-path",
			wantInMsg: "absolute path",
		},
		{
			name: "a relative temp directory",
			in: CopilotBaselineInput{GOOS: "linux", Home: home, Getenv: envMap(nil),
				TempDir: "relative/tmp"},
			wantKind:  "copilot-sandbox-baseline-unresolved-path",
			wantInMsg: "absolute path",
		},
		{
			name: "a relative tclaude executable",
			in: CopilotBaselineInput{GOOS: "linux", Home: home, Getenv: envMap(nil),
				TclaudeExecutable: "relative/tclaude"},
			wantKind:  "copilot-sandbox-baseline-unresolved-path",
			wantInMsg: "absolute path",
		},
		{
			name: "COPILOT_HOME under the macOS application support base",
			in: CopilotBaselineInput{GOOS: "darwin", Home: home,
				Getenv: envMap(map[string]string{
					CopilotHomeEnvVar: filepath.Join(home, "Library", "Application Support"),
				})},
			wantKind:  "copilot-sandbox-baseline-too-broad",
			wantInMsg: "macOS application support base",
		},
		{
			name: "an agentd socket inside tclaude's private state",
			in: CopilotBaselineInput{GOOS: "linux", Home: home, Getenv: envMap(nil),
				AgentdSockets: []string{filepath.Join(home, ".tclaude", "data", "agentd.sock")}},
			wantKind:  "copilot-sandbox-baseline-too-broad",
			wantInMsg: "protected state",
		},
		{
			name: "COPILOT_HOME nested inside the Codex state directory",
			in: CopilotBaselineInput{GOOS: "linux", Home: home,
				Getenv: envMap(map[string]string{
					CopilotHomeEnvVar: filepath.Join(home, ".codex", "copilot"),
				})},
			wantKind:  "copilot-sandbox-baseline-too-broad",
			wantInMsg: "protected state",
		},
		{
			name: "a grant covering tclaude's private state directory",
			in: CopilotBaselineInput{GOOS: "linux", Home: home,
				Getenv: envMap(map[string]string{
					CopilotHomeEnvVar: filepath.Join(home, ".tclaude"),
				})},
			wantKind:  "copilot-sandbox-baseline-too-broad",
			wantInMsg: "protected state",
		},
		{
			name: "a grant covering the Codex state directory",
			in: CopilotBaselineInput{GOOS: "linux", Home: home,
				Getenv: envMap(map[string]string{
					CopilotCacheHomeEnvVar: filepath.Join(home, ".codex"),
				})},
			wantKind:  "copilot-sandbox-baseline-too-broad",
			wantInMsg: "protected state",
		},
		{
			name: "a temp directory pointed at a system root",
			in: CopilotBaselineInput{GOOS: "linux", Home: home, Getenv: envMap(nil),
				TempDir: "/etc"},
			wantKind:  "copilot-sandbox-baseline-too-broad",
			wantInMsg: "top-level system directory",
		},
		{
			name: "a temp directory pointed at /usr",
			in: CopilotBaselineInput{GOOS: "darwin", Home: home, Getenv: envMap(nil),
				TempDir: "/usr"},
			wantKind:  "copilot-sandbox-baseline-too-broad",
			wantInMsg: "top-level system directory",
		},
		{
			name:      "an unresolved home directory",
			in:        CopilotBaselineInput{GOOS: "linux", Home: "", Getenv: envMap(nil)},
			wantKind:  "copilot-sandbox-baseline-unresolved-home",
			wantInMsg: "absolute home directory",
		},
		{
			name:      "a relative home directory",
			in:        CopilotBaselineInput{GOOS: "linux", Home: "home", Getenv: envMap(nil)},
			wantKind:  "copilot-sandbox-baseline-unresolved-home",
			wantInMsg: "absolute home directory",
		},
		{
			name:      "an unsupported platform",
			in:        CopilotBaselineInput{GOOS: "windows", Home: home, Getenv: envMap(nil)},
			wantKind:  "copilot-sandbox-baseline-unsupported-platform",
			wantInMsg: "linux and darwin only",
		},
		{
			name: "a supplied executable at the filesystem root",
			in: CopilotBaselineInput{GOOS: "linux", Home: home, Getenv: envMap(nil),
				CopilotExecutable: "/"},
			wantKind:  "copilot-sandbox-baseline-too-broad",
			wantInMsg: "covers the home directory",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entries, err := CopilotSandboxBaseline(tc.in)
			require.Error(t, err, "must refuse rather than return %v", entries)
			assert.Nil(t, entries, "a refused baseline must return no grants at all")
			var capErr *SandboxCapabilityError
			require.ErrorAs(t, err, &capErr)
			assert.Equal(t, CopilotName, capErr.Harness)
			assert.Equal(t, tc.wantKind, capErr.Kind)
			assert.Contains(t, capErr.Message, tc.wantInMsg)
		})
	}
}

// TestCopilotSandboxBaselineAllowsTopLevelTempDir is the counterpart to the
// system-root refusal: /tmp IS a top-level directory, Copilot grants it in its
// own default policy, and refusing it would break every shell tool.
//
// The home directory here is a synthetic path rather than t.TempDir(). That is
// the point of the next test: on a machine whose TMPDIR is /tmp — every CI
// runner — t.TempDir() lives INSIDE the granted temp directory, so using it
// would silently test the home-containment refusal instead of this rule.
func TestCopilotSandboxBaselineAllowsTopLevelTempDir(t *testing.T) {
	entries, err := CopilotSandboxBaseline(CopilotBaselineInput{
		GOOS:    "linux",
		Home:    "/home/copilot-baseline-test-user",
		Getenv:  envMap(nil),
		TempDir: "/tmp",
	})
	require.NoError(t, err)
	assert.Equal(t, "/tmp", entryByID(t, entries, CopilotBaselineTempDir).Path)
}

// TestCopilotSandboxBaselineRefusesTempDirContainingHome records the
// precedence between the two rules above, because they can genuinely collide:
// a container or CI runner with HOME under /tmp makes a temp-dir grant a HOME
// grant, and the HOME rule has to win. The temp row is exempt from the
// system-root rule, not from home containment.
func TestCopilotSandboxBaselineRefusesTempDirContainingHome(t *testing.T) {
	_, err := CopilotSandboxBaseline(CopilotBaselineInput{
		GOOS:    "linux",
		Home:    "/tmp/build/home",
		Getenv:  envMap(nil),
		TempDir: "/tmp",
	})
	require.Error(t, err)
	var capErr *SandboxCapabilityError
	require.ErrorAs(t, err, &capErr)
	assert.Equal(t, "copilot-sandbox-baseline-too-broad", capErr.Kind)
	assert.Contains(t, capErr.Message, "covers the home directory")
}

// TestCopilotSandboxBaselineAcceptsConventionalTempRoots is the other half of
// the narrowed exemption: the legitimate temp forms must still resolve. Only
// /tmp needs the exemption at all — every other conventional temp location is
// already deeper than one level and never reaches the system-root rule.
func TestCopilotSandboxBaselineAcceptsConventionalTempRoots(t *testing.T) {
	cases := []struct{ name, goos, tempDir string }{
		{"linux /tmp", "linux", "/tmp"},
		{"darwin /tmp", "darwin", "/tmp"},
		{"darwin firmlinked temp", "darwin", "/private/tmp"},
		{"linux per-user runtime temp", "linux", "/run/user/1000"},
		{"linux /var/tmp", "linux", "/var/tmp"},
		{"darwin per-session temp", "darwin", "/var/folders/df/abc123/T"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entries, err := CopilotSandboxBaseline(CopilotBaselineInput{
				GOOS:    tc.goos,
				Home:    "/home/copilot-baseline-test-user",
				Getenv:  envMap(nil),
				TempDir: tc.tempDir,
			})
			require.NoError(t, err)
			assert.Equal(t, tc.tempDir, entryByID(t, entries, CopilotBaselineTempDir).Path)
		})
	}
}

// TestCopilotSandboxBaselineDedupesAgentdEndpointsByResolvedPath pins the
// three properties the endpoint dedup has to hold at once.
//
// The key is the RESOLVED path because the duplicates that actually occur are
// not textual: a legacy endpoint reached through a symlinked home cleans
// differently and names the same socket. Deduping on the raw spelling would
// emit two mount rules for one node; deduping too eagerly would drop a real
// endpoint, and agent coordination would fail for whichever one lost.
func TestCopilotSandboxBaselineDedupesAgentdEndpointsByResolvedPath(t *testing.T) {
	root := t.TempDir()
	realDir := filepath.Join(root, "real")
	require.NoError(t, os.MkdirAll(realDir, 0o755))
	link := filepath.Join(root, "link")
	require.NoError(t, os.Symlink(realDir, link))

	viaReal := filepath.Join(realDir, "agentd.sock")
	viaLink := filepath.Join(link, "agentd.sock")
	distinct := filepath.Join(realDir, "legacy-agentd.sock")

	entries, err := CopilotSandboxBaseline(CopilotBaselineInput{
		GOOS:          "linux",
		Home:          filepath.Join(root, "home"),
		Getenv:        envMap(nil),
		AgentdSockets: []string{viaReal, viaLink, distinct, viaReal},
	})
	require.NoError(t, err)

	var sockets []string
	for _, e := range entries {
		if e.ID == CopilotBaselineAgentdSocket {
			sockets = append(sockets, e.Path)
		}
	}
	// First occurrence wins and keeps the CALLER's spelling, order preserved;
	// the symlinked alias collapses into it; the genuinely different endpoint
	// survives.
	assert.Equal(t, []string{viaReal, distinct}, sockets)
}

// TestCopilotSandboxBaselineRefusesBadEndpointAmongGoodOnes proves the dedup
// cannot become a way to smuggle a bad endpoint past validation: every
// surviving row is validated, and one refusal fails the whole catalog.
func TestCopilotSandboxBaselineRefusesBadEndpointAmongGoodOnes(t *testing.T) {
	home := "/home/copilot-baseline-test-user"
	_, err := CopilotSandboxBaseline(CopilotBaselineInput{
		GOOS:   "linux",
		Home:   home,
		Getenv: envMap(nil),
		AgentdSockets: []string{
			filepath.Join(home, ".tclaude", "api", "agentd.sock"),
			filepath.Join(home, ".tclaude", "data", "agentd.sock"),
			filepath.Join(home, ".tclaude-agentd.sock"),
		},
	})
	require.Error(t, err, "a protected-state endpoint must refuse even alongside valid ones")
	var capErr *SandboxCapabilityError
	require.ErrorAs(t, err, &capErr)
	assert.Equal(t, "copilot-sandbox-baseline-too-broad", capErr.Kind)
	assert.Contains(t, capErr.Message, "protected state")
}

// TestCopilotSandboxBaselineProtectsTclaudeStateExactly pins that the
// protection is the private subtree, NOT the whole ~/.tclaude root: the
// canonical agentd socket lives under ~/.tclaude/api precisely so it stays
// grantable while ~/.tclaude/data stays denied. Refusing the api/ socket would
// break agent coordination inside the sandbox.
func TestCopilotSandboxBaselineProtectsTclaudeStateExactly(t *testing.T) {
	home := "/home/copilot-baseline-test-user"
	entries, err := CopilotSandboxBaseline(CopilotBaselineInput{
		GOOS:   "linux",
		Home:   home,
		Getenv: envMap(nil),
		AgentdSockets: []string{
			filepath.Join(home, ".tclaude", "api", "agentd.sock"),
			filepath.Join(home, ".tclaude-agentd.sock"),
			filepath.Join(home, ".tclaude", "agentd.sock"),
		},
	})
	require.NoError(t, err, "the canonical and legacy endpoints all sit outside ~/.tclaude/data")
	count := 0
	for _, e := range entries {
		if e.ID == CopilotBaselineAgentdSocket {
			count++
		}
	}
	assert.Equal(t, 3, count)
}

// TestCopilotSandboxBaselineNodeKinds pins the node type on every row: a
// consumer cannot stat a cold cache directory that does not exist yet, and
// binding a socket as a directory fails at launch.
func TestCopilotSandboxBaselineNodeKinds(t *testing.T) {
	root := t.TempDir()
	entries, err := CopilotSandboxBaseline(CopilotBaselineInput{
		GOOS:              "linux",
		Home:              filepath.Join(root, "home"),
		Getenv:            envMap(nil),
		TempDir:           filepath.Join(root, "tmp"),
		AgentdSockets:     []string{filepath.Join(root, "api", "agentd.sock")},
		TclaudeExecutable: filepath.Join(root, "bin", "tclaude"),
		CopilotExecutable: filepath.Join(root, "bin", "copilot"),
	})
	require.NoError(t, err)
	want := map[string]CopilotNodeKind{
		CopilotBaselineStateDir:      CopilotNodeDirectory,
		CopilotBaselinePackageCache:  CopilotNodeDirectory,
		CopilotBaselineDeviceIDCache: CopilotNodeDirectory,
		CopilotBaselineTempDir:       CopilotNodeDirectory,
		CopilotBaselineExecutable:    CopilotNodeFile,
		CopilotBaselineTclaudeBinary: CopilotNodeFile,
		CopilotBaselineAgentdSocket:  CopilotNodeSocket,
	}
	for _, e := range entries {
		assert.Equal(t, want[e.ID], e.Kind, "entry %q", e.ID)
	}
	assert.Len(t, entries, len(want), "every row kind must be pinned above")
}

// TestCopilotSandboxBaselineNeverGrantsSharedBases is the regression guard for
// the catalog as a whole: whatever rows exist, none of them may land on HOME
// or on a shared base directory.
func TestCopilotSandboxBaselineNeverGrantsSharedBases(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			root := t.TempDir()
			home := filepath.Join(root, "home")
			entries, err := CopilotSandboxBaseline(CopilotBaselineInput{
				GOOS:              goos,
				Home:              home,
				Getenv:            envMap(nil),
				TempDir:           filepath.Join(root, "tmp"),
				AgentdSockets:     []string{filepath.Join(root, "api", "agentd.sock")},
				TclaudeExecutable: filepath.Join(root, "bin", "tclaude"),
				CopilotExecutable: filepath.Join(root, "bin", "copilot"),
				Workspace:         filepath.Join(root, "repo"),
			})
			require.NoError(t, err)
			require.NotEmpty(t, entries)

			forbidden := []string{
				"/", root, home,
				filepath.Join(home, ".cache"),
				filepath.Join(home, ".config"),
				filepath.Join(home, ".local"),
				filepath.Join(home, "Library"),
				filepath.Join(home, "Library", "Caches"),
				filepath.Join(root, "repo"),
			}
			for _, e := range entries {
				assert.True(t, filepath.IsAbs(e.Path), "entry %q path %q is not absolute", e.ID, e.Path)
				assert.NotEmpty(t, e.Source, "entry %q has no resolution source", e.ID)
				assert.NotEmpty(t, e.Purpose, "entry %q has no purpose", e.ID)
				assert.NotEmpty(t, e.Evidence, "entry %q has no evidence", e.ID)
				assert.True(t, e.Access.Read, "entry %q grants no read access", e.ID)
				for _, bad := range forbidden {
					assert.NotEqual(t, bad, e.Path, "entry %q grants the shared path %q", e.ID, bad)
				}
			}
		})
	}
}

// TestCopilotAccessString pins the rendered mode vocabulary consumers log.
func TestCopilotAccessString(t *testing.T) {
	assert.Equal(t, "r", CopilotAccess{Read: true}.String())
	assert.Equal(t, "rw", copilotReadWrite().String())
	assert.Equal(t, "rx", copilotReadExec().String())
	assert.Equal(t, "rwx", copilotReadWriteExec().String())
	assert.Equal(t, "none", CopilotAccess{}.String())
}

// TestCopilotHookInstallDirMatchesBaseline is the anti-drift check between the
// two places that resolve Copilot's state directory: hooks are installed under
// <COPILOT_HOME>/hooks, and the baseline must pre-approve that same directory
// or live status would break inside the sandbox.
func TestCopilotHookInstallDirMatchesBaseline(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")

	// A DIRTY value on purpose: a trailing slash and a "." segment are what an
	// operator's shell export looks like, and they are exactly what would make
	// the installed path and the pre-approved path two different strings.
	dirty := filepath.Join(root, "state") + "/./"
	t.Setenv(CopilotHomeEnvVar, dirty)

	entries, err := CopilotSandboxBaseline(CopilotBaselineInput{
		GOOS: "linux", Home: home, Getenv: envMap(map[string]string{
			CopilotHomeEnvVar: dirty,
		}),
	})
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(root, "state"),
		entryByID(t, entries, CopilotBaselineStateDir).Path)
	assert.Equal(t, copilotHome(), entryByID(t, entries, CopilotBaselineStateDir).Path)
}

// TestCopilotHomeRefusesRelativeOverride pins the fail-closed half of the same
// resolver: a relative COPILOT_HOME yields "cannot determine" rather than a
// cwd-relative hooks file.
func TestCopilotHomeRefusesRelativeOverride(t *testing.T) {
	t.Setenv(CopilotHomeEnvVar, "relative/state")
	assert.Empty(t, copilotHome())
}

// TestCopilotSandboxBaselineRefusesCaseVariantBroadGrants is the Copilot half
// of TCL-981's shared containment fix.
//
// The baseline's refusals are containment tests against $HOME, the shared cache
// bases, tclaude's protected state and the workspace. On a case-insensitive
// volume a differently cased spelling of any of those names the SAME directory,
// so a byte-exact comparison would wave through exactly the too-broad grant the
// gate exists to refuse — COPILOT_HOME=$HOME/.TCLAUDE would hand a confined
// Copilot launch the daemon's own database.
//
// Every case below is refused on EVERY volume, and that uniformity is the
// deliberate shape of the fix rather than blind lowercasing. Each variant
// spelling case/NFC-folds onto a protected path AND does not exist, so no
// filesystem identity can refute the collision. Rather than guess what this
// volume would do with a name that is not there — a question whose answer is
// per-directory, not per-volume, and which produced repeated fail-opens when
// this change tried to answer it empirically — the guard refuses.
//
// The "do not lowercase blindly" half of the contract is enforced elsewhere and
// stays intact: a case variant that EXISTS as a distinct directory is refuted by
// os.SameFile and accepted (see TestCopilotSandboxBaselineAcceptsDistinctCaseVariants
// below), and a path that folds onto nothing protected never reaches the guard's
// I/O at all.
func TestCopilotSandboxBaselineRefusesCaseVariantBroadGrants(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	home := filepath.Join(root, "Home")
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".tclaude", "data"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".cache"), 0o755))

	cases := []struct {
		name      string
		in        CopilotBaselineInput
		wantInMsg string
	}{
		{
			name: "COPILOT_HOME spelled as a case variant of the home directory",
			in: CopilotBaselineInput{GOOS: runtime.GOOS, Home: home,
				Getenv: envMap(map[string]string{CopilotHomeEnvVar: filepath.Join(root, "home")})},
			wantInMsg: "covers the home directory",
		},
		{
			name: "COPILOT_HOME spelled as a case variant of tclaude's protected state",
			in: CopilotBaselineInput{GOOS: runtime.GOOS, Home: home,
				Getenv: envMap(map[string]string{
					CopilotHomeEnvVar: filepath.Join(home, ".TCLAUDE", "Data"),
				})},
			wantInMsg: "protected state",
		},
		{
			name: "an agentd socket spelled as a case variant inside protected state",
			in: CopilotBaselineInput{GOOS: runtime.GOOS, Home: home, Getenv: envMap(nil),
				AgentdSockets: []string{filepath.Join(home, ".Tclaude", "Data", "agentd.sock")}},
			wantInMsg: "protected state",
		},
		{
			name: "COPILOT_CACHE_HOME spelled as a case variant of the shared cache base",
			in: CopilotBaselineInput{GOOS: "linux", Home: home,
				Getenv: envMap(map[string]string{
					CopilotCacheHomeEnvVar: filepath.Join(home, ".CACHE"),
				})},
			wantInMsg: "XDG cache base",
		},
		{
			name: "a grant spelled as a case variant covering the workspace",
			in: CopilotBaselineInput{GOOS: runtime.GOOS, Home: home,
				Getenv: envMap(map[string]string{
					CopilotHomeEnvVar: filepath.Join(root, "REPO"),
				}),
				Workspace: filepath.Join(root, "repo", "worktree")},
			wantInMsg: "covers the workspace",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entries, err := CopilotSandboxBaseline(tc.in)
			require.Error(t, err, "must refuse rather than return %v", entries)
			assert.Nil(t, entries, "a refused baseline must return no grants at all")
			var capErr *SandboxCapabilityError
			require.ErrorAs(t, err, &capErr)
			assert.Equal(t, CopilotName, capErr.Harness)
			assert.Equal(t, "copilot-sandbox-baseline-too-broad", capErr.Kind)
			assert.Contains(t, capErr.Message, tc.wantInMsg)
		})
	}
}

// TestCopilotSandboxBaselineAcceptsDistinctCaseVariants is the other half of the
// contract above, and the reason the uniform refusal is not blind lowercasing.
//
// Here the case-variant directory EXISTS, so on a case-sensitive volume
// os.SameFile refutes the folded nomination and the grant is accepted as the
// ordinary distinct directory it is. On a case-insensitive volume the same
// two spellings reach one inode, SameFile confirms it, and the grant is refused
// — the correct answer on each. This is the one place volume adaptation still
// belongs, because here the filesystem can actually be asked.
func TestCopilotSandboxBaselineAcceptsDistinctCaseVariants(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	home := filepath.Join(root, "Home")
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".tclaude", "data"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".cache"), 0o755))

	// Stage the variant spelling too, so the filesystem can settle the question
	// by identity instead of the guard having to refuse an unresolvable name.
	variant := filepath.Join(root, "home")
	require.NoError(t, os.MkdirAll(variant, 0o755))

	homeInfo, err := os.Lstat(home)
	require.NoError(t, err)
	variantInfo, err := os.Lstat(variant)
	require.NoError(t, err)
	folds := os.SameFile(homeInfo, variantInfo)
	t.Logf("volume folds case: %t (home=%q)", folds, home)

	in := CopilotBaselineInput{GOOS: runtime.GOOS, Home: home,
		Getenv: envMap(map[string]string{CopilotHomeEnvVar: variant})}
	entries, err := CopilotSandboxBaseline(in)
	if folds {
		require.Error(t, err,
			"one inode means one directory, so this grant covers $HOME and must be refused")
		return
	}
	require.NoError(t, err,
		"two distinct inodes must stay two distinct directories — refusing here would "+
			"mean the fix had started folding case blindly")
	assert.NotEmpty(t, entries)
}

// TestCopilotSandboxBaselineRefusesCaseVariantFirmlinkSystemRoot closes the last
// spelling gap in the baseline gate.
//
// The system-root rule tests BOTH the operator's literal spelling and its
// resolved form, and it depends on copilotNormalizeFirmlink collapsing macOS's
// "/private/<x>" onto "/<x>". That prefix match used to be byte-exact, so on a
// case-insensitive boot volume "/Private/etc" — which names /etc — stayed
// un-collapsed, presented a Dir() of "/Private" instead of "/", and was
// therefore not classified as a top-level system directory. COPILOT_HOME or
// TMPDIR pointed there would have become an rw grant on /etc.
//
// This is asserted with GOOS "darwin" regardless of the host, because the rule
// under test is a pure function of the platform argument and the spelling.
func TestCopilotSandboxBaselineRefusesCaseVariantFirmlinkSystemRoot(t *testing.T) {
	for _, spelling := range []string{
		"/Private/etc",
		"/PRIVATE/etc",
		"/private/etc",
	} {
		t.Run(spelling, func(t *testing.T) {
			assert.Equal(t, "/etc", copilotNormalizeFirmlink("darwin", spelling),
				"every case spelling of the firmlink prefix must collapse alike")
			assert.True(t, copilotSystemRootDir("darwin", spelling),
				"%q names /etc and must be classified as a top-level system directory", spelling)
		})
	}

	// Linux has no firmlink, and its filesystems are case-sensitive: the prefix
	// must stay literal there so a real "/Private/etc" directory is not silently
	// treated as "/etc".
	assert.Equal(t, "/Private/etc", copilotNormalizeFirmlink("linux", "/Private/etc"))

	// A path that merely starts with the same letters is not a firmlink.
	assert.Equal(t, "/privateer/etc", copilotNormalizeFirmlink("darwin", "/privateer/etc"))
	// The prefix alone has no remainder to promote.
	assert.Equal(t, "/private", copilotNormalizeFirmlink("darwin", "/private"))
	assert.Equal(t, "/Private", copilotNormalizeFirmlink("darwin", "/Private"))

	// End to end through the gate: the variant spelling must be refused.
	home := filepath.Join(t.TempDir(), "home")
	_, err := CopilotSandboxBaseline(CopilotBaselineInput{
		GOOS: "darwin", Home: home,
		Getenv: envMap(map[string]string{CopilotHomeEnvVar: "/Private/etc"}),
	})
	require.Error(t, err)
	var capErr *SandboxCapabilityError
	require.ErrorAs(t, err, &capErr)
	assert.Contains(t, capErr.Message, "top-level system directory")
}
