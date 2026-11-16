package agentd

import (
	"log/slog"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// The Claude Code Remote Access session link (https://claude.ai/code/…) of an
// agent's current launch. The dashboard offers it as "Open in Claude app ↗":
// on a phone with the Claude app, the universal link opens the same session
// there.
//
// Claude Code writes a bridge_status record carrying the link into its
// transcript each time Remote Access comes up. The transcript follower
// (convops) reads it and records it on the launch's session row
// (db.RecordSessionRCLink), so it is known without reading the pane and
// survives a daemon restart (tk_5ef87574). Switching Remote Access either
// way clears it (db.ClearSessionRCLink): on starts a new session, whose new
// record the follower picks up. Nothing records Remote Access going down, so
// a /remote-control off typed in the pane leaves the link until the next
// launch (an accepted trade-off): the tracked flag still gates it.
//
// The link gives control of the session (for whoever is signed in to that
// Claude account), so it is treated like an interactive terminal: never
// logged, in the local snapshot for the operator, and in a peer's projection
// only for agents the peer may attach to (peer_snapshot.go).

// rcSessionURLFor returns the link for a live session row whose Remote Access
// is (best known) on, recorded from that same launch; else "".
func rcSessionURLFor(sess *db.SessionRow, alive bool) string {
	if sess == nil || !alive || !sess.RemoteControl {
		return ""
	}
	return sess.RCSessionURL
}

// clearRCSessionURL drops sess's link before Remote Access is switched.
func clearRCSessionURL(sess *db.SessionRow) {
	if sess == nil || sess.ID == "" {
		return
	}
	if err := db.ClearSessionRCLink(sess.ID); err != nil {
		slog.Warn("remote-control: clearing the session link failed", "conv_id", sess.ConvID, "session_id", sess.ID, "error", err)
	}
	sess.RCSessionURL = ""
}
