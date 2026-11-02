// Package stewardtools lists what an external steward's assistant can call
// through `tclaude mcp serve`. The MCP server registers exactly these tools and
// the Config page shows this list, so what the operator reads before turning the
// feature on is what the assistant gets. It holds no authorization: every tool
// calls ordinary /v1 routes, whose own gates decide.
package stewardtools

// Tool describes one MCP tool.
type Tool struct {
	Name string `json:"name"`
	// Description is written for both readers: the assistant choosing a tool and
	// the operator deciding whether to allow it.
	Description string `json:"description"`
	// Routes are the daemon routes the tool calls.
	Routes []string `json:"routes"`
	// Access says in words what decides the tool, for the operator choosing what to
	// allow. A permission often widens a tool rather than switching it on.
	Access string `json:"access"`
	// Permissions are the slugs Access names, empty when the tool only touches the
	// principal's own identity or inbox.
	Permissions []string `json:"permissions,omitempty"`
	// Writes marks a tool that changes something another party sees.
	Writes bool `json:"writes,omitempty"`
}

// The tools by name. The server's handlers are keyed on these.
const (
	Whoami           = "whoami"
	FleetBrief       = "fleet_brief"
	ListAgents       = "list_agents"
	Inbox            = "inbox"
	ReadMessage      = "read_message"
	SendMessage      = "send_message"
	Reply            = "reply"
	NotifyHuman      = "notify_human"
	WaitForActivity  = "wait_for_activity"
	ListBoards       = "list_boards"
	ReadBoard        = "read_board"
	CreateBoard      = "create_board"
	UpdateBoard      = "update_board"
	DashboardView    = "dashboard_view"
	DashboardCue     = "dashboard_cue"
	DashboardCapture = "dashboard_capture"
	SetTaskLink      = "set_task_link"
)

// List returns the tools in the order they are offered.
func List() []Tool {
	return []Tool{
		{
			Name:        Whoami,
			Description: "Show this assistant's name and identity on tclaude.",
			Routes:      []string{"GET /v1/whoami"},
			Access:      "Always available",
		},
		{
			Name:        FleetBrief,
			Description: "Read the fleet: which agents are working, idle, offline or need the human, and unread messages.",
			Routes:      []string{"GET /v1/fleet"},
			Access:      "Needs fleet.read",
			Permissions: []string{"fleet.read"},
		},
		{
			Name:        ListAgents,
			Description: "List the agents and groups this assistant can reach, optionally one group.",
			Routes:      []string{"GET /v1/peers"},
			Access:      "Always shows its own groups and agents in no group; fleet.read adds every group",
			Permissions: []string{"fleet.read"},
		},
		{
			Name:        Inbox,
			Description: "List messages sent to this assistant, with short previews.",
			Routes:      []string{"GET /v1/inbox"},
			Access:      "Always available",
		},
		{
			Name:        ReadMessage,
			Description: "Read one message in full. This marks it read, which makes room in the inbox: past 10 unread messages, senders are told to retry.",
			Routes:      []string{"GET /v1/messages/{id}"},
			Access:      "Always available",
		},
		{
			Name:        SendMessage,
			Description: "Send a message to an agent or a group, including member@peer on a linked tclaude instance.",
			Routes:      []string{"POST /v1/messages"},
			Access:      "An agent needs message.direct, a shared or owned group, or a group link. A group needs it to be a member or owner. Another instance needs a group link or message.direct",
			Permissions: []string{"message.direct"},
			Writes:      true,
		},
		{
			Name:        Reply,
			Description: "Reply to a message this assistant received.",
			Routes:      []string{"POST /v1/messages/{id}/reply"},
			Access:      "Always available: having received a message is enough",
			Writes:      true,
		},
		{
			Name:        NotifyHuman,
			Description: "Send the human a message in the dashboard's Messages tab.",
			Routes:      []string{"POST /v1/notify-human"},
			Access:      "Needs human.notify, or owning a group",
			Permissions: []string{"human.notify"},
			Writes:      true,
		},
		{
			Name:        WaitForActivity,
			Description: "Wait up to 50 seconds for new messages or a change in what needs attention. Pass the cursor from the last call to get only what changed since. A call with no cursor returns at once with the current state.",
			Routes:      []string{"GET /v1/inbox", "GET /v1/fleet"},
			Access:      "Always watches mail; fleet.read adds what needs attention",
			Permissions: []string{"fleet.read"},
		},
		{
			Name:        ListBoards,
			Description: "List the collab boards.",
			Routes:      []string{"GET /v1/boards"},
			Access:      "Needs boards.read",
			Permissions: []string{"boards.read"},
		},
		{
			Name:        ReadBoard,
			Description: "Read a board's elements. Works with no dashboard open.",
			Routes:      []string{"GET /v1/boards/{id}"},
			Access:      "Needs boards.read",
			Permissions: []string{"boards.read"},
		},
		{
			Name:        CreateBoard,
			Description: "Create a collab board, optionally for a group.",
			Routes:      []string{"POST /v1/boards"},
			Access:      "Needs boards.write",
			Permissions: []string{"boards.write"},
			Writes:      true,
		},
		{
			Name: UpdateBoard,
			Description: "Add, change or delete board elements. To change an element, send it whole with version + 1. " +
				"To delete one, send it with isDeleted true and version + 1.",
			Routes:      []string{"PATCH /v1/boards/{id}"},
			Access:      "Needs boards.write",
			Permissions: []string{"boards.write"},
			Writes:      true,
		},
		{
			Name: DashboardView,
			Description: "Read which view the human's dashboard is on. known is false when no dashboard has reported since " +
				"the daemon started. A view can be stale: age_seconds and tab_heard_age_seconds say how old it is.",
			Routes:      []string{"GET /v1/dashboard/view"},
			Access:      "Needs dashboard.view.read",
			Permissions: []string{"dashboard.view.read"},
		},
		{
			Name: DashboardCue,
			Description: "Move the human's dashboard to a view, or highlight something there with a note. A cue expires, and " +
				"nobody sees it if no dashboard opens in time: dashboard_open in the answer says whether one is open now.",
			Routes:      []string{"POST /v1/dashboard/cue"},
			Access:      "Needs dashboard.guide",
			Permissions: []string{"dashboard.guide"},
			Writes:      true,
		},
		{
			Name: DashboardCapture,
			Description: "See the human's dashboard as an image. The human is shown that it happened. It fails, and says " +
				"why, when no dashboard is open or none is on screen.",
			Routes:      []string{"POST /v1/dashboard/capture"},
			Access:      "Off until you grant dashboard.capture, because the image goes to your assistant's provider",
			Permissions: []string{"dashboard.capture"},
		},
		{
			Name:        SetTaskLink,
			Description: "Set or clear the task link shown beside an agent in the dashboard.",
			Routes:      []string{"POST /v1/agent/{agent}/task"},
			Access:      "Needs agent.task, which comes with the Workers level, or owning a group with that agent in it",
			Permissions: []string{"agent.task"},
			Writes:      true,
		},
	}
}
