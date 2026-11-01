package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Action links (tcl-99t8tr): a notification can carry one button that names an
// action from a fixed list, never a free URL or command. The agent names the
// action; the daemon works out the target, builds the text on the button, and
// acts for nobody until the operator clicks. So a terminal link can only ever
// be the sender's own tracked directory, and a PR link one the sender presented.
//
// What is stored is the name: "terminal", "board:<id>", "pr:<url>" or
// "tab:<name>". The agent hears nothing back, and never gets the shell's input
// or output.

const (
	humanActionTerminal = "terminal"
	humanActionBoard    = "board"
	humanActionPR       = "pr"
	humanActionTab      = "tab"
)

// The directory an action link's shell opens in: what the agent's row and the
// ⌨ in its terminal show as where it is now.
const humanActionShellWhich = "worktree"

// A directory shell is a plain tmux session the daemon starts, with no sandbox
// wrapper, and the agent can write the files it runs among.
const humanActionShellNote = "Runs as you, outside the agent's sandbox. The agent can write to this directory."

// humanActionTabs are the dashboard tabs an action can go to.
var humanActionTabs = map[string]struct{ label, path string }{
	"groups":      {"Groups", "/"},
	"terminals":   {"Terminals", "/terminals"},
	"automations": {"Automations", "/automations"},
	"processes":   {"Processes", "/automations/processes"},
	"plugins":     {"Plugins", "/plugins"},
	"access":      {"Access", "/access"},
	"messages":    {"Messages", "/messages"},
	"usage":       {"Usage", "/usage"},
	"diagnostics": {"Diagnostics", "/diagnostics"},
	"config":      {"Config", "/config"},
}

// parseHumanAction splits an action's name from its argument.
func parseHumanAction(action string) (kind, arg string) {
	kind, arg, _ = strings.Cut(action, ":")
	return kind, arg
}

// validateHumanAction checks an action a sender names, as that sender. Every
// refusal says why, so the agent can correct it.
func validateHumanAction(callerConv, action string) error {
	agentID, err := db.AgentIDForConv(callerConv)
	if err != nil {
		return err
	}
	if agentID == "" {
		return errors.New("an action needs an enrolled agent as its sender")
	}
	kind, arg := parseHumanAction(action)
	switch kind {
	case humanActionTerminal:
		if arg != "" {
			return errors.New("terminal takes no argument: a terminal link is always your own directory")
		}
		if humanActionDir(callerConv) == "" {
			return errors.New("your directory is not known yet, so there is no terminal to offer")
		}
		return nil
	case humanActionBoard:
		if _, err := db.GetCollabBoard(arg); err != nil {
			return fmt.Errorf("no board %q", arg)
		}
		return nil
	case humanActionPR:
		allowed, err := humanActionPRAllowed(agentID, arg)
		if err != nil {
			return err
		}
		if !allowed {
			return errors.New("a pr action names a PR you presented: run `tclaude agent present-pr <url>` first")
		}
		return nil
	case humanActionTab:
		if _, ok := humanActionTabs[arg]; !ok {
			return fmt.Errorf("no dashboard tab %q", arg)
		}
		return nil
	}
	return errors.New(`action must be "terminal", "board:<id>", "pr:<url>" or "tab:<name>"`)
}

// humanActionPRAllowed reports whether agentID presented prURL. Under the Git
// proxy a PR counts once its repository was validated, as it does on the PR
// badge.
func humanActionPRAllowed(agentID, prURL string) (bool, error) {
	if strings.TrimSpace(prURL) == "" {
		return false, nil
	}
	row, err := db.GetAgentPR(agentID, prURL)
	if err != nil {
		return false, err
	}
	if row.ID == 0 {
		return false, nil
	}
	proxied, err := gitProxyActivated()
	if err != nil {
		return false, err
	}
	return !proxied || row.ValidatedRepoRoot != "", nil
}

// humanActionDir is the directory the shell of conv's agent opens in, the same
// one the terminal route resolves when the shell attaches.
func humanActionDir(convID string) string {
	start, current, worktree, _ := resolveDirs(convID)
	return pickDir(humanActionShellWhich, start, current, worktree)
}

// checkNotifyHumanAction gates an action on a new notification and writes the
// refusal. It reports whether the message may go on.
func checkNotifyHumanAction(w http.ResponseWriter, callerConv, action string, decision bool) bool {
	if action == "" {
		return true
	}
	if !collabRoutesEnabled() {
		writeError(w, http.StatusNotFound, collabDisabledCode, config.CollabDisabledMessage)
		return false
	}
	if callerConv == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "an action names the agent's own things; actions are for agents")
		return false
	}
	if decision {
		writeError(w, http.StatusBadRequest, "invalid_arg", "a message is a decision or an action, not both")
		return false
	}
	if err := validateHumanAction(callerConv, action); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return false
	}
	return true
}

// dashboardHumanMessageAction is the button on a message. All of it is the
// daemon's text, apart from the board title and the paths it names, which the
// dashboard renders as text. Unavailable says why the button is off.
type dashboardHumanMessageAction struct {
	Kind        string `json:"kind"`
	Label       string `json:"label"`
	Detail      string `json:"detail,omitempty"`
	Note        string `json:"note,omitempty"`
	Unavailable string `json:"unavailable,omitempty"`
}

