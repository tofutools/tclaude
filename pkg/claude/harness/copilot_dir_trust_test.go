package harness

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// planCopilotDirTrust is pure (bytes in → bytes out), so the merge, precedence
// and refusal cases are all exhaustively unit-testable; the file-level helper
// is driven against a temp COPILOT_HOME to pin the atomic-write and
// resolution behaviour. Mirrors claude_dir_trust_test.go's split.

// trustedFolders decodes the seeded key out of a planned config.json.
func trustedFolders(t *testing.T, data []byte) []string {
	t.Helper()
	var root struct {
		TrustedFolders []string `json:"trustedFolders"`
	}
	require.NoError(t, json.Unmarshal(data, &root))
	return root.TrustedFolders
}

func TestPlanCopilotDirTrust_CreatesFromAbsentFiles(t *testing.T) {
	changed, out, err := planCopilotDirTrust(nil, []string{"/work/proj"})
	require.NoError(t, err)
	require.True(t, changed)
	assert.Equal(t, []string{"/work/proj"}, trustedFolders(t, out))
}

func TestPlanCopilotDirTrust_AlreadyTrustedIsANoOp(t *testing.T) {
	config := []byte(`{"trustedFolders":["/work/proj"]}`)
	changed, out, err := planCopilotDirTrust(config, []string{"/work/proj"})
	require.NoError(t, err)
	assert.False(t, changed, "an already-trusted dir must not rewrite the file")
	assert.Nil(t, out)
}

// A trailing separator is the same directory. Seeding it again would grow the
// operator's list on every spawn.
func TestPlanCopilotDirTrust_MatchesAnUncleanExistingSpelling(t *testing.T) {
	config := []byte(`{"trustedFolders":["/work/proj/"]}`)
	changed, _, err := planCopilotDirTrust(config, []string{"/work/proj"})
	require.NoError(t, err)
	assert.False(t, changed)
}

func TestPlanCopilotDirTrust_PreservesUnrelatedConfigKeys(t *testing.T) {
	config := []byte(`{
  // This file is managed automatically.
  "theme": "dark",
  "banner": "a <b> & c",
  "counter": 90071992547409911,
  "sandbox": {"enabled": false}
}`)
	changed, out, err := planCopilotDirTrust(config, []string{"/work/proj"})
	require.NoError(t, err)
	require.True(t, changed)

	var root map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(out, &root))
	assert.JSONEq(t, `{"enabled":false}`, string(root["sandbox"]))
	assert.Equal(t, `"dark"`, string(root["theme"]))
	// Raw bytes are carried across, so a large integer keeps every digit and
	// the HTML-ish characters are not re-escaped.
	assert.Equal(t, `90071992547409911`, string(root["counter"]))
	assert.Equal(t, `"a <b> & c"`, string(root["banner"]))
	assert.Equal(t, []string{"/work/proj"}, trustedFolders(t, out))
}

// A JSON null is what a cleared list looks like; it is absence, not ambiguity.
func TestPlanCopilotDirTrust_NullListIsTreatedAsEmpty(t *testing.T) {
	changed, out, err := planCopilotDirTrust([]byte(`{"trustedFolders":null}`),
		[]string{"/work/proj"})
	require.NoError(t, err)
	require.True(t, changed)
	assert.Equal(t, []string{"/work/proj"}, trustedFolders(t, out))
}

// The CLI's own managed keys share this file, so an edit must carry them across
// untouched — a seed that dropped firstLaunchAt would be tclaude rewriting CLI
// state it does not own.
func TestPlanCopilotDirTrust_ExtendsAManagedStub(t *testing.T) {
	stub := []byte(`// User settings belong in settings.json.
// This file is managed automatically.
{
  "trustedFolders": [
    "/home/me/repo-a"
  ],
  "firstLaunchAt": "2026-03-11T00:00:00.000Z"
}`)
	changed, out, err := planCopilotDirTrust(stub, []string{"/work/proj"})
	require.NoError(t, err)
	require.True(t, changed)
	assert.Equal(t, []string{"/home/me/repo-a", "/work/proj"}, trustedFolders(t, out))

	var root map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(out, &root))
	assert.Equal(t, `"2026-03-11T00:00:00.000Z"`, string(root["firstLaunchAt"]))
}

