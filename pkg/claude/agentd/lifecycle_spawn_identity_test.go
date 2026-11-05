package agentd

import (
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tclcommon "github.com/tofutools/tclaude/pkg/common"
)

// A launch outside tclaude's sandbox keeps a label the deferred continuation
// reserved, mints one otherwise, and leaves the held cleanup in place.
func TestReserveSpawnLabelOutsideTheSandboxCreatesNoRoot(t *testing.T) {
	resetTestDB(t)
	held := 0
	keep := func() { held++ }

	label, cleanup, fail := reserveSpawnLabel(&spawnParams{pendingSpawnLabel: "spwn-kept"}, false, keep)
	require.Nil(t, fail)
	assert.Equal(t, "spwn-kept", label)
	cleanup()
	assert.Equal(t, 1, held, "the held cleanup is returned unchanged")

	label, _, fail = reserveSpawnLabel(&spawnParams{}, false, keep)
	require.Nil(t, fail)
	assert.True(t, strings.HasPrefix(label, "spwn-"), "minted label %q", label)
}

// Behind tclaude's sandbox a fresh label reserves a private attachment root,
// and the returned cleanup removes it again.
func TestReserveSpawnLabelBehindTheSandboxReservesItsRoot(t *testing.T) {
	resetTestDB(t)
	label, cleanup, fail := reserveSpawnLabel(&spawnParams{}, true, func() {})
	require.Nil(t, fail)
	root := tclcommon.SpawnAttachmentsPrivateDir(label)
	_, err := os.Stat(root)
	require.NoError(t, err, "the private attachment root must exist")
	cleanup()
	_, err = os.Stat(root)
	assert.True(t, os.IsNotExist(err), "the returned cleanup must remove the root")
}

// A reserved label whose root another spawn generation already owns is
// refused, and the caller keeps the cleanup it held: the other generation's
// root is not removed.
func TestReserveSpawnLabelRefusesAnotherGenerationsRoot(t *testing.T) {
	resetTestDB(t)
	_, _, err := tclcommon.PrepareSpawnAttachmentsPrivateDir("spwn-owned")
	require.NoError(t, err)
	held := 0
	_, cleanup, fail := reserveSpawnLabel(&spawnParams{pendingSpawnLabel: "spwn-owned"}, true, func() { held++ })
	require.NotNil(t, fail)
	assert.Equal(t, http.StatusConflict, fail.Status)
	assert.Equal(t, "spawn_label_collision", fail.Kind)
	cleanup()
	assert.Equal(t, 1, held)
	_, err = os.Stat(tclcommon.SpawnAttachmentsPrivateDir("spwn-owned"))
	assert.NoError(t, err, "another generation's root survives the refusal")
}

// A Codex app-server launch gets its agent id up front.
func TestPrepareSpawnStateRootMintsAnAppServerAgentID(t *testing.T) {
	resetTestDB(t)
	p := spawnParams{CodexAppServer: true}
	require.Nil(t, prepareSpawnStateRoot(&p))
	assert.NotEmpty(t, p.AgentID)
}

// A promotion that fails after the root was reserved hands back the root's
// cleanup, so the deferred cleanup still removes the root this spawn created.
func TestReserveSpawnLabelRemovesItsRootWhenPromotionFails(t *testing.T) {
	resetTestDB(t) // no daemon staging root exists, so promotion fails
	label, cleanup, fail := reserveSpawnLabel(&spawnParams{Attachments: []string{"upload.txt"}}, true, func() {})
	require.NotNil(t, fail)
	assert.Equal(t, http.StatusBadRequest, fail.Status)
	assert.Equal(t, "invalid_attachments", fail.Kind)
	root := tclcommon.SpawnAttachmentsPrivateDir(label)
	_, err := os.Stat(root)
	require.NoError(t, err, "the reserved root exists until the deferred cleanup runs")
	cleanup()
	_, err = os.Stat(root)
	assert.True(t, os.IsNotExist(err), "the handed-back cleanup must remove the reserved root")
}
