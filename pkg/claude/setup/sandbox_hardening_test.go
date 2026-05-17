package setup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// decodeTree decodes a JSON object string into the generic tree shape
// the merge engine works on — the same shape json.Unmarshal produces
// from a real settings.json (objects -> map[string]any, arrays ->
// []any, numbers -> float64).
func decodeTree(t *testing.T, jsonStr string) map[string]any {
	t.Helper()
	var tree map[string]any
	require.NoError(t, json.Unmarshal([]byte(jsonStr), &tree))
	return tree
}

// freshSpecTree returns the hardening spec round-tripped through JSON,
// i.e. a tree that already contains every entry the hardening installs.
// Used to drive the idempotency tests.
func freshSpecTree(t *testing.T) map[string]any {
	t.Helper()
	b, err := json.Marshal(sandboxHardeningSpec())
	require.NoError(t, err)
	return decodeTree(t, string(b))
}

// runMerge merges the hardening spec into tree and returns the report.
func runMerge(tree map[string]any) *hardeningReport {
	r := &hardeningReport{}
	mergeHardening("", tree, sandboxHardeningSpec(), r)
	return r
}

// The spec has 12 leaf values: 2 scalars (sandbox.enabled,
// sandbox.network.allowAllUnixSockets) and 10 array elements
// (allowUnixSockets 1, denyWrite 2, denyRead 2, allowRead 1,
// permissions.deny 4).
const specLeafCount = 12

// --- merge engine: the risky part, tested hard --------------------------------

// Merging into an empty tree adds every entry and produces a tree deep-equal
// to the spec.
func TestMergeHardening_EmptyTree(t *testing.T) {
	tree := map[string]any{}
	r := runMerge(tree)

	assert.True(t, r.changed())
	assert.Len(t, r.added, specLeafCount)
	assert.Empty(t, r.alreadyPresent)
	assert.Empty(t, r.scalarConflicts)
	assert.Empty(t, r.typeConflicts)
	assert.Equal(t, sandboxHardeningSpec(), tree, "merged tree must equal the spec")
}

// A second merge over a fully-hardened tree is a pure no-op: nothing added,
// every entry reported as already present, and the tree byte-identical.
func TestMergeHardening_Idempotent(t *testing.T) {
	tree := freshSpecTree(t)
	before, err := json.Marshal(tree)
	require.NoError(t, err)

	r := runMerge(tree)

	assert.False(t, r.changed(), "second merge must not change anything")
	assert.Empty(t, r.added)
	assert.Len(t, r.alreadyPresent, specLeafCount)
	assert.Empty(t, r.scalarConflicts)
	assert.Empty(t, r.typeConflicts)

	after, err := json.Marshal(tree)
	require.NoError(t, err)
	assert.JSONEq(t, string(before), string(after), "tree must be unchanged")
}

// Keys and array elements tclaude knows nothing about must round-trip
// untouched — both at the top level and nested inside the objects the
// hardening also writes into.
func TestMergeHardening_PreservesUnknownFields(t *testing.T) {
	tree := decodeTree(t, `{
	  "model": "opus",
	  "hooks": {"Stop": [{"hooks": [{"type": "command", "command": "x"}]}]},
	  "sandbox": {
	    "enabled": true,
	    "experimental": {"future": "knob"},
	    "filesystem": {"allowWrite": ["/tmp/scratch"]}
	  },
	  "permissions": {
	    "allow": ["Bash(ls:*)"],
	    "deny": ["Edit(/etc/**)"]
	  }
	}`)

	runMerge(tree)

	// Untouched unknowns survive.
	assert.Equal(t, "opus", tree["model"])
	assert.Contains(t, tree, "hooks")
	sandbox := tree["sandbox"].(map[string]any)
	assert.Equal(t, map[string]any{"future": "knob"}, sandbox["experimental"])
	fs := sandbox["filesystem"].(map[string]any)
	assert.Equal(t, []any{"/tmp/scratch"}, fs["allowWrite"], "unknown sibling key preserved")
	perms := tree["permissions"].(map[string]any)
	assert.Equal(t, []any{"Bash(ls:*)"}, perms["allow"], "unknown sibling key preserved")

	// The hardening still landed alongside the unknowns.
	assert.Equal(t, []any{"~/.tclaude", "~/.claude/sessions"}, fs["denyWrite"])
	// permissions.deny kept its pre-existing element and got the new ones.
	assert.Equal(t, []any{
		"Edit(/etc/**)",
		"Edit(~/.tclaude/**)",
		"Read(~/.tclaude/**)",
		"Edit(~/.claude/sessions/**)",
		"Read(~/.claude/sessions/**)",
	}, perms["deny"])
}

