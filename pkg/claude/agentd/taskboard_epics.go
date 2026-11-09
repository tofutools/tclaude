package agentd

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Epics (tk_a970ef38): when the last open child of an epic closes, the board
// marks it close-suggested (the "close epic?" nudge) and its creator is
// mailed once. Nothing closes an epic by itself.

func init() { db.GroupTaskEpicCloseSuggested = mailEpicCloseSuggested }

// mailEpicCloseSuggested mails an epic's creator, once per suggestion, that
// all of its children are closed. An epic the operator created has the
// board's nudge only.
func mailEpicCloseSuggested(epicID string) {
	claimed, err := db.ClaimGroupTaskCloseNudge(epicID)
	if err != nil || !claimed {
		if err != nil {
			slog.Warn("taskboard: epic nudge", "epic", epicID, "error", err)
		}
		return
	}
	epic, err := db.GetGroupTask(epicID)
	if err != nil {
		return
	}
	creator, ok := strings.CutPrefix(epic.CreatedBy, "agent:")
	if !ok {
		return
	}
	conv, _ := db.CurrentConvForAgent(creator)
	g, err := db.GetAgentGroupByID(epic.GroupID)
	if conv == "" || err != nil || g == nil {
		return
	}
	text := fmt.Sprintf("Every task in epic %s %q in %s is closed. Close the epic when its work is done:\n"+
		"  tclaude agent taskboard done %s\n"+
		"or add the work still missing to it (taskboard add --parent %s).\n", epic.ID, epic.Title, g.Name, epic.ID, epic.ID)
	systemMailTaskboard(g, conv, taskboardSubject(g, epic, "close epic?"), taskboardLinked(g, epic, text))
}
