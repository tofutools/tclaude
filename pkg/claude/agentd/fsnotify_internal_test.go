package agentd

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// White-box test for the follower-map leak fix in handleFire. The leak is a
// timing race in the live monitor (a debounce fire draining after its path's
// Remove), so it is not reproducible through the external fsnotify harness;
// handleFire isolates the eviction logic so it can be driven deterministically.
func TestConvMonitor_HandleFireEvictsFollowerForRemovedPath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	db.ResetForTest()

	m := &convMonitor{projectsDir: filepath.Join(t.TempDir(), "projects")}
	timers := map[string]*pendingTimer{}
	followers := map[string]*convops.ConvFollower{}
	known := map[string]bool{}

	// A path whose file does not exist — the conv was already removed, so
	// the Remove handler cleared known[path] and deleted followers[path]
	// before this stale fire drains.
	path := filepath.Join(m.projectsDir, "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee.jsonl")
	fe := fireEvent{path: path, pt: &pendingTimer{}}

	// known[path] is false: reindex recreates a follower for the dead path,
	// but handleFire must then evict it so the map does not leak.
	m.handleFire(fe, timers, known, followers)
	require.NotContains(t, followers, path,
		"a fire draining after a Remove must not leak a follower")

	// Contrast: for a live (tracked) path, the follower is retained so the
	// next write can increment from its cursor.
	known[path] = true
	m.handleFire(fe, timers, known, followers)
	require.Contains(t, followers, path, "a live path retains its follower")
}