// An existing array keeps its own elements and order; only the missing
// hardening elements are appended, and an element already present is not
// duplicated.
func TestMergeHardening_ArrayAppendAndDedupe(t *testing.T) {
	tree := decodeTree(t, `{
	  "sandbox": {
	    "filesystem": {
	      "denyWrite": ["/custom/path", "~/.tclaude"]
	    }
	  }
	}`)

	r := runMerge(tree)

	fs := tree["sandbox"].(map[string]any)["filesystem"].(map[string]any)
	assert.Equal(t, []any{"/custom/path", "~/.tclaude", "~/.claude/sessions"},
		fs["denyWrite"], "custom + existing kept, only the missing element appended")

	// "~/.tclaude" was already there -> reported as already-present, not added.
	assert.NotContains(t, strings.Join(r.added, "\n"), `denyWrite += "~/.tclaude"`)
	assert.Contains(t, strings.Join(r.added, "\n"), `denyWrite += "~/.claude/sessions"`)
	joined := strings.Join(r.alreadyPresent, "\n")
	assert.Contains(t, joined, `~/.tclaude`)
}

// A scalar already present with a DIFFERENT value (the brief's example:
// sandbox.enabled is false) must be left untouched and reported as a
// conflict — never silently flipped.
func TestMergeHardening_ScalarConflict(t *testing.T) {
	tree := decodeTree(t, `{"sandbox": {"enabled": false}}`)

	r := runMerge(tree)

	sandbox := tree["sandbox"].(map[string]any)
	assert.Equal(t, false, sandbox["enabled"], "conflicting scalar must NOT be overwritten")
	require.Len(t, r.scalarConflicts, 1)
	assert.Contains(t, r.scalarConflicts[0], "sandbox.enabled")
	assert.Contains(t, r.scalarConflicts[0], "left unchanged")

	// The rest of the hardening still applied around the conflict.
	assert.True(t, r.changed())
	assert.Contains(t, sandbox, "filesystem")
}

// A scalar already present with the wanted value is a no-op, not a conflict.
func TestMergeHardening_ScalarAlreadyCorrect(t *testing.T) {
	tree := decodeTree(t, `{"sandbox": {"enabled": true}}`)

	r := runMerge(tree)

	assert.Empty(t, r.scalarConflicts)
	assert.Contains(t, strings.Join(r.alreadyPresent, "\n"), "sandbox.enabled")
}

// A key that should hold an array but holds a scalar is a type conflict:
// warn and skip, never clobber.
func TestMergeHardening_TypeConflict_ArraySlot(t *testing.T) {
	tree := decodeTree(t, `{"permissions": {"deny": "Edit(/etc/**)"}}`)

	r := runMerge(tree)

	assert.Equal(t, "Edit(/etc/**)", tree["permissions"].(map[string]any)["deny"],
		"type-conflicting value must be left as-is")
	require.Len(t, r.typeConflicts, 1)
	assert.Contains(t, r.typeConflicts[0], "permissions.deny")
	assert.Contains(t, r.typeConflicts[0], "JSON array")
}

// A key that should hold an object but holds a scalar is a type conflict:
// the whole sub-tree below it is skipped, with a single warning.
func TestMergeHardening_TypeConflict_ObjectSlot(t *testing.T) {
	tree := decodeTree(t, `{"sandbox": "yes please"}`)

	r := runMerge(tree)

	assert.Equal(t, "yes please", tree["sandbox"], "type-conflicting value must be left as-is")
	require.Len(t, r.typeConflicts, 1)
	assert.Contains(t, r.typeConflicts[0], "sandbox")
	assert.Contains(t, r.typeConflicts[0], "JSON object")
	// Nothing below the conflicting sandbox node was forced in...
	assert.NotContains(t, strings.Join(r.added, "\n"), "sandbox.")
	// ...but the unrelated permissions sub-tree was still added.
	assert.Contains(t, tree, "permissions")
}

// A key that should hold a scalar but holds a container is a type conflict.
func TestMergeHardening_TypeConflict_ScalarSlot(t *testing.T) {
	tree := decodeTree(t, `{"sandbox": {"enabled": ["unexpected"]}}`)

	r := runMerge(tree)

	assert.Equal(t, []any{"unexpected"}, tree["sandbox"].(map[string]any)["enabled"])
	require.Len(t, r.typeConflicts, 1)
	assert.Contains(t, r.typeConflicts[0], "sandbox.enabled")
	assert.Contains(t, r.typeConflicts[0], "scalar")
}

