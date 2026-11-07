package agentd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An archive exported before the drawing-board rename names a drawing board
// as resource=board:<id>; importing it keeps the grant, on the new kind.
func TestImportedScopeMovesAPreRenameBoardMatcherToDrawingBoard(t *testing.T) {
	got, err := canonicalImportedPermissionScopeForSlug(PermResourcesRead, `{"resource":["board:plan","process_template:release"]}`)
	require.NoError(t, err)
	assert.Contains(t, got, `"drawing_board:plan"`)
	assert.Contains(t, got, `"process_template:release"`)
	assert.NotContains(t, got, `"board:plan"`)
}
