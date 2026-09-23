package sandboxpolicy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCommonRuleCatalogResolvesAuditedPlatformPaths(t *testing.T) {
	home := t.TempDir()
	canonicalHome, err := filepath.EvalSymlinks(home)
	require.NoError(t, err)
	linux, err := CommonRuleCatalog(home, "linux")
	require.NoError(t, err)
	byID := map[string]CommonRule{}
	for _, rule := range linux {
		byID[rule.ID] = rule
	}
	assert.Contains(t, byID[CommonRuleBrowserProfiles].Paths, filepath.Join(canonicalHome, ".config", "google-chrome"))
	assert.Contains(t, byID[CommonRuleToolchainCaches].Paths, filepath.Join(canonicalHome, "go", "pkg", "mod"))
	assert.NotEmpty(t, byID[CommonRuleToolchainCaches].Warning)
	// The home preset is the one that cannot stand alone; its warning is the
	// only thing telling an operator they must add reopens too.
	assert.Equal(t, []string{canonicalHome}, byID[CommonRuleHome].Paths)
	assert.NotEmpty(t, byID[CommonRuleHome].Warning)
	assert.Equal(t, CommonRuleTierHome, byID[CommonRuleHome].Tier)

	mac, err := CommonRuleCatalog(home, "darwin")
	require.NoError(t, err)
	for _, rule := range mac {
		byID[rule.ID] = rule
	}
	assert.Contains(t, byID[CommonRuleBrowserProfiles].Paths, filepath.Join(canonicalHome, "Library", "Application Support", "Firefox", "Profiles"))
}

func TestCommonRuleCatalogCanonicalizesExistingSymlinkLeaves(t *testing.T) {
	home := t.TempDir()
	target := t.TempDir()
	canonicalTarget, err := filepath.EvalSymlinks(target)
	require.NoError(t, err)
	require.NoError(t, os.Symlink(target, filepath.Join(home, ".ssh")))
	catalog, err := CommonRuleCatalog(home, "linux")
	require.NoError(t, err)
	for _, rule := range catalog {
		if rule.ID == CommonRuleSSH {
			assert.Equal(t, []string{canonicalTarget}, rule.Paths)
			return
		}
	}
	t.Fatal("SSH rule missing")
}

func TestCommonRuleCatalogCanonicalizesMissingLeafThroughSymlinkedAncestor(t *testing.T) {
	home := t.TempDir()
	target := t.TempDir()
	canonicalTarget, err := filepath.EvalSymlinks(target)
	require.NoError(t, err)
	require.NoError(t, os.Symlink(target, filepath.Join(home, ".config")))

	catalog, err := CommonRuleCatalog(home, "linux")
	require.NoError(t, err)
	for _, rule := range catalog {
		if rule.ID == CommonRuleVCSTokens {
			assert.Contains(t, rule.Paths, filepath.Join(canonicalTarget, "gh"))
			return
		}
	}
	t.Fatal("VCS token rule missing")
}

// A common rule is a row inserter, not a mechanism: inserting its paths as deny
// grants must produce an ordinary profile that normalizes byte-identically to
// the same rows authored by hand.
func TestCommonRulePathsInsertAsOrdinaryDenyRows(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".ssh"), 0o700))
	catalog, err := CommonRuleCatalog(home, "linux")
	require.NoError(t, err)
	var inserted []FilesystemGrant
	for _, rule := range catalog {
		if rule.ID != CommonRuleSSH {
			continue
		}
		for _, path := range rule.Paths {
			inserted = append(inserted, FilesystemGrant{Path: path, Access: AccessDeny})
		}
	}
	require.NotEmpty(t, inserted)
	normalized, _, err := NormalizeForPersistence(Profile{Name: "p", Filesystem: inserted})
	require.NoError(t, err)
	assert.Equal(t, inserted, normalized.Filesystem)
}

// The removed mechanism must leave no trace on the wire: a profile that carries
// only ordinary rows serializes without any baseline/exclusion keys, and a
// payload that still has them decodes with them dropped rather than rejected.
func TestRemovedReadBaselineFieldsAreAbsentAndIgnored(t *testing.T) {
	b, err := json.Marshal(Profile{Name: "legacy"})
	require.NoError(t, err)
	assert.NotContains(t, string(b), "read_baseline")

	var decoded Profile
	require.NoError(t, json.Unmarshal([]byte(`{"name":"legacy","read_baseline":"minimal","read_baseline_exclusions":["secrets.ssh"]}`), &decoded))
	assert.Equal(t, Profile{Name: "legacy"}, decoded)
}

// The harness-state presets are the shortcut for letting an agent run another
// harness with `tclaude run`: they insert WRITE rows, and the Claude preset
// includes the ~/.claude.json settings file, which only a file row can name.
func TestCommonRuleCatalogHarnessStatePresetsInsertWriteRows(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	canonicalHome, err := filepath.EvalSymlinks(home)
	require.NoError(t, err)
	for _, dir := range []string{"sessions", "projects", "todos"} {
		require.NoError(t, os.MkdirAll(filepath.Join(home, ".claude", dir), 0o700))
	}
	require.NoError(t, os.WriteFile(filepath.Join(home, ".claude", ".credentials.json"), []byte("{}"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".claude.json"), []byte("{}"), 0o600))
	catalog, err := CommonRuleCatalog(home, "linux")
	require.NoError(t, err)
	byID := map[string]CommonRule{}
	for _, rule := range catalog {
		byID[rule.ID] = rule
	}
	for _, id := range []string{CommonRuleClaudeState, CommonRuleCodexState, CommonRuleCopilotState, CommonRuleOpenCodeState} {
		rule := byID[id]
		assert.Equal(t, AccessWrite, rule.Access, id)
		assert.Equal(t, CommonRuleTierHarness, rule.Tier, id)
		assert.NotEmpty(t, rule.Warning, id)
		assert.NotEmpty(t, rule.Paths, id)
	}
	assert.Equal(t, []string{filepath.Join(canonicalHome, ".codex")}, byID[CommonRuleCodexState].Paths)
	// ~/.claude itself would reach the protected ~/.claude/sessions, so the
	// preset grants its other entries one by one.
	assert.ElementsMatch(t, []string{
		filepath.Join(canonicalHome, ".claude.json"),
		filepath.Join(canonicalHome, ".claude", ".credentials.json"),
		filepath.Join(canonicalHome, ".claude", "projects"),
		filepath.Join(canonicalHome, ".claude", "todos"),
	}, byID[CommonRuleClaudeState].Paths)
	assert.Empty(t, byID[CommonRuleSSH].Access, "deny presets keep omitting access for older dashboards")

	var inserted []FilesystemGrant
	for _, path := range byID[CommonRuleClaudeState].Paths {
		inserted = append(inserted, FilesystemGrant{Path: path, Access: AccessWrite})
	}
	normalized, _, err := NormalizeForPersistence(Profile{Name: "p", Filesystem: inserted})
	require.NoError(t, err, "the inserted rows must save as an ordinary profile")
	var saved []FilesystemGrant
	for _, grant := range normalized.Filesystem {
		// Normalization records the host kind (file/directory); the inserted
		// path and access must come through unchanged.
		saved = append(saved, FilesystemGrant{Path: grant.Path, Access: grant.Access})
	}
	assert.ElementsMatch(t, inserted, saved)
}
