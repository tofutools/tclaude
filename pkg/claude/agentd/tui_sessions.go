package agentd

// Non-agent sessions in the terminal console.
//
// The console's subject is agents, and agents come off the daemon's own /v1
// API (see tui.go). A plain tmux session — one the console's own shell form
// started, or a `tclaude session new` from the operator's shell — is not an
// agent: no conversation the agent API describes, no group, no permissions,
// and so no /v1 shape to read it through. It is host-local state, exactly like
// the two moves tui.go already keeps off the API (handing this terminal to a
// pane, starting a shell session), and it is read and acted on here the same
// way and behind the same gates: an operator console that shares the daemon's
// host. A console the daemon classifies as an agent sees none of it — the
// directories alone are the operator's own, outside any sandbox, the same
// reasoning that gates Tab-completion.

import (
	"fmt"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// tuiSessionGroupCell is what a non-agent session shows where an agent shows
// its groups. It is the row's "this is not an agent" marker: a session has no
// group by construction, so the column is free, and the operator reads the
// distinction on the row itself rather than having to infer it from a blank.
const tuiSessionGroupCell = "(session)"

// tuiSessionRow is one live non-agent session on the daemon's host. Only the
// fields the listing renders — and the tmux handle every action needs — are
// carried; a session has no conversation, model, cost or context state for the
// console to show.
type tuiSessionRow struct {
	// SessionID is the sessions-table primary key: what identifies the row
	// across a refresh, and what `tclaude session ls` calls the session.
	SessionID   string
	TmuxSession string
	Cwd         string
	Harness     string
	Status      string
}

// name is the display label: the tmux handle, which is both what `tmux ls`
// shows and what `tclaude session attach <handle>` takes. A row without one
// falls back to its id, which findSession also resolves.
func (s tuiSessionRow) name() string {
	if h := strings.TrimSpace(s.TmuxSession); h != "" {
		return h
	}
	return s.SessionID
}

// status is the session's recorded activity. Unlike an agent row there is no
// offline case to override it — only live sessions are listed — but the value
// is shown verbatim rather than assumed: a harness that has ended inside a
// pane the operator has not closed yet really is "exited", and saying
// "running" there would be a guess.
func (s tuiSessionRow) status() string {
	if st := strings.TrimSpace(s.Status); st != "" {
		return st
	}
	return "running"
}

// tuiListLocalSessions is the console's live non-agent session listing,
// indirected through a package var so tests can drive the model without a
// session database or a tmux server.
var tuiListLocalSessions = listLocalNonAgentSessions

// listLocalNonAgentSessions returns the daemon host's live sessions that are
// not an agent's, ordered by the handle they are attached by.
//
// Live only, on purpose: an exited session has no pane to switch to and no
// resume verb behind it (a session is not a conversation), so listing one
// would add a row whose only possible answer to enter is "there is nothing
// there". `tclaude session ls -a` is where the exited ones live.
func listLocalNonAgentSessions() ([]tuiSessionRow, error) {
	states, err := session.ListSessionStates()
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	// Every conversation that belongs to an agent generation — current,
	// retired, or superseded by a reincarnate. A session keyed to one of those
	// is an agent's session and the agent listing owns it; re-listing it here
	// would show one pane twice under two identities.
	agentConvs, err := db.ListAgentConvIDs()
	if err != nil {
		return nil, fmt.Errorf("read agent conversations: %w", err)
	}
	// A spawn whose conv-id has not materialised yet has no agent conversation
	// to match on, so without this it would flash through the listing as a
	// plain session for the second or two before enrollment lands. Its pending
	// row is keyed by the spawn label, which IS the session id. A failed read
	// costs only that suppression, so it does not fail the listing.
	pending := map[string]struct{}{}
	if rows, perr := db.ListPendingSpawns(); perr == nil {
		for _, ps := range rows {
			pending[ps.Label] = struct{}{}
		}
	}
	// One tmux ls for the whole listing, shared with the daemon's other poll
	// consumers through the short-TTL cache. The map is read-only.
	alive, err := cachedLiveTmuxSessions()
	if err != nil {
		return nil, fmt.Errorf("list tmux sessions: %w", err)
	}
	var out []tuiSessionRow
	for _, st := range states {
		if st.TmuxSession == "" {
			continue
		}
		if _, live := alive[st.TmuxSession]; !live {
			continue
		}
		if st.ConvID != "" {
			if _, isAgent := agentConvs[st.ConvID]; isAgent {
				continue
			}
		}
		if _, isPending := pending[st.ID]; isPending {
			continue
		}
		out = append(out, tuiSessionRow{
			SessionID:   st.ID,
			TmuxSession: st.TmuxSession,
			Cwd:         st.Cwd,
			Harness:     st.Harness,
			Status:      st.Status,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].name()) < strings.ToLower(out[j].name())
	})
	return out, nil
}

