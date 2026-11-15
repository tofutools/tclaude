package agentd

import (
	"strings"
	"sync"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// The Claude Code Remote Access session link (https://claude.ai/code/…) an
// agent's /rc footer pill carries, as last read from its pane. The dashboard
// offers it as "Open in Claude app ↗": on a phone with the Claude app, the
// universal link opens the same session there.
//
// It is remembered, never polled: reading it forks a tmux capture
// (observeRemoteControl), so only the remote-control status and toggle calls
// read it, and the snapshot shows what they last saw. The dashboard asks for
// a fresh read (intent "status") when a menu that would offer the link opens
// and none is known yet.
//
// The link gives control of the session (for whoever is signed in to that
// Claude account), so it is treated like an interactive terminal: never
// logged, in the local snapshot for the operator, and in a peer's projection
// only for agents the peer may attach to (peer_snapshot.go).

type rcSessionLink struct {
	// session is the session row it was read from. Each launch (spawn,
	// resume, restart) is a new row, whose Claude Code has a new link, while
	// a resume may reuse the tmux session's name.
	session string
	url     string
}

var rcSessionLinks = struct {
	sync.Mutex
	byConv map[string]rcSessionLink
}{byConv: map[string]rcSessionLink{}}

// rememberRCSessionURL records the link a confident "armed" read of sess's
// pane found. Only the exact claude.ai/code origin is kept; an armed read
// with no link keeps what was known for the same session.
func rememberRCSessionURL(sess *db.SessionRow, url string) {
	if sess == nil || sess.ConvID == "" || sess.ID == "" || !strings.HasPrefix(url, rcSessionURLPrefix) {
		return
	}
	rcSessionLinks.Lock()
	defer rcSessionLinks.Unlock()
	rcSessionLinks.byConv[sess.ConvID] = rcSessionLink{session: sess.ID, url: url}
}

// forgetRCSessionURL drops conv's link: its pane read "off", or Remote Access
// was switched (switching it on starts a new session with a new link).
func forgetRCSessionURL(convID string) {
	rcSessionLinks.Lock()
	defer rcSessionLinks.Unlock()
	delete(rcSessionLinks.byConv, convID)
}

// rcSessionURLFor returns the link for a live session row whose Remote Access
// is (best known) on, read from that same session; else "".
func rcSessionURLFor(sess *db.SessionRow, alive bool) string {
	if sess == nil || !alive || !sess.RemoteControl || sess.ID == "" {
		return ""
	}
	rcSessionLinks.Lock()
	defer rcSessionLinks.Unlock()
	link, ok := rcSessionLinks.byConv[sess.ConvID]
	if !ok || link.session != sess.ID {
		return ""
	}
	return link.url
}
