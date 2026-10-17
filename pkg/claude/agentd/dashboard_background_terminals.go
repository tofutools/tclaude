package agentd

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// backgroundTerminal is one tclaude tmux session that is still running with no
// client attached: nothing — no web terminal, no native window — is looking at
// it. The Terminals tab lists these so idle agents, sessions and shells are not
// left running in the background by accident.
//
// Key is the dashboard terminal-pane key the matching attach opens under (see
// terminals-tab.js), so the page can drop a row the moment its pane opens,
// before the next listing sees the new tmux client.
type backgroundTerminal struct {
	// Kind is agent, session (a plain conversation), shell (an agent's web
	// shell), group_shell, or other (an unrecognised session on the socket,
	// listed for the count but not attachable from the dashboard).
	Kind        string `json:"kind"`
	TmuxSession string `json:"tmux_session"`
	Key         string `json:"key,omitempty"`
	Label       string `json:"label"`
	AgentID     string `json:"agent_id,omitempty"`
	ConvID      string `json:"conv_id,omitempty"`
	Harness     string `json:"harness,omitempty"`
	Which       string `json:"which,omitempty"`
	Group       string `json:"group,omitempty"`
	// Superseded marks a session or shell left over from an agent generation
	// that has since been replaced. The attach endpoints always follow an
	// agent to its latest generation, so it has no Key: the dashboard cannot
	// reattach it, only point at `tmux attach`.
	Superseded bool `json:"superseded,omitempty"`
	// Killable marks a row the dashboard may end outright with
	// POST /api/background-terminals/kill: shells, group shells, unrecognised
	// sessions and superseded generations. A live agent or session is not —
	// it has the lifecycle shutdown, which lets the harness exit cleanly.
	Killable     bool  `json:"killable,omitempty"`
	LastActivity int64 `json:"last_activity,omitempty"`
	Created      int64 `json:"created,omitempty"`
}

type backgroundTerminalsResponse struct {
	Terminals []backgroundTerminal `json:"terminals"`
}

// backgroundTmuxSession is one parsed list-sessions line.
type backgroundTmuxSession struct {
	name     string
	activity int64
	created  int64
	// cwd is the active pane's working directory, "" when tmux cannot tell.
	cwd string
}

const backgroundTmuxFormat = "#{session_name}\t#{session_attached}\t#{session_activity}\t#{session_created}\t#{pane_current_path}"

// handleDashboardBackgroundTerminals lists the unattached tclaude tmux sessions.
//
//	GET /api/background-terminals
func handleDashboardBackgroundTerminals(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	sessions, err := listBackgroundTmuxSessions()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "tmux", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, backgroundTerminalsResponse{Terminals: classifyBackgroundTerminals(sessions)})
}

// listBackgroundTmuxSessions returns the sessions no client is attached to. A
// missing tmux server is the normal nothing-running state.
func listBackgroundTmuxSessions() ([]backgroundTmuxSession, error) {
	out, err := tmuxOutputWithTimeout("list-sessions", "-F", backgroundTmuxFormat)
	if err != nil {
		if tmuxServerNotRunning(err) {
			return nil, nil
		}
		return nil, err
	}
	var sessions []backgroundTmuxSession
	for line := range strings.SplitSeq(string(out), "\n") {
		fields := strings.Split(strings.TrimRight(line, "\r"), "\t")
		name := strings.TrimSpace(fields[0])
		// A restart bridge only holds a client across an agent restart and
		// removes itself, and a __tclaude- session is tclaude's own plumbing
		// (OpenCode's managed `opencode serve`) that no client ever attaches
		// to. Neither is something the operator left behind.
		if name == "" || len(fields) < 2 || strings.HasPrefix(name, "restart-") || strings.HasPrefix(name, "__tclaude-") {
			continue
		}
		attached, err := strconv.Atoi(strings.TrimSpace(fields[1]))
		if err != nil || attached > 0 {
			continue
		}
		s := backgroundTmuxSession{name: name}
		if len(fields) > 2 {
			s.activity, _ = strconv.ParseInt(strings.TrimSpace(fields[2]), 10, 64)
		}
		if len(fields) > 3 {
			s.created, _ = strconv.ParseInt(strings.TrimSpace(fields[3]), 10, 64)
		}
		if len(fields) > 4 {
			s.cwd = fields[4]
		}
		sessions = append(sessions, s)
	}
	return sessions, nil
}

