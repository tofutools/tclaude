package agentd

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"golang.org/x/sys/unix"
)

func TestPinInheritedLaunchDirs_EntryCreationFailureOnlyFallsBackForOptionalSandboxRoots(t *testing.T) {
	required := t.TempDir()
	optional := t.TempDir()
	denyCreate := func(int, string, int, uint32) (int, error) {
		return -1, unix.EACCES
	}

	_, _, _, _, cleanup, err := pinInheritedLaunchDirsWithTokenAndOpen(
		[]string{required}, nil, "required-denied", denyCreate)
	cleanup()
	require.Error(t, err)
	assert.ErrorIs(t, err, unix.EACCES,
		"cwd and Git pins must keep failing closed when marker creation is denied")

	mapping, token, pinned, canonicalOnly, cleanup, err := pinInheritedLaunchDirsWithTokenAndOpen(
		nil, []string{optional}, "optional-denied", denyCreate)
	require.NoError(t, err)
	t.Cleanup(cleanup)
	assert.Equal(t, "optional-denied", token)
	assert.Equal(t, optional, mapping[optional])
	assert.Empty(t, pinned)
	assert.Equal(t, []string{optional}, canonicalOnly,
		"a sandbox grant does not imply permission to create host directory entries")
}

func TestPinInheritedLaunchDirs_WritablePinsAreCheckedAndCleaned(t *testing.T) {
	required := t.TempDir()
	optional := t.TempDir()
	mapping, token, pinned, canonicalOnly, cleanup, err := pinInheritedLaunchDirsWithToken(
		[]string{required}, []string{optional}, "writable-pins")
	require.NoError(t, err)
	assert.Equal(t, required, mapping[required])
	assert.Equal(t, optional, mapping[optional])
	assert.ElementsMatch(t, []string{required, optional}, pinned)
	assert.Empty(t, canonicalOnly)
	for _, dir := range pinned {
		assert.FileExists(t, filepath.Join(dir, dirWriteProofFilePrefix+token))
	}

	cleanup()
	for _, dir := range pinned {
		assert.NoFileExists(t, filepath.Join(dir, dirWriteProofFilePrefix+token))
	}
}

func TestDirWriteChallenge_SingleUse(t *testing.T) {
	tok := mintDirWriteChallenge("conv-a", []string{"/x"}, nil)
	require.NotEmpty(t, tok)

	ch, ok := takeDirWriteChallenge(tok)
	require.True(t, ok)
	assert.Equal(t, "conv-a", ch.convID)
	assert.Equal(t, []string{"/x"}, ch.dirs)

	_, ok = takeDirWriteChallenge(tok)
	assert.False(t, ok, "a taken token must be gone")
}

func TestDirWriteChallenge_ExpiryPruned(t *testing.T) {
	prev := dirWriteProofTTL
	dirWriteProofTTL = -time.Second // already expired at mint
	tok := mintDirWriteChallenge("conv-exp", []string{"/x"}, nil)
	dirWriteProofTTL = prev
	require.NotEmpty(t, tok)

	// The expired entry may still be takeable before a prune runs, but its
	// expiry must read as past — the verify path checks it.
	if ch, ok := takeDirWriteChallenge(tok); ok {
		assert.True(t, time.Now().After(ch.expires), "entry must be expired")
	}

	// Minting again prunes expired leftovers rather than growing the table.
	tok2 := mintDirWriteChallenge("conv-exp", []string{"/x"}, nil)
	require.NotEmpty(t, tok2)
	takeDirWriteChallenge(tok2)
}

func TestDirWriteChallenge_PerConvCapEvictsOldest(t *testing.T) {
	tokens := make([]string, 0, dirWriteProofMaxPerConv+1)
	for i := 0; i <= dirWriteProofMaxPerConv; i++ {
		tok := mintDirWriteChallenge("conv-cap", []string{"/x"}, nil)
		require.NotEmpty(t, tok)
		tokens = append(tokens, tok)
	}
	t.Cleanup(func() {
		for _, tok := range tokens {
			takeDirWriteChallenge(tok)
		}
	})
	_, ok := takeDirWriteChallenge(tokens[0])
	assert.False(t, ok, "minting past the per-conv cap must evict the oldest challenge")
	_, ok = takeDirWriteChallenge(tokens[len(tokens)-1])
	assert.True(t, ok, "the newest challenge must survive")
}

