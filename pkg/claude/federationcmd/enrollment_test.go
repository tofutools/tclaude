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
	require.NotNil(t, cmd.Flags().Lookup("request-trust"))
	require.Nil(t, enrollCmd().Flags().Lookup("give-permissions"), "plain enroll gives nothing")
	require.Nil(t, enrollCmd().Flags().Lookup("yes-unrestricted"))
	for _, flag := range []string{"give-permissions", "yes-unrestricted", "confirm-fingerprint", "token-file", "preview"} {
		require.NotNil(t, becomeNodeCmd().Flags().Lookup(flag), flag)
	}
	require.Equal(t, []map[string]string{{"slug": "host.read", "scope": ""}, {"slug": "groups.members.spawn", "scope": "group=builders"}, {"slug": "agents.status.read", "scope": ""}},
		becomeNodeGives([]string{"host.read, groups.members.spawn@group=builders", "agents.status.read"}))
}
