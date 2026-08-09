package agentd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// humanBytes renders every size an agent reads in an artifact listing or a
// refusal, including the one in the refusal's own threshold. Its unit ladder is
// the kind of arithmetic that is right until it silently is not — an unclamped
// exponent indexes past the end of "KMGT" and panics the daemon.
func TestHumanBytes(t *testing.T) {
	for _, tc := range []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{1, "1 B"},
		{1023, "1023 B"},
		{1024, "1.0 KiB"},
		{2048, "2.0 KiB"},
		{1024*1024 - 1, "1024.0 KiB"},
		{1024 * 1024, "1.0 MiB"},
		{512 << 20, "512.0 MiB"},
		{700 << 20, "700.0 MiB"},
		{1 << 30, "1.0 GiB"},
		{1 << 40, "1.0 TiB"},
		// Past the last unit the exponent must CLAMP rather than index on: a
		// petabyte reads as 1024 TiB, and does not panic.
		{1 << 50, "1024.0 TiB"},
		{1 << 60, "1048576.0 TiB"},
	} {
		assert.Equal(t, tc.want, humanBytes(tc.n), "humanBytes(%d)", tc.n)
	}
}

// The download budget is stated to callers in humanBytes terms, so the constant
// and the rendering have to agree — a cap that refuses at "512 MiB" while the
// help text promises 512 MiB is only true if this holds.
func TestArtifactBudgetRendersAsTheDocumentedFigure(t *testing.T) {
	assert.Equal(t, "512.0 MiB", humanBytes(maxGHArtifactBytes))
}

// A walk that stops early must never be reported as a completed one. The
// guard that matters most is the empty-tree branch: without it, a listing cut
// off at the root entry tells an agent the artifact unpacked to nothing, about
// an artifact that is sitting on disk full of files.
func TestArtifactListingStoppedWalk(t *testing.T) {
	dest := t.TempDir()
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		require.NoError(t, os.WriteFile(filepath.Join(dest, name), []byte("hello"), 0o644))
	}

	t.Run("a deadline that has already passed", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		out, _ := artifactListing(ctx, dest)

		assert.NotContains(t, out, "unpacked to no files at all",
			"a stopped walk counts zero files; calling that an empty artifact is a lie "+
				"about a directory that is not empty")
		assert.Contains(t, out, "at least", "a partial count must not read as a total")
		assert.Contains(t, out, "ran out of time")
		assert.Contains(t, out, "really are at the path above",
			"the caller is still connected on a daemon-side deadline, and must be told the "+
				"files landed rather than left to retry a download that would delete them")
		assert.NotContains(t, out, "went away", "nothing was cancelled")
		assert.Contains(t, out, dest)
	})

	t.Run("a cancelled request", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		out, _ := artifactListing(ctx, dest)

		assert.NotContains(t, out, "unpacked to no files at all")
		assert.Contains(t, out, "at least")
		assert.Contains(t, out, "went away")
		assert.NotContains(t, out, "ran out of time")
	})

	// The ordinary path still reports exact figures — the hedging must not
	// leak into a walk that finished.
	t.Run("a walk that completes", func(t *testing.T) {
		out, walk := artifactListing(context.Background(), dest)
		assert.Contains(t, out, "3 files")
		assert.NotContains(t, out, "at least")
		assert.NotContains(t, out, "stopped")
		assert.NotContains(t, out, "ran out of time")
		// Only a complete walk may be measured against the unpacked-size cap;
		// a floor would refuse honest downloads and pass oversized ones.
		assert.True(t, walk.Complete)
		assert.Equal(t, 3, walk.Files)
		assert.Equal(t, int64(15), walk.Bytes)
	})

	t.Run("a stopped walk is never reported as measurable", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, walk := artifactListing(ctx, dest)
		assert.False(t, walk.Complete,
			"an incomplete byte total must not be handed to the size cap as if it were one")
	})
}

func TestValidateGHArtifactName(t *testing.T) {
	t.Run("accepted", func(t *testing.T) {
		for _, name := range []string{
			"coverage", "test-results", "build (ubuntu-latest)", "logs_1.22", "täckning",
		} {
			got, fault := validateGHArtifactName(name)
			assert.Nil(t, fault, "%q should be a legal artifact name", name)
			assert.Equal(t, name, got)
		}
	})

	// An empty name is not an error — it means "every live artifact", which is
	// the documented default.
	t.Run("empty means every artifact", func(t *testing.T) {
		got, fault := validateGHArtifactName("  ")
		assert.Nil(t, fault)
		assert.Empty(t, got)
	})

	t.Run("refused", func(t *testing.T) {
		for _, name := range []string{
			"-n",                     // reads as a flag
			"--dir=/etc",             // reads as a flag with a value
			"../../etc/passwd",       // steers where gh writes
			`a\b`,                    // ditto, on the other separator
			"tab\there",              // control character
			"a\x00b",                 // NUL
			"quote\"d",               // GitHub refuses it too
			"glob*",                  // ditto
			strings.Repeat("x", 201), // over the length bound
		} {
			_, fault := validateGHArtifactName(name)
			assert.NotNil(t, fault, "%q must not survive being called an artifact name", name)
		}
	})
}