func TestResolveDirWriteProofDirs(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	require.NoError(t, os.Mkdir(real, 0o755))
	link := filepath.Join(base, "link")
	require.NoError(t, os.Symlink(real, link))

	resolved, mapping, err := resolveDirWriteProofDirs([]string{link, real, ""})
	require.NoError(t, err)

	realResolved, err := filepath.EvalSymlinks(real)
	require.NoError(t, err)
	wd, err := os.Getwd()
	require.NoError(t, err)
	wdResolved, err := filepath.EvalSymlinks(wd)
	require.NoError(t, err)

	assert.Equal(t, realResolved, mapping[link], "symlink resolves to its target")
	assert.Equal(t, realResolved, mapping[real])
	assert.Equal(t, wdResolved, mapping[""], "blank means the daemon's own cwd")
	assert.Contains(t, resolved, realResolved)
	assert.Contains(t, resolved, wdResolved)
	// link and real dedupe to one entry.
	assert.Len(t, resolved, 2)
}

func TestReassertDirWriteProof(t *testing.T) {
	// Empty (exempt / unverified) is always fine.
	assert.Nil(t, reassertDirWriteProof(nil))

	// Canonicalise the temp base up front: production only ever hands
	// reassertDirWriteProof paths that were already resolved by
	// resolveDirWriteProofDirs, and on macOS t.TempDir() sits under the
	// /var -> /private/var symlink, so a raw temp path is non-canonical and
	// would (correctly) be rejected — that is not the case under test here.
	base, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	real := filepath.Join(base, "real")
	require.NoError(t, os.Mkdir(real, 0o755))

	// A canonical, unchanged dir passes.
	assert.Nil(t, reassertDirWriteProof([]string{real}))

	// Swapped for a symlink to elsewhere → refused (the TOCTOU case).
	other := filepath.Join(base, "other")
	require.NoError(t, os.Mkdir(other, 0o755))
	require.NoError(t, os.RemoveAll(real))
	require.NoError(t, os.Symlink(other, real))
	fail := reassertDirWriteProof([]string{real})
	require.NotNil(t, fail)
	assert.Equal(t, "write_proof_failed", fail.Kind)

	// Removed entirely → refused.
	require.NoError(t, os.Remove(real))
	assert.NotNil(t, reassertDirWriteProof([]string{real}))
}

func TestChildSandboxGrantsDirWrite(t *testing.T) {
	assert.False(t, childSandboxGrantsDirWrite(harness.CodexName, harness.SandboxReadOnly, ""))
	assert.True(t, childSandboxGrantsDirWrite(harness.CodexName, harness.SandboxWorkspaceWrite, ""))
	assert.True(t, childSandboxGrantsDirWrite(harness.CodexName, harness.SandboxManagedProfile, ""))
	assert.True(t, childSandboxGrantsDirWrite(harness.DefaultName, harness.ClaudeSandboxInherit, ""))
	assert.True(t, childSandboxGrantsDirWrite(harness.DefaultName, harness.ClaudeSandboxOn, ""))
	assert.True(t, childSandboxGrantsDirWrite(harness.OpenCodeName, harness.OpenCodeSandboxAccessControl, ""))
	assert.True(t, childSandboxGrantsDirWrite(harness.OpenCodeName, harness.OpenCodeSandboxOff, ""))
	assert.True(t, childSandboxGrantsDirWrite(harness.OpenCodeName, "unknown", ""),
		"an unclassified OpenCode mode must not bypass directory proof before the later fail-closed mode guard")
	assert.True(t, childSandboxGrantsDirWrite("", "", ""))

	// TCL-991: under tclaude's own wall the harness-native mode says nothing
	// about cwd write — the launch grants it regardless — so no mode may take
	// the no-cwd-write exemption there.
	for _, implementation := range []string{
		string(sandboxpolicy.ImplementationTclaudeLayer),
		string(sandboxpolicy.ImplementationStacked),
		"not-an-implementation",
	} {
		assert.Truef(t, childSandboxGrantsDirWrite(
			harness.CodexName, harness.SandboxReadOnly, implementation),
			"codex read-only under %q must still prove its launch dir", implementation)
	}
	// The exemption survives where it is still true.
	assert.False(t, childSandboxGrantsDirWrite(harness.CodexName, harness.SandboxReadOnly,
		string(sandboxpolicy.ImplementationHarnessBuiltin)))
	assert.False(t, childSandboxGrantsDirWrite(harness.CodexName, harness.SandboxReadOnly,
		string(sandboxpolicy.ImplementationOff)))
}

func TestOpenCodeOffParentIsDirWriteProofExempt(t *testing.T) {
	setupTestDB(t)
	const convID = "opencode-off-parent"
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "sess-opencode-off-parent", ConvID: convID, Cwd: t.TempDir(),
		Harness: harness.OpenCodeName, HarnessBuiltinMode: harness.OpenCodeSandboxOff,
	}))
	exempt, err := dirWriteProofCallerExempt(convID)
	require.NoError(t, err)
	assert.True(t, exempt)
}
