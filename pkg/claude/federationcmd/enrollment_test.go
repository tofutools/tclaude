package federationcmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func TestEnrollmentBearerSourcesAndCLI(t *testing.T) {
	_, e := readEnrollmentBearer(&enrollmentParams{})
	require.Error(t, e)
	_, e = readEnrollmentBearer(&enrollmentParams{Token: "secret", TokenStdin: true})
	require.Error(t, e)
	_, e = readEnrollmentBearer(&enrollmentParams{Token: strings.Repeat("x", 9000)})
	require.Error(t, e)
	path := filepath.Join(testutil.CanonicalTempDir(t), "token")
	require.NoError(t, os.WriteFile(path, []byte(" token\n"), 0600))
	s, e := readEnrollmentBearer(&enrollmentParams{TokenFile: path})
	require.NoError(t, e)
	require.Equal(t, "token", s)
	require.NotNil(t, enrollCmd().Flags().Lookup("token-stdin"))
	require.NotNil(t, enrollCmd().Flags().Lookup("preview"))
	cmd, _, e := enrollTokenCmd().Find([]string{"create"})
	require.NoError(t, e)
	require.NotNil(t, cmd.Flags().Lookup("uses"))
	require.NotNil(t, cmd.Flags().Lookup("trust-level"))
}
