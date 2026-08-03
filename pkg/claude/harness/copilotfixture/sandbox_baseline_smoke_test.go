package copilotfixture_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/harness/copilotfixture"
)

// TestCopilotSandboxBaselineCoversObservedWrites is the fixture-backed proof
// behind harness.CopilotSandboxBaseline (TCL-975): the catalog is not a
// reading of the documentation, it is a claim about what the real binary
// touches, and this test is what makes the claim falsifiable.
//
// It runs a complete credential-free turn, walks everything the CLI created,
// and requires each created path to fall inside a baseline entry resolved from
// the SAME environment the run used. A future CLI that starts writing
// somewhere new fails here rather than at an operator's confined launch.
//
// Two properties are asserted rather than merely observed:
//
//   - Nothing is created in HOME outside the catalog. That is the whole reason
//     the baseline can refuse a HOME grant.
//   - Nothing is created in the launch working directory. The workspace grant
//     is the caller's, and the catalog deliberately does not carry it, so a
//     turn that quietly wrote there would mean the split is wrong.
func TestCopilotSandboxBaselineCoversObservedWrites(t *testing.T) {
	requireSmoke(t)

	// A caller-chosen id so the session-state directory in the golden is
	// recognisably the enrolled one rather than an anonymous uuid.
	const sessionID = "22222222-3333-4444-8555-666666666666"

	mock := copilotfixture.NewMockProvider(t, []copilotfixture.Turn{
		{Text: "MOCK BASELINE ANSWER"},
	})
	dirs := copilotfixture.NewSandboxDirs(t)
	// XDG_CACHE_HOME gets its OWN directory here, unlike the wire scenarios:
	// this test's whole subject is which variable owns which write, and
	// pointing both at one directory would hide the split the catalog encodes.
	result := copilotfixture.Run(t, copilotfixture.RunOptions{
		Root: dirs.Root, Home: dirs.Home, Cache: dirs.Cache, XDGCache: dirs.XDGCache,
		WorkDir: dirs.WorkDir,
		BaseURL: mock.BaseURL(), Prompt: "Baseline question.", SessionID: sessionID,
	})
	require.Equal(t, 0, result.ExitCode, "stderr: %s", result.Stderr)

	// The baseline is resolved from the run's own environment: the runner sets
	// COPILOT_HOME, COPILOT_CACHE_HOME and XDG_CACHE_HOME, and HOME is the
	// disposable root. Anything the catalog then names is a path the CLI was
	// actually launched against.
	runEnv := map[string]string{
		harness.CopilotHomeEnvVar:      dirs.Home,
		harness.CopilotCacheHomeEnvVar: dirs.Cache,
		"XDG_CACHE_HOME":               dirs.XDGCache,
	}
	entries, err := harness.CopilotSandboxBaseline(harness.CopilotBaselineInput{
		Home:      dirs.Root,
		Getenv:    func(k string) string { return runEnv[k] },
		Workspace: dirs.WorkDir,
	})
	require.NoError(t, err)

	writable := map[string]string{}
	for _, e := range entries {
		if e.Access.Write {
			writable[e.Path] = e.ID
		}
	}
	require.Contains(t, writable, dirs.Home, "COPILOT_HOME must be a writable baseline entry")
	require.Contains(t, writable, dirs.Cache, "the package cache must be a writable baseline entry")
	// On Linux the device-id row resolves through XDG_CACHE_HOME (never
	// COPILOT_CACHE_HOME); on macOS it ignores both and follows the Microsoft
	// device-id convention into Library/Application Support.
	wantDeviceID := filepath.Join(dirs.XDGCache, "Microsoft", "DeveloperTools")
	if runtime.GOOS == "darwin" {
		wantDeviceID = filepath.Join(
			dirs.Root, "Library", "Application Support", "Microsoft", "DeveloperTools")
	}
	require.Contains(t, writable, wantDeviceID,
		"the device-id cache must be a writable baseline entry at this platform's location")

	layout, err := copilotfixture.ObserveBaselineLayout(dirs)
	require.NoError(t, err)

	assert.Empty(t, layout.HomeOutsideBaseline,
		"Copilot created state in HOME that no baseline entry covers; a confined launch "+
			"would fail on these paths, so the catalog needs a new row (or the CLI regressed)")
	assert.Empty(t, layout.WorkDir.Entries,
		"a plain turn wrote into the working directory; the workspace grant is the caller's, "+
			"so this would change what the baseline may leave out")

	// The mandatory rows are not merely present, they are USED: an entry the
	// CLI never writes would be an over-grant worth deleting.
	assert.NotEmpty(t, layout.CopilotHome.Entries)
	assert.Contains(t, layout.CopilotHome.Entries, "session-store.db")
	assert.Contains(t, layout.CopilotHome.Entries,
		filepath.Join("session-state", "<uuid>"),
		"the enrolled session's state directory lives under COPILOT_HOME")
	assert.NotEmpty(t, layout.Cache.Entries)
	assert.Contains(t, layout.DeviceIDCache.Entries, "deviceid",
		"the best-effort device-id row is written on Linux and macOS alike; only its "+
			"directory moves between them")

	// The XDG cache base is NOT granted whole — only its
	// Microsoft/DeveloperTools subtree is — so a write elsewhere under it is an
	// uncovered path in exactly the way a HOME write would be. The walk skips
	// this root when computing HomeOutsideBaseline (it is a baseline root), so
	// without this check nothing would notice. On macOS the expected content is
	// none at all, which this same loop states.
	for _, rel := range layout.XDGCache.Entries {
		assert.True(t,
			rel == "Microsoft" || strings.HasPrefix(rel, "Microsoft/DeveloperTools"),
			"Copilot wrote %q under XDG_CACHE_HOME, which the catalog covers only at "+
				"Microsoft/DeveloperTools; the baseline needs a new row (or the CLI regressed)", rel)
	}

	// The golden is per-platform because the tree genuinely differs: the package
	// cache and the device-id file each move on macOS, and to different places.
	// One merged golden could only be written by dropping whichever rows differ,
	// which is precisely the evidence this recording exists to hold.
	compareLayoutGolden(t, "sandbox_baseline_"+runtime.GOOS, dirs, layout)
}

