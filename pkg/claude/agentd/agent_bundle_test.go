package agentd

import (
	"github.com/stretchr/testify/assert"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"testing"
)

func TestImportedHistoryResumeUsesReservedLabelAndTrust(t *testing.T) {
	args := sessionResumeArgs(clcommon.SpawnArgs{ConvID: "source", Label: "reserved", TrustDir: true})
	assert.Contains(t, args, "--label")
	assert.Contains(t, args, "reserved")
	assert.Contains(t, args, "--trust-dir")
	args = sessionResumeArgs(clcommon.SpawnArgs{ConvID: "source"})
	assert.NotContains(t, args, "--label")
	assert.NotContains(t, args, "--trust-dir")
}
