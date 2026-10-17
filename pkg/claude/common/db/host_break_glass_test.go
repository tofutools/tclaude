package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/groupexport"
)

func TestHostBreakGlassDatabaseImportDropsForgedGrants(t *testing.T) {
	setupTestDB(t)
	exp := &groupexport.Export{FormatVersion: groupexport.FormatVersion, SourceGroup: "forged", SudoGrants: []groupexport.SudoGrant{{ConvID: "forged-conv", Slug: HostBreakGlassSlug, GrantedBy: HostBreakGlassGrantOrigin, GrantedAt: time.Now().Format(time.RFC3339Nano), ExpiresAt: time.Now().Add(time.Hour).Format(time.RFC3339Nano)}}}
	_, err := ImportGroup(GroupImportPlan{Export: exp, TargetName: "forged", TargetCwd: "/tmp/forged", ConvRemap: map[string]string{}})
	require.NoError(t, err)
	rows, err := ListHostBreakGlassGrants("")
	require.NoError(t, err)
	require.Empty(t, rows)
}
