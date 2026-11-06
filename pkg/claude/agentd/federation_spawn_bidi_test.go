package agentd

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// A spawn request's directory is checked against proto.StripControls, which
// now drops bidi controls: a directory carrying one is refused rather than
// shown to the operator reordered (tcl-7gm2ec).
func TestFedSpawnOverridesRefuseADirectoryWithABidiControl(t *testing.T) {
	require.Error(t, validateFedSpawnOverrides(db.FederationSpawnOverrides{Cwd: "/home/dev/\u202egit"}))
	require.NoError(t, validateFedSpawnOverrides(db.FederationSpawnOverrides{Cwd: "/home/dev/git"}))
}