// Fail-safe: a shape tclaude cannot edit is refused rather than overwritten —
// silently replacing it would discard folders the operator trusted.
func TestPlanCopilotDirTrust_RefusesANonArrayTrustedFolders(t *testing.T) {
	for _, data := range []string{
		`{"trustedFolders":"/work/proj"}`,
		`{"trustedFolders":{"a":true}}`,
		`{"trustedFolders":[1,2]}`,
	} {
		_, _, err := planCopilotDirTrust([]byte(data), []string{"/work/proj"})
		require.Error(t, err, "input %s must be refused", data)
		assert.Contains(t, err.Error(), CopilotConfigFileName)
	}
}

func TestPlanCopilotDirTrust_RefusesAFileItCannotParse(t *testing.T) {
	for _, data := range []string{`["not","an","object"]`, `nonsense`, `{"trustedFolders":[`} {
		_, _, err := planCopilotDirTrust([]byte(data), []string{"/work/proj"})
		require.Error(t, err, "input %q must be refused, not treated as empty", data)
	}
}

func TestEnsureCopilotDirTrustedInHome_WritesConfigJSONPrivately(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, ensureCopilotDirTrustedInHome(home, "/work/proj"))

	path := filepath.Join(home, CopilotConfigFileName)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, []string{"/work/proj"}, trustedFolders(t, data))

	fi, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm(),
		"a fresh trust store is created private — COPILOT_HOME is credential-adjacent")

	// settings.json is NOT written: the flat key there was measured not to
	// clear the modal, and guessing at the nested schema is exactly what this
	// contract refuses to do.
	_, statErr := os.Stat(filepath.Join(home, CopilotSettingsFileName))
	assert.True(t, os.IsNotExist(statErr))
}

func TestEnsureCopilotDirTrustedInHome_PreservesAnExistingMode(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, CopilotConfigFileName)
	require.NoError(t, os.WriteFile(path, []byte(`{"theme":"dark"}`), 0o644))

	require.NoError(t, ensureCopilotDirTrustedInHome(home, "/work/proj"))
	fi, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o644), fi.Mode().Perm())
}

// Idempotence at the file level: the second call must not touch the file at
// all, so a spawn cannot race the CLI's own writes more than once per dir.
func TestEnsureCopilotDirTrustedInHome_SecondCallDoesNotRewrite(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, ensureCopilotDirTrustedInHome(home, "/work/proj"))
	path := filepath.Join(home, CopilotConfigFileName)
	before, err := os.Stat(path)
	require.NoError(t, err)
	firstBytes, err := os.ReadFile(path)
	require.NoError(t, err)

	require.NoError(t, ensureCopilotDirTrustedInHome(home, "/work/proj"))
	after, err := os.Stat(path)
	require.NoError(t, err)
	secondBytes, err := os.ReadFile(path)
	require.NoError(t, err)

	assert.Equal(t, firstBytes, secondBytes)
	assert.Equal(t, before.ModTime(), after.ModTime(), "an idempotent call must not rewrite the file")
}