// humanActionView builds a message's button as things stand now: the directory
// is where the agent is, and an agent or board that is gone turns it off. dirs
// holds the directories looked up so far, by agent, for this listing.
func humanActionView(m *db.HumanMessage, dirs map[string]string) *dashboardHumanMessageAction {
	if m.Action == "" {
		return nil
	}
	kind, arg := parseHumanAction(m.Action)
	view := &dashboardHumanMessageAction{Kind: kind}
	switch kind {
	case humanActionTerminal:
		view.Label, view.Note = "Open a terminal here", humanActionShellNote
		dir, seen := dirs[m.FromAgent]
		if !seen {
			if convID := humanActionLiveConv(m.FromAgent); convID != "" {
				dir = humanActionDir(convID)
			}
			dirs[m.FromAgent] = dir
		}
		view.Detail = dir
		if dir == "" {
			view.Unavailable = "That agent is gone, or its directory is not known."
		}
	case humanActionBoard:
		view.Label = "Open board"
		board, err := db.GetCollabBoard(arg)
		if err != nil {
			view.Unavailable = "That board is gone."
			return view
		}
		view.Label = "Open board " + boardActionTitle(board)
	case humanActionPR:
		view.Label, view.Detail = "Open PR", arg
		if number := deriveGitHubPRNumber(arg); number > 0 {
			view.Label = fmt.Sprintf("Open PR #%d", number)
		}
		if allowed, err := humanActionPRAllowed(m.FromAgent, arg); err != nil || !allowed {
			view.Unavailable = "That PR is no longer presented."
		}
	case humanActionTab:
		tab, ok := humanActionTabs[arg]
		if !ok {
			return nil
		}
		view.Label = "Go to " + tab.label
	default:
		return nil
	}
	return view
}

func boardActionTitle(board *db.CollabBoard) string {
	if board.Title != "" {
		return board.Title
	}
	return board.ID
}

// humanActionLiveConv is the agent's current conversation, or "" once it has
// retired.
func humanActionLiveConv(agentID string) string {
	row, err := db.GetAgent(agentID)
	if err != nil || !row.Active() {
		return ""
	}
	convID, err := db.CurrentConvForAgent(agentID)
	if err != nil {
		return ""
	}
	return convID
}

// humanActionTarget is what the dashboard does on a click, all of it resolved
// here from the stored action. Nothing in the click request can change it.
type humanActionTarget struct {
	Kind  string `json:"kind"`
	Agent string `json:"agent,omitempty"`
	Group string `json:"group,omitempty"`
	Board string `json:"board,omitempty"`
	Title string `json:"title,omitempty"`
	URL   string `json:"url,omitempty"`
	Path  string `json:"path,omitempty"`
}

// resolveHumanAction turns a stored action into its target. The error is for
// the operator.
func resolveHumanAction(m *db.HumanMessage) (*humanActionTarget, error) {
	view := humanActionView(m, map[string]string{})
	if view == nil {
		return nil, errors.New("this message has no action")
	}
	if view.Unavailable != "" {
		return nil, errors.New(view.Unavailable)
	}
	_, arg := parseHumanAction(m.Action)
	target := &humanActionTarget{Kind: view.Kind}
	switch view.Kind {
	case humanActionTerminal:
		target.Agent = m.FromAgent
		target.Title = m.FromTitle
		target.Group = humanActionShellGroup(m)
	case humanActionBoard:
		board, err := db.GetCollabBoard(arg)
		if err != nil {
			return nil, errors.New("That board is gone.")
		}
		target.Board, target.Title = board.ID, boardActionTitle(board)
	case humanActionPR:
		target.URL = arg
	case humanActionTab:
		target.Path = humanActionTabs[arg].path
	}
	return target, nil
}

// humanActionShellGroup is the group whose environment the shell starts with:
// the one the agent wrote from while it still belongs, otherwise any of its own.
func humanActionShellGroup(m *db.HumanMessage) string {
	groups, err := db.ListGroupsForConv(humanActionLiveConv(m.FromAgent))
	if err != nil || len(groups) == 0 {
		return ""
	}
	for _, group := range groups {
		if group.Name == m.GroupName {
			return group.Name
		}
	}
	return groups[0].Name
}

// handleDashboardHumanMessageAction serves POST /api/human-messages/action, the
// operator's click on a message's button. The body names only the message. The
// audit row says which action ran, and the message counts as read.
func handleDashboardHumanMessageAction(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if !collabRoutesEnabled() {
		writeError(w, http.StatusNotFound, collabDisabledCode, config.CollabDisabledMessage)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4*1024)
	var body struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_arg", "id is required")
		return
	}
	m, err := db.GetHumanMessage(body.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db", err.Error())
		return
	}
	if m == nil {
		writeError(w, http.StatusNotFound, "not_found", "no such message")
		return
	}
	target, err := resolveHumanAction(m)
	if err != nil {
		writeError(w, http.StatusConflict, "action_unavailable", err.Error())
		return
	}
	setAuditDetail(r, m.Action)
	if _, err := db.MarkHumanMessageRead(m.ID); err != nil {
		slog.Warn("human message action: mark read failed", "id", m.ID, "error", err)
	}
	writeJSON(w, http.StatusOK, target)
}