// tuiKillSession ends one non-agent session, indirected through a package var
// so tests can drive the confirmation without a tmux server.
var tuiKillSession = killLocalSessionPane

// killLocalSessionPane kills a session's tmux session — the console's half of
// `tclaude session kill`.
//
// It kills the pane and stops there. It deliberately does NOT delete the
// session row: the daemon's own reaper notices the dead pane and writes
// "exited", which is exactly what happens when the operator types `exit` in
// that shell, so a session killed from here leaves the same history behind as
// one that ended on its own. Nothing is lost by that either — the listing is
// live sessions only, so the row leaves the console the moment the pane does.
func killLocalSessionPane(tmuxSession string) error {
	if strings.TrimSpace(tmuxSession) == "" {
		return fmt.Errorf("no tmux session to kill")
	}
	out, err := clcommon.TmuxCommand(
		"kill-session", "-t", clcommon.ExactTarget(tmuxSession)).CombinedOutput()
	if err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	return nil
}

func tuiKillSessionCmd(row tuiSessionRow) tea.Cmd {
	return func() tea.Msg {
		return tuiSessionKilledMsg{session: row.name(), err: tuiKillSession(row.TmuxSession)}
	}
}

// ---- listing rows ----------------------------------------------------------

// tuiRowKind is what a listing row IS. The console shows two different things
// in one table, and nearly every key means something different on each, so the
// distinction is carried explicitly rather than inferred from an empty field.
type tuiRowKind int

const (
	tuiRowAgent tuiRowKind = iota
	tuiRowSession
)

// tuiListRow is one row of the console's listing: an agent, or a live
// non-agent session. Exactly one of the two payloads is meaningful, named by
// kind.
type tuiListRow struct {
	kind    tuiRowKind
	agent   tuiAgentRow
	session tuiSessionRow
}

func agentListRow(a tuiAgentRow) tuiListRow {
	return tuiListRow{kind: tuiRowAgent, agent: a}
}

func sessionListRow(s tuiSessionRow) tuiListRow {
	return tuiListRow{kind: tuiRowSession, session: s}
}

func (r tuiListRow) isSession() bool { return r.kind == tuiRowSession }

// key identifies the row across a refresh, so the cursor can follow the thing
// it was on rather than the row number it sat at (see restoreCursor). The two
// kinds are namespaced apart: a session id and a conv-id are drawn from
// different id spaces and must never be mistaken for one another.
func (r tuiListRow) key() string {
	if r.isSession() {
		return "session:" + r.session.SessionID
	}
	if r.agent.ConvID != "" {
		return "agent:" + r.agent.ConvID
	}
	return "agent:" + r.agent.AgentID
}

func (r tuiListRow) name() string {
	if r.isSession() {
		return r.session.name()
	}
	return r.agent.name()
}

func (r tuiListRow) statusCell() string {
	if r.isSession() {
		return r.session.status()
	}
	return r.agent.status()
}

// groupCell is the GROUP column, which is where a row says it is not an agent
// — a session has no group to show there.
func (r tuiListRow) groupCell() string {
	if r.isSession() {
		return tuiSessionGroupCell
	}
	return strings.Join(r.agent.Groups, ",")
}

func (r tuiListRow) harnessCell() string {
	if r.isSession() {
		return r.session.Harness
	}
	return r.agent.State.Harness
}

func (r tuiListRow) dirCell() string {
	if r.isSession() {
		return r.session.Cwd
	}
	return r.agent.dir()
}

// branchCell is empty for a session: the branch shown for an agent is read off
// its conversation's turns, and a session has no conversation.
func (r tuiListRow) branchCell() string {
	if r.isSession() {
		return ""
	}
	return r.agent.Branch
}