// TestCopilotSandboxBaselineRefusesRunEnvironmentPointedAtHome pins the
// fail-closed behavior against the real launch environment shape: an operator
// who points COPILOT_HOME at their home directory gets a refusal, not a
// baseline that grants HOME.
func TestCopilotSandboxBaselineRefusesRunEnvironmentPointedAtHome(t *testing.T) {
	dirs := copilotfixture.NewSandboxDirs(t)
	_, err := harness.CopilotSandboxBaseline(harness.CopilotBaselineInput{
		Home: dirs.Root,
		Getenv: func(k string) string {
			if k == harness.CopilotHomeEnvVar {
				return dirs.Root
			}
			return ""
		},
	})
	require.Error(t, err)
	var capErr *harness.SandboxCapabilityError
	require.ErrorAs(t, err, &capErr)
	assert.Equal(t, "copilot-sandbox-baseline-too-broad", capErr.Kind)
}

// compareLayoutGolden commits the observed layout the same way the wire
// fixtures are committed: normalized, secret-checked, and re-recorded only
// through an explicit -update run whose diff is the evidence.
func compareLayoutGolden(
	t *testing.T,
	name string,
	dirs copilotfixture.Dirs,
	layout copilotfixture.BaselineLayout,
) {
	t.Helper()
	encoded, err := copilotfixture.Marshal(struct {
		CLIVersion string                        `json:"cliVersion"`
		Layout     copilotfixture.BaselineLayout `json:"layout"`
	}{copilotfixture.PinnedCLIVersion, layout})
	require.NoError(t, err)
	assertNoLeakedSecrets(t, encoded, dirs)

	path := filepath.Join("testdata", copilotfixture.PinnedCLIVersion, name+".json")
	if *update {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, encoded, 0o644))
		t.Logf("re-recorded %s", path)
		return
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err,
		"missing golden %s; re-record with `go test -run %s -update`", path, t.Name())
	assert.JSONEq(t, string(want), string(encoded),
		"Copilot on-disk layout drift in %s. Review the diff against the sandbox baseline "+
			"catalog, then re-record with -update if the change is intended.", path)
}
