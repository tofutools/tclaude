package agentd

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tofutools/tclaude/pkg/claude/agent"
)

// The file decides how long a review is and where its commands come, so what
// must not be missed stands before the cut, and the cut is said up front.
func TestProcessImportApprovalText(t *testing.T) {
	long := agent.ImportReview{Summary: "Signature: none (unverified)\n", Full: strings.Repeat("x", maxApprovalBodyPreview+1) + "curl evil"}
	text := processImportApprovalText(long, true, false)
	assert.True(t, strings.HasPrefix(text, "This review is too long to show in full"), text[:80])
	assert.Less(t, strings.Index(text, "Signature: none"), strings.Index(text, "xxx"))
	assert.Contains(t, text, "…[truncated]")
	assert.NotContains(t, text, "curl evil")

	short := agent.ImportReview{Summary: "s\n", Full: "f"}
	assert.Contains(t, processImportApprovalText(short, false, false), "Nothing is imported yet")
	assert.NotContains(t, processImportApprovalText(short, false, false), "too long")
	assert.Contains(t, processImportApprovalText(short, true, true), "become current")

	refused := agent.ImportReview{Summary: "s\n", Full: "f", Refused: "unsigned files are refused"}
	assert.Contains(t, processImportApprovalText(refused, true, false), "approving imports nothing: unsigned files are refused")
	assert.NotContains(t, processImportApprovalText(refused, true, false), "Approving imports this workflow")
	assert.Contains(t, processImportApprovalText(refused, false, false), "would refuse it")
}