// The in-code spec must stay a faithful copy of the recommended config
// block in docs/sandbox-hardening.md — the doc is the source of truth.
func TestSandboxHardeningSpec_MatchesDoc(t *testing.T) {
	docPath := filepath.Join(findRepoRoot(t), filepath.FromSlash(sandboxHardeningDocPath))
	body, err := os.ReadFile(docPath)
	require.NoError(t, err)

	// Extract the first ```json fenced block from the doc.
	var block []string
	inBlock := false
	for _, line := range strings.Split(string(body), "\n") {
		if !inBlock {
			if strings.TrimSpace(line) == "```json" {
				inBlock = true
			}
			continue
		}
		if strings.TrimSpace(line) == "```" {
			break
		}
		block = append(block, line)
	}
	require.NotEmpty(t, block, "no ```json block found in %s", sandboxHardeningDocPath)

	docTree := decodeTree(t, strings.Join(block, "\n"))
	assert.Equal(t, docTree, sandboxHardeningSpec(),
		"sandboxHardeningSpec() has drifted from the config block in %s", sandboxHardeningDocPath)
}

// --- file-level apply ---------------------------------------------------------

// A missing settings file is treated as {} and created with the full
// hardening; no backup is made because there was nothing to back up.
func TestApplySandboxHardening_MissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")

	res, err := applySandboxHardening(path)
	require.NoError(t, err)

	assert.True(t, res.wrote)
	assert.Empty(t, res.backupPath, "nothing to back up for a missing file")
	assert.FileExists(t, path)

	tree := readTree(t, path)
	assert.Equal(t, sandboxHardeningSpec(), tree)
}

// An empty file is also treated as {}.
func TestApplySandboxHardening_EmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	require.NoError(t, os.WriteFile(path, []byte("   \n"), 0o644))

	res, err := applySandboxHardening(path)
	require.NoError(t, err)

	assert.True(t, res.wrote)
	assert.Equal(t, sandboxHardeningSpec(), readTree(t, path))
}

// Running apply twice leaves the file byte-identical the second time and
// makes no second backup — the brief's hard idempotency requirement.
func TestApplySandboxHardening_Idempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	res1, err := applySandboxHardening(path)
	require.NoError(t, err)
	assert.True(t, res1.wrote)
	afterFirst, err := os.ReadFile(path)
	require.NoError(t, err)

	res2, err := applySandboxHardening(path)
	require.NoError(t, err)
	assert.False(t, res2.wrote, "second run must not rewrite the file")
	assert.Empty(t, res2.backupPath, "second run must not create a backup")
	assert.Empty(t, res2.report.added)

	afterSecond, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, afterFirst, afterSecond, "file must be byte-identical after a no-op run")

	// Exactly zero backups: run 1 had no file to back up, run 2 did not write.
	assert.Empty(t, backupFiles(t, dir))
}

// Unknown keys in a real-looking settings file survive the apply round-trip.
func TestApplySandboxHardening_PreservesUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	original := `{
	  "model": "opus",
	  "statusLine": {"type": "command", "command": "tclaude status-bar"},
	  "hooks": {"Stop": [{"hooks": [{"type": "command", "command": "tclaude session hook-callback"}]}]},
	  "permissions": {"allow": ["Bash(go test:*)"]}
	}`
	require.NoError(t, os.WriteFile(path, []byte(original), 0o644))

	res, err := applySandboxHardening(path)
	require.NoError(t, err)
	assert.True(t, res.wrote)

	tree := readTree(t, path)
	assert.Equal(t, "opus", tree["model"])
	assert.Equal(t, map[string]any{"type": "command", "command": "tclaude status-bar"},
		tree["statusLine"])
	assert.Contains(t, tree, "hooks")
	perms := tree["permissions"].(map[string]any)
	assert.Equal(t, []any{"Bash(go test:*)"}, perms["allow"], "unknown permissions.allow preserved")
	assert.Contains(t, perms, "deny", "hardening added permissions.deny alongside")
	assert.Contains(t, tree, "sandbox")
}

// A backup is taken before an existing, non-empty file is rewritten, and
// it captures the pre-merge contents verbatim.
func TestApplySandboxHardening_BacksUpExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	original := `{"model": "opus"}`
	require.NoError(t, os.WriteFile(path, []byte(original), 0o644))

	res, err := applySandboxHardening(path)
	require.NoError(t, err)

	require.NotEmpty(t, res.backupPath, "an existing file must be backed up")
	backup, err := os.ReadFile(res.backupPath)
	require.NoError(t, err)
	assert.Equal(t, original, string(backup), "backup must hold the pre-merge contents")
	assert.True(t, strings.HasPrefix(filepath.Base(res.backupPath), "settings.json.bak-"))
}

// The backup must inherit the original file's permission bits — a
// private (0600) settings.json must not be copied into a 0644 backup.
func TestApplySandboxHardening_BackupInheritsFileMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits are not meaningful on Windows")
	}
	path := filepath.Join(t.TempDir(), "settings.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"model": "opus"}`), 0o600))

	res, err := applySandboxHardening(path)
	require.NoError(t, err)
	require.NotEmpty(t, res.backupPath)

	info, err := os.Stat(res.backupPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(),
		"backup must inherit the private mode of the original")
}

