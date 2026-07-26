package harness

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// IsCodexRolloutPathForConv is the gate on a rollout path that crossed a
// sandbox boundary before the daemon opened it (TCL-754). The looser
// IsCodexRolloutPath validates only the filename, which is sufficient when
// the reader and the path's origin are the same process and is NOT
// sufficient when they are not.
func TestIsCodexRolloutPathForConv(t *testing.T) {
	const own = "11111111-2222-3333-4444-555555555555"
	const peer = "99999999-2222-3333-4444-555555555555"

	home := t.TempDir()
	sessions := filepath.Join(home, ".codex", "sessions", "2026", "07", "26")
	require.NoError(t, os.MkdirAll(sessions, 0o755))

	write := func(dir, name string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(p, []byte("{}\n"), 0o600))
		return p
	}

	ownRollout := write(sessions, "rollout-2026-07-26T10-00-00-"+own+".jsonl")
	peerRollout := write(sessions, "rollout-2026-07-26T10-00-00-"+peer+".jsonl")

	// A file outside the tree whose NAME is a perfectly valid rollout for
	// the caller's own conv — the exact shape a filename-only check waves
	// through.
	outside := t.TempDir()
	impostor := write(outside, "rollout-2026-07-26T10-00-00-"+own+".jsonl")

	// And the symlink variant: a link inside the tree pointing out of it.
	// This is why containment must be evaluated after resolution.
	link := filepath.Join(sessions, "rollout-2026-07-26T11-00-00-"+own+".jsonl")
	require.NoError(t, os.Symlink(impostor, link))

	assert.True(t, IsCodexRolloutPathForConv(home, own, ownRollout),
		"a session's own rollout must be accepted")

	assert.False(t, IsCodexRolloutPathForConv(home, own, peerRollout),
		"a PEER's rollout is a cross-agent read and must be refused even though it is in the tree")
	assert.False(t, IsCodexRolloutPathForConv(home, own, impostor),
		"a correctly-named file outside the tree must be refused")
	assert.False(t, IsCodexRolloutPathForConv(home, own, link),
		"a symlink out of the tree must be refused: containment is checked after resolution")

	assert.False(t, IsCodexRolloutPathForConv(home, "", ownRollout),
		"no conv-id means no ownership claim to check against; refuse")
	assert.False(t, IsCodexRolloutPathForConv(home, own, filepath.Join(sessions, "notes.txt")),
		"the filename-shape check remains in force")
	assert.False(t, IsCodexRolloutPathForConv(home, own,
		filepath.Join(sessions, "rollout-2026-07-26T12-00-00-"+own+".jsonl")),
		"a path that does not resolve cannot be read either; refuse")

	// The looser predicate still behaves as documented — this pins that
	// the two are genuinely different gates, so nobody collapses them.
	assert.True(t, IsCodexRolloutPath(impostor),
		"the filename-only predicate is deliberately unchanged and still accepts this")
}
