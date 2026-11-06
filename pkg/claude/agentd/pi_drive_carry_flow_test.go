package agentd_test

import (
	"encoding/json"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// The pi drive has no session-new flag, so a clone's launch argv cannot carry
// it and an import's profile cannot say it: each path has to write it on the
// new agent, or the new pi falls back to send-keys.

func piDriveRecorded(t *testing.T, conv string) bool {
	t.Helper()
	profile, err := db.RecordedLaunchPostureForConv(conv)
	require.NoError(t, err)
	return profile != nil && profile.PiDrive != nil && *profile.PiDrive
}

func TestPiDriveSurvivesClone(t *testing.T) {
	for name, body := range map[string]map[string]any{
		"no-copy": {"no_copy_conv": true},
		"copy":    {},
	} {
		t.Run(name, func(t *testing.T) {
			f := piDriveFlow(t)
			f.HaveGroup("crew")
			driven, _ := spawnPi(t, f, "crew", map[string]any{"name": "pi-driven", "pi_drive": true})
			plain, _ := spawnPi(t, f, "crew", map[string]any{"name": "pi-keys"})

			cloned := f.AsHuman().CloneWith(driven.ConvID, body)
			require.NotEmptyf(t, cloned.NewConv, "no clone: %s", cloned.Raw)
			assert.True(t, piDriveRecorded(t, cloned.NewConv), "the clone of a driven pi lost the drive")

			cloned = f.AsHuman().CloneWith(plain.ConvID, body)
			require.NotEmptyf(t, cloned.NewConv, "no clone: %s", cloned.Raw)
			assert.False(t, piDriveRecorded(t, cloned.NewConv), "the clone of a send-keys pi gained the drive")
		})
	}
}

func TestPiDriveSurvivesExportAndImport(t *testing.T) {
	f := piDriveFlow(t)
	f.HaveGroup("crew")
	f.HaveGroup("receiver")
	// An ask posture travels: the receiver launches it without a sandbox.
	driven, _ := spawnPi(t, f, "crew", map[string]any{"name": "pi-driven", "pi_drive": true, "approval": harness.PiApprovalAsk})
	plain, _ := spawnPi(t, f, "crew", map[string]any{"name": "pi-keys", "approval": harness.PiApprovalAsk})

	land := func(conv string) (bool, string) {
		t.Helper()
		rec := profileReq(t, f, "GET", "/v1/agent-bundle/export?agent="+conv, nil)
		require.Equal(t, 200, rec.Code, rec.Body.String())
		b, err := decodeAgentExport(t, rec.Body.Bytes())
		require.NoError(t, err)
		dest := testutil.CanonicalTempDir(t)
		rec = postAgentBundle(t, f, b, "apply=true&group=receiver&name=landed&cwd="+url.QueryEscape(dest), "")
		require.Equal(t, 200, rec.Code, rec.Body.String())
		var result struct {
			Applied bool `json:"applied"`
			Spawn   struct {
				ConvID string `json:"conv_id"`
			} `json:"spawn"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &result))
		require.True(t, result.Applied, rec.Body.String())
		require.NotEmpty(t, result.Spawn.ConvID)
		return b.Manifest.Agent.PiDrive, result.Spawn.ConvID
	}

	carried, landed := land(driven.ConvID)
	assert.True(t, carried, "the export of a driven pi does not carry the drive")
	assert.True(t, piDriveRecorded(t, landed), "the imported pi lost the drive")

	carried, landed = land(plain.ConvID)
	assert.False(t, carried, "the export of a send-keys pi carries a drive")
	assert.False(t, piDriveRecorded(t, landed), "the imported send-keys pi gained the drive")
}
