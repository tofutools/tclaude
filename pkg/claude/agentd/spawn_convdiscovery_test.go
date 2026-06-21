package agentd

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// fakeDiscoveryStore is a harness.ConvStore that returns a fixed entry list
// from ListConvs; the other methods are never reached by
// discoverSpawnedConvID.
type fakeDiscoveryStore struct {
	entries []convops.SessionEntry
	err     error
}

func (f fakeDiscoveryStore) ListConvs(string) ([]convops.SessionEntry, error) {
	return f.entries, f.err
}
func (fakeDiscoveryStore) Resolve(string, string, bool) (*harness.ConvRef, error) { return nil, nil }
func (fakeDiscoveryStore) Title(string) (string, error)                           { return "", nil }
func (fakeDiscoveryStore) SetTitle(string, string) error                          { return nil }
func (fakeDiscoveryStore) Exists(string, string) (bool, error)                    { return true, nil }

func harnessWithConvs(s harness.ConvStore) *harness.Harness {
	return &harness.Harness{Name: "fake", Convs: s}
}

// discoverSpawnedConvID is the spawn-freeze fix (JOH-205): for a harness that
// reports its conv-id lazily (Codex fires no hook until the first user turn),
// the daemon discovers the conv-id from the conv store — the rollout written
// at launch — rather than blocking until first input. These pin the
// correlation rules: at/after launch, newest, not-already-claimed.
func TestDiscoverSpawnedConvID(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	db.ResetForTest()

	launch := time.Now()
	const cwd = "/work/repo"
	fresh := func(id string, created time.Time) convops.SessionEntry {
		return convops.SessionEntry{
			SessionID:   id,
			ProjectPath: cwd,
			Created:     created.UTC().Format(time.RFC3339),
			Harness:     "fake",
		}
	}

	t.Run("nil harness or nil store yields empty (CC stays on the hook)", func(t *testing.T) {
		assert.Empty(t, discoverSpawnedConvID(nil, cwd, launch))
		assert.Empty(t, discoverSpawnedConvID(&harness.Harness{Name: "x"}, cwd, launch))
	})

	t.Run("picks the conv created at/after launch, ignoring pre-existing ones", func(t *testing.T) {
		h := harnessWithConvs(fakeDiscoveryStore{entries: []convops.SessionEntry{
			fresh("old-conv", launch.Add(-time.Hour)),
			fresh("spawned-conv", launch.Add(1*time.Second)),
		}})
		assert.Equal(t, "spawned-conv", discoverSpawnedConvID(h, cwd, launch))
	})

	t.Run("only pre-launch convs => empty (keep polling)", func(t *testing.T) {
		h := harnessWithConvs(fakeDiscoveryStore{entries: []convops.SessionEntry{
			fresh("old-conv", launch.Add(-time.Hour)),
		}})
		assert.Empty(t, discoverSpawnedConvID(h, cwd, launch))
	})

	t.Run("newest wins among fresh convs", func(t *testing.T) {
		h := harnessWithConvs(fakeDiscoveryStore{entries: []convops.SessionEntry{
			fresh("fresh-older", launch.Add(1*time.Second)),
			fresh("fresh-newer", launch.Add(2*time.Second)),
		}})
		assert.Equal(t, "fresh-newer", discoverSpawnedConvID(h, cwd, launch))
	})

	t.Run("skips a conv already claimed by another session row", func(t *testing.T) {
		// The newer conv already belongs to a session row (a concurrent spawn
		// in the same cwd), so discovery falls back to the unclaimed one
		// instead of stealing it.
		require.NoError(t, db.SaveSession(&db.SessionRow{
			ID: "spwn-other", ConvID: "fresh-claimed", TmuxSession: "spwn-other", Harness: "fake",
		}))
		h := harnessWithConvs(fakeDiscoveryStore{entries: []convops.SessionEntry{
			fresh("fresh-unclaimed", launch.Add(1*time.Second)),
			fresh("fresh-claimed", launch.Add(2*time.Second)),
		}})
		assert.Equal(t, "fresh-unclaimed", discoverSpawnedConvID(h, cwd, launch))
	})

	t.Run("scan error yields empty (caller keeps polling)", func(t *testing.T) {
		h := harnessWithConvs(fakeDiscoveryStore{err: assert.AnError})
		assert.Empty(t, discoverSpawnedConvID(h, cwd, launch))
	})

	t.Run("falls back to FileMtime when Created is empty", func(t *testing.T) {
		h := harnessWithConvs(fakeDiscoveryStore{entries: []convops.SessionEntry{
			{SessionID: "mtime-conv", ProjectPath: cwd, FileMtime: launch.Add(1 * time.Second).Unix()},
		}})
		assert.Equal(t, "mtime-conv", discoverSpawnedConvID(h, cwd, launch))
	})
}