// Rewriting an existing settings.json must preserve its permission mode
// — a private (0600) file must not come back 0644. This pins the
// behaviour regardless of how the rewrite is implemented (today
// os.WriteFile keeps an existing file's mode; an atomic temp+rename
// refactor would need the explicit mode-preservation to hold this).
func TestApplySandboxHardening_RewritePreservesFileMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits are not meaningful on Windows")
	}
	path := filepath.Join(t.TempDir(), "settings.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"model": "opus"}`), 0o600))

	res, err := applySandboxHardening(path)
	require.NoError(t, err)
	require.True(t, res.wrote)

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(),
		"rewritten settings.json must keep its original private mode")
}

// A scalar conflict in a real file: the conflicting value is preserved on
// disk, the rest of the hardening is still written, and the conflict is
// reported.
func TestApplySandboxHardening_ScalarConflictPreservedOnDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"sandbox": {"enabled": false}}`), 0o644))

	res, err := applySandboxHardening(path)
	require.NoError(t, err)
	assert.True(t, res.wrote)
	require.Len(t, res.report.scalarConflicts, 1)

	tree := readTree(t, path)
	sandbox := tree["sandbox"].(map[string]any)
	assert.Equal(t, false, sandbox["enabled"], "conflicting scalar must survive on disk")
	assert.Contains(t, sandbox, "filesystem", "the rest of the hardening still applied")
}

// A settings file whose top-level JSON is not an object must be rejected,
// not silently clobbered — rewriting it would lose the user's data.
func TestApplySandboxHardening_RejectsNonObjectJSON(t *testing.T) {
	for _, body := range []string{`[1, 2, 3]`, `"a string"`, `{not json`} {
		path := filepath.Join(t.TempDir(), "settings.json")
		require.NoError(t, os.WriteFile(path, []byte(body), 0o644))

		_, err := applySandboxHardening(path)
		require.Error(t, err, "non-object/invalid JSON %q must error", body)

		// The file must be left exactly as it was.
		got, readErr := os.ReadFile(path)
		require.NoError(t, readErr)
		assert.Equal(t, body, string(got), "input file must not be modified on error")
	}
}

// --- installSandboxHardening: the production entry point ----------------------

// installSandboxHardening writes the hardening into the real settings
// path (HOME-relative) and is idempotent across runs.
func TestInstallSandboxHardening_EndToEnd(t *testing.T) {
	home := tempHome(t)
	settingsPath := filepath.Join(home, ".claude", "settings.json")

	out := captureStdout(t, func() {
		require.NoError(t, installSandboxHardening())
	})
	assert.Contains(t, out, "✓ Added")
	assert.Contains(t, out, "written to")
	assert.Equal(t, sandboxHardeningSpec(), readTree(t, settingsPath))

	out2 := captureStdout(t, func() {
		require.NoError(t, installSandboxHardening())
	})
	assert.Contains(t, out2, "already in place — no changes")
	assert.NotContains(t, out2, "✓ Added")
}

// installExtras wires --install-sandbox-hardening (and --install-all) to
// the installer, and does not run it without the flag.
func TestInstallExtras_SandboxHardening(t *testing.T) {
	settingsHasSandbox := func(t *testing.T, home string) bool {
		t.Helper()
		path := filepath.Join(home, ".claude", "settings.json")
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			return false
		}
		require.NoError(t, err)
		var tree map[string]any
		require.NoError(t, json.Unmarshal(data, &tree))
		_, ok := tree["sandbox"]
		return ok
	}

	t.Run("flag installs it", func(t *testing.T) {
		home := tempHome(t)
		require.NoError(t, installExtras(&Params{InstallSandboxHardening: true}))
		assert.True(t, settingsHasSandbox(t, home))
		assertNoSkills(t, home)
	})
	t.Run("install-all includes it", func(t *testing.T) {
		home := tempHome(t)
		require.NoError(t, installExtras(&Params{InstallAll: true}))
		assert.True(t, settingsHasSandbox(t, home))
	})
	t.Run("absent without the flag", func(t *testing.T) {
		home := tempHome(t)
		require.NoError(t, installExtras(&Params{InstallAgentSkills: true}))
		assert.False(t, settingsHasSandbox(t, home), "hardening must not run unasked")
	})
}

// --- test helpers -------------------------------------------------------------

// readTree reads a settings file back as a generic JSON tree for assertions.
func readTree(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return decodeTree(t, string(data))
}

// backupFiles lists the settings.json.bak-* files in dir.
func backupFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var baks []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "settings.json.bak-") {
			baks = append(baks, e.Name())
		}
	}
	return baks
}
