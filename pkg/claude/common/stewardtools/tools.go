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
	Whoami          = "whoami"
	FleetBrief      = "fleet_brief"
	ListAgents      = "list_agents"
	Inbox           = "inbox"
	ReadMessage     = "read_message"
	SendMessage     = "send_message"
	Reply           = "reply"
	NotifyHuman     = "notify_human"
	WaitForActivity = "wait_for_activity"
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
			Access:      "An agent needs message.direct or a shared group, a group needs it to be a member or owner, and another instance needs a group link or message.direct",
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
			Access:      "Needs human.notify",
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
	}
}
