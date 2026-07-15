package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/table"
	"github.com/tofutools/tclaude/pkg/common"
)

func inboxCmd() *cobra.Command {
	c := boa.CmdT[struct{}]{
		Use:         "inbox",
		Short:       "Read messages addressed to the current agent",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			inboxLsCmd(),
			inboxWatchCmd(),
			inboxSentCmd(),
			inboxReadCmd(),
			inboxPruneCmd(),
		},
	}.ToCobra()
	// `mailbox` is the longer-form term we're standardising on (matches the
	// planned interactive `mailbox -w` view). Both names work.
	c.Aliases = []string{"mailbox", "mail"}
	return c
}

// --- inbox ls ---

type inboxLsParams struct {
	Limit  int    `long:"limit" short:"n" help:"Max number of messages to show" default:"20"`
	Unread bool   `long:"unread" help:"Only show messages without read_at"`
	JSON   bool   `long:"json" help:"Output JSON"`
	Target string `long:"target" optional:"true" help:"Read another agent's inbox (title / prefix / conv-id). Requires the agent.inbox-watch slug or group ownership."`
}

func inboxLsCmd() *cobra.Command {
	return boa.CmdT[inboxLsParams]{
		Use:         "ls",
		Short:       "List messages in this conversation's inbox",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *inboxLsParams, _ *cobra.Command, _ []string) {
			os.Exit(runInboxLs(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

type inboxEntry struct {
	ID        int64  `json:"id"`
	From      string `json:"from,omitempty"`
	FromShort string `json:"from_short,omitempty"`
	To        string `json:"to,omitempty"`
	ToShort   string `json:"to_short,omitempty"`
	Group     string `json:"group"`
	Subject   string `json:"subject,omitempty"`
	Preview   string `json:"preview,omitempty"`
	CreatedAt string `json:"created_at"`
	Read      bool   `json:"read"`
	Delivered bool   `json:"delivered,omitempty"`
	ParentID  int64  `json:"parent_id,omitempty"`
}

func runInboxLs(p *inboxLsParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	return runInboxLsDaemon(p, stdout, stderr)
}

func runInboxLsDaemon(p *inboxLsParams, stdout, stderr io.Writer) int {
	q := fmt.Sprintf("/v1/inbox?limit=%d", p.Limit)
	if p.Unread {
		q += "&unread=1"
	}
	var out []inboxEntry
	if err := DaemonRequest("GET", q, nil, &out, DaemonOpts{TargetConv: p.Target}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	return renderInbox(p, out, stdout)
}

func renderInbox(p *inboxLsParams, out []inboxEntry, stdout io.Writer) int {
	if p.JSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			return rcIOFailure
		}
		return rcOK
	}
	if len(out) == 0 {
		fmt.Fprintln(stdout, "(empty inbox)")
		return rcOK
	}
	tbl := table.New(
		table.Column{Header: "", Width: 1},
		table.Column{Header: "ID", Width: 5, Align: table.AlignRight},
		table.Column{Header: "FROM", Width: 12},
		table.Column{Header: "GROUP", MinWidth: 6, Weight: 0.4, Truncate: true},
		table.Column{Header: "SUBJECT", MinWidth: 10, Weight: 1.6, Truncate: true},
	)
	tbl.SetTerminalWidth(table.GetTerminalWidth())
	for _, e := range out {
		marker := "*"
		if e.Read {
			marker = " "
		}
		subj := e.Subject
		if subj == "" {
			subj = e.Preview
		}
		// Prefix replies with "↳ " so threads stand out at a glance.
		// Cheap visual cue; the parent_id is in --json for tools that
		// want to render the chain explicitly.
		if e.ParentID > 0 {
			subj = "↳ " + subj
		}
		tbl.AddRow(table.Row{Cells: []string{
			marker,
			fmt.Sprintf("%d", e.ID),
			e.FromShort,
			e.Group,
			subj,
		}})
	}
	fmt.Fprintln(stdout, tbl.Render())
	return rcOK
}

// --- inbox sent ---

type inboxSentParams struct {
	Limit int  `long:"limit" short:"n" help:"Max number of messages to show" default:"20"`
	JSON  bool `long:"json" help:"Output JSON"`
}

func inboxSentCmd() *cobra.Command {
	return boa.CmdT[inboxSentParams]{
		Use:         "sent",
		Short:       "List messages this conversation has sent (outbox)",
		Long:        "Outbox view: messages this conv-id is the sender of, most recent first. Shows delivery + read state from the recipient's side.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *inboxSentParams, _ *cobra.Command, _ []string) {
			os.Exit(runInboxSent(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runInboxSent(p *inboxSentParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	q := fmt.Sprintf("/v1/inbox?limit=%d&outbox=1", p.Limit)
	var out []inboxEntry
	if err := DaemonGet(q, &out); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	return renderOutbox(p, out, stdout)
}

func renderOutbox(p *inboxSentParams, out []inboxEntry, stdout io.Writer) int {
	if p.JSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			return rcIOFailure
		}
		return rcOK
	}
	if len(out) == 0 {
		fmt.Fprintln(stdout, "(no sent messages)")
		return rcOK
	}
	// Status column collapses delivered/read into a single 3-char glyph
	// so the row stays narrow:
	//   "···" = queued (not delivered yet, target offline at send time)
	//   "→··" = delivered, recipient hasn't read it
	//   "→✓·" = delivered + recipient read
	tbl := table.New(
		table.Column{Header: "ST", Width: 3},
		table.Column{Header: "ID", Width: 5, Align: table.AlignRight},
		table.Column{Header: "TO", Width: 12},
		table.Column{Header: "GROUP", MinWidth: 6, Weight: 0.4, Truncate: true},
		table.Column{Header: "SUBJECT", MinWidth: 10, Weight: 1.6, Truncate: true},
	)
	tbl.SetTerminalWidth(table.GetTerminalWidth())
	for _, e := range out {
		subj := e.Subject
		if subj == "" {
			subj = e.Preview
		}
		tbl.AddRow(table.Row{Cells: []string{
			outboxStatusGlyph(e),
			fmt.Sprintf("%d", e.ID),
			e.ToShort,
			e.Group,
			subj,
		}})
	}
	fmt.Fprintln(stdout, tbl.Render())
	return rcOK
}

func outboxStatusGlyph(e inboxEntry) string {
	switch {
	case e.Read:
		return "→✓·"
	case e.Delivered:
		return "→··"
	default:
		return "···"
	}
}

// --- inbox read ---

type inboxReadParams struct {
	ID         string `pos:"true" help:"Message ID from inbox ls"`
	KeepUnread bool   `long:"keep-unread" help:"Don't update read_at"`
	Target     string `long:"target" optional:"true" help:"Read another agent's message (title / prefix / conv-id). Implies --keep-unread. Requires the agent.inbox-watch slug or group ownership."`
}

func inboxReadCmd() *cobra.Command {
	return boa.CmdT[inboxReadParams]{
		Use:         "read",
		Short:       "Print a message body and mark it read",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *inboxReadParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.ID).SetAlternativesFunc(completeInboxMessageIDs)
			return nil
		},
		RunFunc: func(p *inboxReadParams, _ *cobra.Command, _ []string) {
			os.Exit(runInboxRead(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runInboxRead(p *inboxReadParams, stdout, stderr io.Writer) int {
	id, err := strconv.ParseInt(p.ID, 10, 64)
	if err != nil {
		fmt.Fprintf(stderr, "Error: invalid message ID %q\n", p.ID)
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	return runInboxReadDaemon(p, id, stdout, stderr)
}

// recipientLine mirrors the daemon's response shape for the audience
// arrays (to_recipients / cc_recipients on /v1/messages/{id}). AgentID is
// the recipient's stable agent_id (JOH-27 PR3b-2); empty for a non-agent
// conv, where the renderer falls back to the conv-id prefix.
type recipientLine struct {
	ConvID  string `json:"conv_id"`
	AgentID string `json:"agent_id"`
	Title   string `json:"title"`
}

type inboxAttachment struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	Size        int64  `json:"size"`
	ContentType string `json:"content_type"`
}

// formatRecipientList renders a recipient list as comma-separated
// "title <agent_id>" entries (or just the short id when no title is
// known), leading with the stable agent_id so the audience reads by the
// rotation-immune handle, falling back to the conv-id prefix for a
// non-agent recipient.
func formatRecipientList(rs []recipientLine) string {
	parts := make([]string, 0, len(rs))
	for _, r := range rs {
		id := shortAgentID(r.AgentID, r.ConvID)
		if r.Title != "" {
			parts = append(parts, fmt.Sprintf("%s <%s>", r.Title, id))
		} else {
			parts = append(parts, id)
		}
	}
	return strings.Join(parts, ", ")
}

func runInboxReadDaemon(p *inboxReadParams, id int64, stdout, stderr io.Writer) int {
	path := fmt.Sprintf("/v1/messages/%d", id)
	if p.KeepUnread {
		path += "?keep-unread=1"
	}
	var m struct {
		ID             int64             `json:"id"`
		From           string            `json:"from"`
		FromAgent      string            `json:"from_agent"`
		FromTitle      string            `json:"from_title"`
		To             string            `json:"to"`
		ToAgent        string            `json:"to_agent"`
		OriginalToConv string            `json:"original_to_conv,omitempty"`
		Group          string            `json:"group"`
		Subject        string            `json:"subject"`
		Body           string            `json:"body"`
		CreatedAt      string            `json:"created_at"`
		ReplyTo        string            `json:"reply_to"`
		ReplyCmd       string            `json:"reply_cmd"`
		InReplyTo      int64             `json:"in_reply_to,omitempty"`
		ParentSubject  string            `json:"parent_subject,omitempty"`
		ToRecipients   []recipientLine   `json:"to_recipients,omitempty"`
		CcRecipients   []recipientLine   `json:"cc_recipients,omitempty"`
		Attachments    []inboxAttachment `json:"attachments,omitempty"`
	}
	if err := DaemonRequest("GET", path, nil, &m, DaemonOpts{TargetConv: p.Target}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	fmt.Fprintln(stdout, "Headers:")
	fmt.Fprintf(stdout, "  Message-ID: %d\n", m.ID)
	if m.InReplyTo > 0 {
		// Mirror RFC-5322's In-Reply-To header. If the parent's
		// subject is known, surface it inline so the agent can
		// orient itself without an extra `inbox read <parent_id>`.
		if m.ParentSubject != "" {
			fmt.Fprintf(stdout, "  In-Reply-To: %d (%q)\n", m.InReplyTo, m.ParentSubject)
		} else {
			fmt.Fprintf(stdout, "  In-Reply-To: %d\n", m.InReplyTo)
		}
	}
	fmt.Fprintf(stdout, "  From:       %s\n", actorHeader(m.FromTitle, m.FromAgent, m.From))
	if len(m.ToRecipients) > 0 {
		// Email-style audience: render the full To: list (and CC: if
		// present) instead of the single per-row to_conv. Lets the
		// receiver see who else is on the thread without an extra
		// round-trip.
		fmt.Fprintf(stdout, "  To:         %s\n", formatRecipientList(m.ToRecipients))
	} else {
		fmt.Fprintf(stdout, "  To:         %s\n", actorID(m.ToAgent, m.To))
	}
	if m.OriginalToConv != "" {
		// The sender addressed a superseded conv-id; the daemon walked
		// the succession chain forward and routed to this row's To
		// (the live successor). Surface the original target so the
		// receiver knows the message was originally meant for their
		// predecessor — useful when picking up partially-handled work
		// from a prior incarnation.
		fmt.Fprintf(stdout, "  Original-To: %s (superseded by current %s)\n",
			short(m.OriginalToConv), short(m.To))
	}
	if len(m.CcRecipients) > 0 {
		fmt.Fprintf(stdout, "  CC:         %s\n", formatRecipientList(m.CcRecipients))
	}
	group := m.Group
	if group == "" {
		// A direct message (the universal-inbox transport) has no
		// routing group; say so rather than printing a blank.
		group = "(direct)"
	}
	fmt.Fprintf(stdout, "  Group:      %s\n", group)
	if m.Subject != "" {
		fmt.Fprintf(stdout, "  Subject:    %s\n", m.Subject)
	}
	fmt.Fprintf(stdout, "  Date:       %s\n", m.CreatedAt)
	if m.ReplyTo != "" {
		fmt.Fprintf(stdout, "  Reply-To:   %s\n", actorID(m.FromAgent, m.ReplyTo))
	}
	if m.ReplyCmd != "" {
		fmt.Fprintf(stdout, "  Reply-Cmd:  %s\n", m.ReplyCmd)
	}
	fmt.Fprintln(stdout, "")
	fmt.Fprintln(stdout, "Body:")
	fmt.Fprintln(stdout, m.Body)
	if len(m.Attachments) > 0 {
		fmt.Fprintln(stdout, "")
		fmt.Fprintln(stdout, "Attachments:")
		for _, a := range m.Attachments {
			fmt.Fprintf(stdout, "  %s (%d bytes, %s): %s\n", a.Name, a.Size, a.ContentType, a.Path)
		}
	}
	return rcOK
}

// actorID is the identifier shown for a message actor in an inbox-read
// header: the stable full agent_id (JOH-27) when present, else the conv-id
// (a non-actor conv, or an older daemon that didn't send the agent field).
// Inbox read is a single-message detail view, so the FULL id is used per
// the display split — conv-id stays available in the daemon's JSON.
func actorID(agentID, convID string) string {
	if agentID != "" {
		return agentID
	}
	return convID
}

// actorHeader renders a message actor as "name (agent_id)" for an
// inbox-read From header, or just the id when no title is indexed.
func actorHeader(title, agentID, convID string) string {
	if title == "" && agentID == "" && convID == "" {
		return "human operator"
	}
	id := actorID(agentID, convID)
	if title == "" {
		return id
	}
	return fmt.Sprintf("%s (%s)", title, id)
}

func runInboxReadDirect(p *inboxReadParams, id int64, stdout, stderr io.Writer) int {
	myID, err := currentConvID()
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcNotFound
	}
	m, err := db.GetAgentMessage(id)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcIOFailure
	}
	if m == nil {
		fmt.Fprintf(stderr, "Error: no message #%d\n", id)
		return rcNotFound
	}
	// Only the recipient may read. Sender can see their own outgoing
	// messages elsewhere (out of scope for v1).
	if m.ToConv != myID {
		fmt.Fprintf(stderr, "Error: message #%d is not addressed to you\n", id)
		return rcAuth
	}

	groupName := "(direct)"
	if g, _ := groupByID(m.GroupID); g != nil {
		groupName = g.Name
	}
	fromTitle := titleFor(m.FromConv)

	fmt.Fprintln(stdout, "Headers:")
	fmt.Fprintf(stdout, "  Message-ID: %d\n", m.ID)
	fmt.Fprintf(stdout, "  From:       %s\n", actorHeader(fromTitle, m.FromAgent, m.FromConv))
	fmt.Fprintf(stdout, "  To:         %s\n", actorID(m.ToAgent, m.ToConv))
	if m.OriginalToConv != "" {
		fmt.Fprintf(stdout, "  Original-To: %s (superseded by current %s)\n",
			short(m.OriginalToConv), short(m.ToConv))
	}
	fmt.Fprintf(stdout, "  Group:      %s\n", groupName)
	if m.Subject != "" {
		fmt.Fprintf(stdout, "  Subject:    %s\n", m.Subject)
	}
	fmt.Fprintf(stdout, "  Date:       %s\n", m.CreatedAt.Format(time.RFC3339))
	fmt.Fprintf(stdout, "  Reply-To:   %s\n", actorID(m.FromAgent, m.FromConv))
	fmt.Fprintf(stdout, "  Reply-Cmd:  tclaude agent reply %d \"<your reply body>\"\n", m.ID)
	fmt.Fprintln(stdout, "")
	fmt.Fprintln(stdout, "Body:")
	fmt.Fprintln(stdout, m.Body)

	if !p.KeepUnread && m.ReadAt.IsZero() {
		if err := db.MarkAgentMessageRead(id); err != nil {
			fmt.Fprintf(stderr, "Warning: failed to mark message %d as read: %v\n", id, err)
		}
	}
	return rcOK
}

func groupByID(id int64) (*db.AgentGroup, error) {
	groups, err := db.ListAgentGroups()
	if err != nil {
		return nil, err
	}
	for _, g := range groups {
		if g.ID == id {
			return g, nil
		}
	}
	return nil, nil
}

// --- inbox prune ---

type inboxPruneParams struct {
	OlderThan string `long:"older-than" help:"Delete messages older than this duration (e.g. 30d, 2w, 12h). Required."`
	ReadOnly  bool   `long:"read-only" help:"Only delete messages the recipient has read"`
}

func inboxPruneCmd() *cobra.Command {
	return boa.CmdT[inboxPruneParams]{
		Use:   "prune",
		Short: "Delete old messages from this conversation's mail history",
		Long: "Removes agent_messages rows older than --older-than that this conv " +
			"is the sender or recipient of. Use --read-only to keep unread messages.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *inboxPruneParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.OlderThan).SetAlternativesFunc(completePruneDurations)
			return nil
		},
		RunFunc: func(p *inboxPruneParams, _ *cobra.Command, _ []string) {
			os.Exit(runInboxPrune(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func completePruneDurations(_ *cobra.Command, _ []string, toComplete string) []string {
	suggestions := []string{"7d", "30d", "90d", "1w", "4w", "12w"}
	out := []string{}
	for _, s := range suggestions {
		if strings.HasPrefix(s, toComplete) {
			out = append(out, s)
		}
	}
	return out
}

func runInboxPrune(p *inboxPruneParams, stdout, stderr io.Writer) int {
	if p.OlderThan == "" {
		fmt.Fprintln(stderr, "Error: --older-than is required (e.g. 30d, 2w, 12h)")
		return rcInvalidArg
	}
	d, err := parseDurationDays(p.OlderThan)
	if err != nil || d <= 0 {
		fmt.Fprintf(stderr, "Error: invalid --older-than %q (try 30d, 2w, 12h)\n", p.OlderThan)
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	body := map[string]any{
		"older_than_seconds": int64(d / time.Second),
		"read_only":          p.ReadOnly,
	}
	var resp struct {
		Deleted int64 `json:"deleted"`
	}
	if err := DaemonPost("/v1/inbox/prune", body, &resp); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	scope := "all"
	if p.ReadOnly {
		scope = "read-only"
	}
	fmt.Fprintf(stdout, "Pruned %d %s message(s) older than %s\n", resp.Deleted, scope, d)
	return rcOK
}
