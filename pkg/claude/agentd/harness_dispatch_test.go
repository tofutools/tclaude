package agentd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// fakeLifecycle reports no in-pane lifecycle commands — modelling a
// harness (like Codex) that renames out-of-band rather than via a slash
// command.
type fakeLifecycle struct{}

func (fakeLifecycle) RenameCommand() string        { return "" }
func (fakeLifecycle) CompactCommand() string       { return "" }
func (fakeLifecycle) SoftExitCommand() string      { return "" }
func (fakeLifecycle) RemoteControlCommand() string { return "" }

// fakeConvStore records SetTitle calls so the test can assert the rename
// routed to the title store rather than the (absent) injection path.
type fakeConvStore struct{ lastConv, lastTitle string }

func (f *fakeConvStore) ListConvs(string) ([]convops.SessionEntry, error)       { return nil, nil }
func (f *fakeConvStore) Resolve(string, string, bool) (*harness.ConvRef, error) { return nil, nil }
func (f *fakeConvStore) Title(string) (string, error)                           { return "", nil }
func (f *fakeConvStore) Exists(string, string) (bool, error)                    { return true, nil }
func (f *fakeConvStore) SetTitle(convID, title string) error {
	f.lastConv, f.lastTitle = convID, title
	return nil
}

// TestDeliverRename_RoutesToTitleStoreForOutOfBandHarness pins the
// capability routing the existing flow tests don't reach: a harness with
// no in-pane rename command renames via ConvStore.SetTitle, not a
// send-keys injection (which would need a live pane this test never
// starts).
func TestDeliverRename_RoutesToTitleStoreForOutOfBandHarness(t *testing.T) {
	setupTestDB(t)

	store := &fakeConvStore{}
	harness.Register(&harness.Harness{
		Name:  "faketitle-test",
		Life:  fakeLifecycle{},
		Convs: store,
	})

	const convID = "conv-faketitle-1"
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "s-ft", ConvID: convID, Status: "running", Harness: "faketitle-test",
	}))

	require.True(t, deliverRename(convID, "new-title"), "rename should be delivered via the title store")
	assert.Equal(t, convID, store.lastConv)
	assert.Equal(t, "new-title", store.lastTitle)
}

// TestHarnessForConv_DefaultsToClaude pins resolution: a claude-tagged
// session resolves to claude; an unknown tag falls back to claude rather
// than failing; a conv with no session row defaults to claude.
func TestHarnessForConv_DefaultsToClaude(t *testing.T) {
	setupTestDB(t)

	require.NoError(t, db.SaveSession(&db.SessionRow{ID: "s-cc", ConvID: "conv-cc", Status: "running"}))
	assert.Equal(t, harness.DefaultName, harnessForConv("conv-cc").Name, "claude-tagged → claude")

	require.NoError(t, db.SaveSession(&db.SessionRow{ID: "s-unk", ConvID: "conv-unk", Status: "running", Harness: "no-such-harness"}))
	assert.Equal(t, harness.DefaultName, harnessForConv("conv-unk").Name, "unknown tag → default claude")

	assert.Equal(t, harness.DefaultName, harnessForConv("conv-no-row").Name, "no session row → default claude")
}
