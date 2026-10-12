package harness

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/harness/transcript"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// The PAF context copy beside a morph's transcript.md references the
// attachments the translation already saved instead of writing them twice,
// and a failure to write it does not fail the translation.
func TestTranslationArtifactsContextIsBestEffortAndSharesMedia(t *testing.T) {
	fx := nativeFixtures[0]
	src := mustHarness(t, fx.harness)
	tr, err := src.Transcript.Decode(bytes.NewReader(fx.raw(t)), fx.id)
	require.NoError(t, err)
	plan := func(dir string) *transcript.Plan {
		return transcript.NewPlan(tr, transcript.Options{Target: fx.harness, InlineMedia: false, MediaDir: filepath.Join(dir, "media")})
	}

	dir := filepath.Join(testutil.CanonicalTempDir(t), "morph")
	require.NoError(t, writeTranslationArtifacts(tr, plan(dir), dir))
	ctx, err := os.ReadFile(filepath.Join(dir, "transcript.jsonl"))
	require.NoError(t, err)
	assert.Contains(t, string(ctx), filepath.Join(dir, "media", "media-001.png"), "the context references the translation's saved image")
	_, err = os.Stat(filepath.Join(dir, "context-media"))
	assert.ErrorIs(t, err, os.ErrNotExist, "no second copy of the media")

	// A transcript.jsonl that cannot be written (here: already present) is
	// skipped; the readable transcript and the media are still written.
	dir = filepath.Join(testutil.CanonicalTempDir(t), "morph")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "transcript.jsonl"), []byte("planted"), 0o600))
	require.NoError(t, writeTranslationArtifacts(tr, plan(dir), dir))
	_, err = os.Stat(filepath.Join(dir, "transcript.md"))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(dir, "media", "media-001.png"))
	require.NoError(t, err)
}