// classifyBackgroundTerminals maps tmux session names back to what opened them:
// a sessions-table row (agent or plain conversation), or one of the dashboard's
// hashed shell names (termSessionNameForGroup / groupTermSessionName).
func classifyBackgroundTerminals(sessions []backgroundTmuxSession) []backgroundTerminal {
	out := make([]backgroundTerminal, 0, len(sessions))
	var shells, groupShells, others []backgroundTmuxSession
	for _, s := range sessions {
		switch {
		case strings.HasPrefix(s.name, "tclaude-groupterm-"):
			groupShells = append(groupShells, s)
		case strings.HasPrefix(s.name, "tclaude-term-"):
			shells = append(shells, s)
		default:
			others = append(others, s)
		}
	}
	// Killable fails closed: when an ownership lookup errors, a live agent's
	// session would read as unrecognised, so nothing in that batch is offered
	// for a kill except what is positively known to be safe to end.
	if len(others) > 0 {
		names := make([]string, len(others))
		for i, s := range others {
			names[i] = s.name
		}
		rows, err := db.FindLatestSessionsByTmux(names)
		for _, s := range others {
			t := classifySessionRow(s, rows[s.name])
			t.Killable = t.Superseded || (err == nil && t.Kind == "other")
			out = append(out, t)
		}
	}
	if len(shells)+len(groupShells) > 0 {
		convs, convErr := db.ListAgentConvIDs()
		groupNames, groupErr := listAgentGroupNames()
		known := convErr == nil && groupErr == nil
		for _, t := range classifyShells(shells, groupShells, convs, groupNames) {
			t.Killable = t.Kind != "other" || known
			out = append(out, t)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if a, b := backgroundKindRank(out[i].Kind), backgroundKindRank(out[j].Kind); a != b {
			return a < b
		}
		// Longest idle first: the forgotten ones are what this list is for.
		return out[i].LastActivity < out[j].LastActivity
	})
	return out
}

func backgroundKindRank(kind string) int {
	switch kind {
	case "agent":
		return 0
	case "session":
		return 1
	case "shell":
		return 2
	case "group_shell":
		return 3
	}
	return 4
}

func classifySessionRow(s backgroundTmuxSession, row *db.SessionRow) backgroundTerminal {
	t := backgroundTerminal{Kind: "other", TmuxSession: s.name, Label: s.name, LastActivity: s.activity, Created: s.created}
	if row == nil || row.ConvID == "" {
		return t
	}
	t.ConvID = row.ConvID
	t.Harness = row.Harness
	t.Label = backgroundConvTitle(row.ConvID)
	agentID, superseded := agentGenerationForConv(row.ConvID)
	switch {
	case superseded:
		t.Kind = "agent"
		t.Superseded = true
	case agentID != "":
		t.Kind = "agent"
		t.AgentID = agentID
		t.Key = "window:" + agentID
	default:
		t.Kind = "session"
		t.Key = "window:" + row.ConvID
	}
	return t
}

func listAgentGroupNames() ([]string, error) {
	groups, err := db.ListAgentGroups()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(groups))
	for _, g := range groups {
		names = append(names, g.Name)
	}
	return names, nil
}

// classifyShells reverses the hashed shell names. The hash is one-way, so the
// candidates (every agent conversation in convs, crossed with every group) are
// hashed and matched; that is a few thousand sha256 sums at most, run only
// when a shell is actually sitting in the background.
func classifyShells(shells, groupShells []backgroundTmuxSession, convs map[string]struct{}, groupNames []string) []backgroundTerminal {
	out := make([]backgroundTerminal, 0, len(shells)+len(groupShells))

	type shellIdentity struct{ conv, group string }
	shellOwners := map[string]shellIdentity{}
	if len(shells) > 0 {
		for conv := range convs {
			shellOwners[shellHashPrefix(conv)] = shellIdentity{conv: conv}
			for _, group := range groupNames {
				shellOwners[shellHashPrefix(conv+"\x00"+group)] = shellIdentity{conv: conv, group: group}
			}
		}
	}
	for _, s := range shells {
		t := backgroundTerminal{Kind: "other", TmuxSession: s.name, Label: s.name, LastActivity: s.activity, Created: s.created}
		rest := strings.TrimPrefix(s.name, "tclaude-term-")
		hash, which, ok := strings.Cut(rest, "-")
		if owner, found := shellOwners[hash]; ok && found {
			if which, valid := normaliseWhich(which); valid {
				t.Kind = "shell"
				t.ConvID = owner.conv
				t.Which = which
				t.Group = owner.group
				t.Label = backgroundConvTitle(owner.conv)
				agentID, superseded := agentGenerationForConv(owner.conv)
				t.Superseded = superseded
				if agentID != "" {
					t.AgentID = agentID
					t.Key = fmt.Sprintf("term:%s:%s:%s", agentID, which, owner.group)
				}
			}
		}
		out = append(out, t)
	}

	groupOwners := map[string]string{}
	for _, group := range groupNames {
		groupOwners[groupTermSessionName(group)] = group
	}
	for _, s := range groupShells {
		t := backgroundTerminal{Kind: "other", TmuxSession: s.name, Label: s.name, LastActivity: s.activity, Created: s.created}
		if group, ok := groupOwners[s.name]; ok {
			t.Kind = "group_shell"
			t.Group = group
			t.Label = group
			t.Key = "groupterm:" + group
		}
		out = append(out, t)
	}
	return out
}

// shellHashPrefix is the hash part of termSessionName/termSessionNameForGroup.
func shellHashPrefix(identity string) string {
	sum := sha256.Sum256([]byte(identity))
	return fmt.Sprintf("%x", sum[:8])
}

