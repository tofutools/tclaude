package agentd

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/agent"
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
	Kind         string `json:"kind"`
	TmuxSession  string `json:"tmux_session"`
	Key          string `json:"key,omitempty"`
	Label        string `json:"label"`
	AgentID      string `json:"agent_id,omitempty"`
	ConvID       string `json:"conv_id,omitempty"`
	Harness      string `json:"harness,omitempty"`
	Which        string `json:"which,omitempty"`
	Group        string `json:"group,omitempty"`
	LastActivity int64  `json:"last_activity,omitempty"`
	Created      int64  `json:"created,omitempty"`
}

type backgroundTerminalsResponse struct {
	Terminals []backgroundTerminal `json:"terminals"`
}

// backgroundTmuxSession is one parsed list-sessions line.
type backgroundTmuxSession struct {
	name     string
	attached int
	activity int64
	created  int64
}

const backgroundTmuxFormat = "#{session_name}\t#{session_attached}\t#{session_activity}\t#{session_created}"

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
		// removes itself; it is never something the operator left behind.
		if name == "" || len(fields) < 2 || strings.HasPrefix(name, "restart-") {
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
		sessions = append(sessions, s)
	}
	return sessions, nil
}

// classifyBackgroundTerminals maps tmux session names back to what opened them:
// a sessions-table row (agent or plain conversation), or one of the dashboard's
// hashed shell names (termSessionNameForGroup / groupTermSessionName).
func classifyBackgroundTerminals(sessions []backgroundTmuxSession) []backgroundTerminal {
	out := make([]backgroundTerminal, 0, len(sessions))
	var shells, groupShells []backgroundTmuxSession
	for _, s := range sessions {
		switch {
		case strings.HasPrefix(s.name, "tclaude-groupterm-"):
			groupShells = append(groupShells, s)
		case strings.HasPrefix(s.name, "tclaude-term-"):
			shells = append(shells, s)
		default:
			out = append(out, classifySessionRow(s))
		}
	}
	if len(shells)+len(groupShells) > 0 {
		out = append(out, classifyShells(shells, groupShells)...)
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

func classifySessionRow(s backgroundTmuxSession) backgroundTerminal {
	t := backgroundTerminal{Kind: "other", TmuxSession: s.name, Label: s.name, LastActivity: s.activity, Created: s.created}
	row, err := db.FindLatestSessionByTmux(s.name)
	if err != nil || row == nil || row.ConvID == "" {
		return t
	}
	t.ConvID = row.ConvID
	t.Harness = row.Harness
	t.Label = backgroundConvTitle(row.ConvID)
	t.Kind = "session"
	if agentID, _ := db.AgentIDForConv(row.ConvID); agentID != "" {
		t.Kind = "agent"
		t.AgentID = agentID
	}
	t.Key = "window:" + backgroundSelector(t.AgentID, t.ConvID)
	return t
}

// classifyShells reverses the hashed shell names. The hash is one-way, so the
// candidates (every agent conversation, crossed with every group) are hashed
// and matched; that is a few thousand sha256 sums at most, run only when a
// shell is actually sitting in the background.
func classifyShells(shells, groupShells []backgroundTmuxSession) []backgroundTerminal {
	var groupNames []string
	if groups, err := db.ListAgentGroups(); err == nil {
		for _, g := range groups {
			groupNames = append(groupNames, g.Name)
		}
	}
	out := make([]backgroundTerminal, 0, len(shells)+len(groupShells))

	type shellIdentity struct{ conv, group string }
	shellOwners := map[string]shellIdentity{}
	if len(shells) > 0 {
		convs, _ := db.ListAgentConvIDs()
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
				t.AgentID, _ = db.AgentIDForConv(owner.conv)
				t.Which = which
				t.Group = owner.group
				t.Label = backgroundConvTitle(owner.conv)
				t.Key = fmt.Sprintf("term:%s:%s:%s", backgroundSelector(t.AgentID, t.ConvID), which, owner.group)
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

// backgroundSelector mirrors the row actions: the stable agent id when there
// is one, the conversation id otherwise.
func backgroundSelector(agentID, convID string) string {
	if agentID != "" {
		return agentID
	}
	return convID
}

func backgroundConvTitle(convID string) string {
	title := agent.CachedTitle(convID)
	if title == "" || title == agent.UnknownTitle {
		return short8(convID)
	}
	return title
}