// The lost-update regression, and the reason this editor goes through
// EditCopilotConfigFile rather than a bare atomic write. `trustedFolders` is
// ONE shared array: concurrent spawns seeding different dirs all
// read-modify-write it, and a last-writer-wins rename silently drops every
// entry but the last — leaving those panes parked on the modal they were seeded
// to clear. Deterministic despite the concurrency: whatever the interleaving,
// the file must end up carrying all of them.
func TestEnsureCopilotDirTrustedInHome_ConcurrentSeedsAllSurvive(t *testing.T) {
	home := t.TempDir()
	const seeds = 8

	var wg sync.WaitGroup
	errs := make([]error, seeds)
	for i := range seeds {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = ensureCopilotDirTrustedInHome(home, fmt.Sprintf("/work/proj-%d", i))
		}()
	}
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, "seed %d", i)
	}

	data, err := os.ReadFile(filepath.Join(home, CopilotConfigFileName))
	require.NoError(t, err)
	got := trustedFolders(t, data)
	want := make([]string, 0, seeds)
	for i := range seeds {
		want = append(want, fmt.Sprintf("/work/proj-%d", i))
	}
	assert.ElementsMatch(t, want, got, "every concurrently seeded dir must survive")
}

func TestEnsureCopilotDirTrustedInHome_RejectsARelativeDir(t *testing.T) {
	require.Error(t, ensureCopilotDirTrustedInHome(t.TempDir(), "relative/dir"))
}

// The launch-aware entry point resolves COPILOT_HOME exactly as every other
// Copilot gate does: the variable wins over $HOME/.copilot, and a relative
// value is refused rather than resolved against tclaude's own cwd.
func TestEnsureCopilotDirTrustedForLaunch_FollowsCopilotHome(t *testing.T) {
	home := t.TempDir()
	relocated := filepath.Join(t.TempDir(), "elsewhere")
	getenv := func(name string) string {
		if name == CopilotHomeEnvVar {
			return relocated
		}
		return ""
	}
	require.NoError(t, EnsureCopilotDirTrustedForLaunch(getenv, home, "/work/proj"))

	data, err := os.ReadFile(filepath.Join(relocated, CopilotConfigFileName))
	require.NoError(t, err, "the relocated home is the one that must carry the entry")
	assert.Equal(t, []string{"/work/proj"}, trustedFolders(t, data))

	_, statErr := os.Stat(filepath.Join(home, ".copilot", CopilotConfigFileName))
	assert.True(t, os.IsNotExist(statErr), "the default location must not be written")
}

func TestEnsureCopilotDirTrustedForLaunch_DefaultsToHomeDotCopilot(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, EnsureCopilotDirTrustedForLaunch(
		func(string) string { return "" }, home, "/work/proj"))

	data, err := os.ReadFile(filepath.Join(home, ".copilot", CopilotConfigFileName))
	require.NoError(t, err)
	assert.Equal(t, []string{"/work/proj"}, trustedFolders(t, data))
}

func TestEnsureCopilotDirTrustedForLaunch_RefusesARelativeCopilotHome(t *testing.T) {
	err := EnsureCopilotDirTrustedForLaunch(
		func(name string) string {
			if name == CopilotHomeEnvVar {
				return "relative/home"
			}
			return ""
		}, t.TempDir(), "/work/proj")
	require.Error(t, err)
	assert.Contains(t, err.Error(), CopilotHomeEnvVar)
}

// A symlinked launch dir seeds BOTH spellings: tclaude is handed the cwd as the
// operator's shell spells it, while Copilot compares its own resolved cwd
// (TCL-987's lesson, applied to the trust check rather than the conv store).
func TestCopilotTrustSpellings_IncludesTheResolvedForm(t *testing.T) {
	// Resolved up front: on macOS t.TempDir hands back the /var/folders/…
	// spelling of a /private/var/folders/… directory, which is the very
	// difference under test and would otherwise appear in both arms.
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	physical := filepath.Join(root, "physical")
	require.NoError(t, os.MkdirAll(physical, 0o755))
	link := filepath.Join(root, "link")
	require.NoError(t, os.Symlink(physical, link))

	got := copilotTrustSpellings(link)
	assert.Equal(t, []string{link, physical}, got)

	// A path with no alternate spelling contributes exactly one entry.
	assert.Equal(t, []string{physical}, copilotTrustSpellings(physical))
}