// agentGenerationForConv returns the agent whose CURRENT generation convID
// is, or superseded=true when convID belongs to an agent but has since been
// replaced. Every attach endpoint resolves a selector through the succession
// chain to the latest generation, so a superseded session cannot be addressed
// from the dashboard by either its agent id or its own conversation id.
func agentGenerationForConv(convID string) (agentID string, superseded bool) {
	a, err := db.GetAgentByConv(convID)
	if err != nil || a == nil {
		return "", false
	}
	if a.CurrentConvID != convID {
		return "", true
	}
	return a.AgentID, false
}

func backgroundConvTitle(convID string) string {
	title := agent.CachedTitle(convID)
	if title == "" || title == agent.UnknownTitle {
		return short8(convID)
	}
	return title
}

// handleDashboardBackgroundTerminalKill ends one background terminal.
//
//	POST /api/background-terminals/kill {"tmux_session": "<name>"}
//
// The name must be one the listing would return right now and mark Killable:
// a session a client attached to meanwhile is no longer in the background,
// and a live agent or session goes through the lifecycle shutdown instead.
func handleDashboardBackgroundTerminalKill(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		TmuxSession string `json:"tmux_session"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.TmuxSession) == "" {
		writeError(w, http.StatusBadRequest, "body", `expected {"tmux_session": "<name>"}`)
		return
	}
	sessions, err := listBackgroundTmuxSessions()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "tmux", err.Error())
		return
	}
	var row *backgroundTerminal
	for _, t := range classifyBackgroundTerminals(sessions) {
		if t.TmuxSession == body.TmuxSession {
			row = &t
			break
		}
	}
	switch {
	case row == nil:
		writeError(w, http.StatusNotFound, "not_background", "no background terminal "+body.TmuxSession+" (attached or already gone)")
		return
	case !row.Killable:
		writeError(w, http.StatusConflict, "not_killable", "a running "+row.Kind+" is stopped with shutdown, not killed here")
		return
	}
	if _, err := tmuxOutputWithTimeout("kill-session", "-t", clcommon.ExactTarget(row.TmuxSession)); err != nil {
		writeError(w, http.StatusInternalServerError, "tmux", "kill-session: "+err.Error())
		return
	}
	slog.Info("background terminal killed from dashboard", "tmux_session", row.TmuxSession, "kind", row.Kind)
	writeJSON(w, http.StatusOK, map[string]string{"tmux_session": row.TmuxSession})
}

// reapOrphanedTerminalShells kills dashboard web shells (tclaude-term-* and
// tclaude-groupterm-*) that nothing can use any more, on each reaper tick:
//
//   - the owner is gone: the agent was deleted (or the group removed), so the
//     shell's hashed name no longer matches anything and the dashboard can
//     neither label nor reattach it;
//   - its directory is gone: the worktree it was opened in has been removed,
//     so the shell sits in a deleted directory.
//
// These shells are never in the sessions table, so no other cleanup reaches
// them. Only unattached shells are touched — one somebody is looking at stays
// until they close it — and the owner rule is skipped whenever the agent or
// group list cannot be read, so a DB hiccup never reads as "every owner gone".
func reapOrphanedTerminalShells() {
	sessions, err := listBackgroundTmuxSessions()
	if err != nil || len(sessions) == 0 {
		return
	}
	var shells, groupShells []backgroundTmuxSession
	for _, s := range sessions {
		switch {
		case strings.HasPrefix(s.name, "tclaude-groupterm-"):
			groupShells = append(groupShells, s)
		case strings.HasPrefix(s.name, "tclaude-term-"):
			shells = append(shells, s)
		}
	}
	if len(shells)+len(groupShells) == 0 {
		return
	}
	convs, convErr := db.ListAgentConvIDs()
	groupNames, groupErr := listAgentGroupNames()
	ownersKnown := convErr == nil && groupErr == nil
	cwds := make(map[string]string, len(shells)+len(groupShells))
	for _, s := range append(shells, groupShells...) {
		cwds[s.name] = s.cwd
	}
	for _, t := range classifyShells(shells, groupShells, convs, groupNames) {
		reason := ""
		switch {
		case ownersKnown && t.Kind == "other":
			reason = "owner gone"
		case directoryGone(cwds[t.TmuxSession]):
			reason = "directory gone"
		default:
			continue
		}
		if _, err := tmuxOutputWithTimeout("kill-session", "-t", clcommon.ExactTarget(t.TmuxSession)); err != nil {
			slog.Warn("reaper: kill orphaned web shell failed", "tmux_session", t.TmuxSession, "error", err)
			continue
		}
		slog.Info("reaper: killed orphaned web shell", "tmux_session", t.TmuxSession, "reason", reason, "cwd", cwds[t.TmuxSession])
	}
}

func directoryGone(dir string) bool {
	if dir == "" {
		return false
	}
	_, err := os.Stat(dir)
	return errors.Is(err, os.ErrNotExist)
}
