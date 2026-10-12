package agentd

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// The scheduler ticks every minute on every node. A tick with nothing to do
// creates no snapshots directory and does not rewrite an unchanged state
// file.
func TestResilienceIdleTickWritesNothing(t *testing.T) {
	t.Setenv("HOME", testutil.CanonicalTempDir(t))
	dir := filepath.Join(config.DataDir(), "snapshots")

	resilienceTickOnce(time.Now(), "")
	resilienceReceiveRetry()
	_, err := os.Stat(dir)
	assert.ErrorIs(t, err, os.ErrNotExist, "no policies leave no snapshots directory")

	// A policy whose target cannot be resolved changes no state either.
	f := &resilienceFile{Policies: []resiliencePolicy{{ID: "rp_000000000001", Target: "group:missing", EverySeconds: 3600, Keep: 1}}}
	require.NoError(t, saveResilience(f))
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	require.NoError(t, os.Chtimes(resiliencePath(), old, old))
	resilienceTickOnce(time.Now(), "")
	st, err := os.Stat(resiliencePath())
	require.NoError(t, err)
	assert.True(t, st.ModTime().Equal(old), "an unchanged state file is not rewritten")
}
