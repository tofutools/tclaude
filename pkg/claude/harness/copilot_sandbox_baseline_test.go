package harness

import (
	"path/filepath"
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
// easiest to get wrong: on macOS the package cache moves to
// ~/Library/Caches/copilot while the device-id cache stays XDG-shaped under
// ~/.cache, because the runtime that writes it has no darwin branch.
func TestCopilotSandboxBaselineDarwinSplit(t *testing.T) {
	home := t.TempDir()
	entries, err := CopilotSandboxBaseline(CopilotBaselineInput{
		GOOS:   "darwin",
		Home:   home,
		Getenv: envMap(nil),
	})
	require.NoError(t, err)

	assert.Equal(t, filepath.Join(home, "Library", "Caches", "copilot"),
		entryByID(t, entries, CopilotBaselinePackageCache).Path)
	assert.Equal(t, filepath.Join(home, ".cache", "Microsoft", "DeveloperTools"),
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
		AgentdSockets:     []string{socket, "", "  "},
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

	// Blank socket entries are dropped, not turned into empty grants.
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
func TestCopilotSandboxBaselineAllowsTopLevelTempDir(t *testing.T) {
	entries, err := CopilotSandboxBaseline(CopilotBaselineInput{
		GOOS:    "linux",
		Home:    t.TempDir(),
		Getenv:  envMap(nil),
		TempDir: "/tmp",
	})
	require.NoError(t, err)
	assert.Equal(t, "/tmp", entryByID(t, entries, CopilotBaselineTempDir).Path)
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
