package harness

import (
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// MarkConvCostInherited records that convID's transcript holds history copied
// from another conversation, whose usage stays that conversation's spend.
//
// Codex, Gemini and OpenCode cost is summed over every usage record in the
// transcript, and the copy keeps those records with their original
// timestamps. tclaude's cost ledger baselines each conversation at zero, so
// without the boundary a clone or imported bundle would report its source's
// whole spend again under the new conversation id. Their cost readers skip
// usage recorded at or before the boundary (see db.ConvCostInheritedUntil).
//
// Call it once the copy has fully landed and before the new conversation
// runs: every copied record then predates the boundary, while the new
// conversation's own usage follows it. A copy that crossed nodes compares the
// source's record times with this node's clock, so only clock skew larger
// than the transfer itself could misplace a record.
func MarkConvCostInherited(convID string) error {
	return db.SetConvCostInheritedUntil(convID, time.Now())
}
