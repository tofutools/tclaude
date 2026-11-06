package agentd

import (
	"errors"
	"log/slog"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// launchTextSubject heads the inbox copy of launch text that could not be
// handed over the drive at launch.
const launchTextSubject = "Launch guidance"

var errNoLaunchDrive = errors.New("no drive handle for this launch")

// deliverLaunchTextOverDrive delivers launch text (a first turn, the managed
// standing guidance) to an API-driven launch over the channel its harness is
// configured for, never by keystrokes and never as a launch argument the
// harness ignores.
//
// OpenCode takes no launch prompt: its pane is only an attach client of the
// agentd-managed server, so the text goes through that server's prompt API,
// the same call spawn makes after its fork. When that send fails the text is
// not dropped: it is queued as an inbox row to the agent, and the nudge
// pipeline delivers it over the same drive once the channel answers (it holds
// rather than falling back to the pane). A send whose acceptance is uncertain
// is queued too; delivery is at-least-once, as nudges are.
//
// launch is the drive handle the launch started; nil goes straight to the
// inbox. It returns the id of the inbox row it queued, 0 when the drive took
// the text (or nothing was queued), so a launch that is then rolled back can
// remove the row.
func deliverLaunchTextOverDrive(convID, text string, launch *openCodeLaunch, cwd, model, effort string) int64 {
	if text == "" {
		return 0
	}
	err := errNoLaunchDrive
	if launch != nil {
		err = sendOpenCodePromptForSpawn(launch, cwd, text, model, effort)
	}
	if err == nil {
		return 0
	}
	slog.Warn("launch text: drive delivery failed; queueing it in the agent's inbox",
		"conv", convID, "error", err)
	return queueLaunchTextInInbox(convID, text)
}

// queueLaunchTextInInbox stores launch text as an unread inbox row from the
// system (no sender), routed through the agent's first group like other
// system notes, and wakes its delivery.
func queueLaunchTextInInbox(convID, text string) int64 {
	var groupID int64
	if groups, err := db.ListGroupsForConv(convID); err == nil && len(groups) > 0 {
		groupID = groups[0].ID
	}
	id, err := queueAgentMessage(&db.AgentMessage{
		GroupID: groupID, ToConv: convID, Subject: launchTextSubject, Body: text,
	})
	if err != nil {
		slog.Error("launch text: queueing the undelivered launch text failed; it is lost",
			"conv", convID, "error", err)
		return 0
	}
	return id
}
