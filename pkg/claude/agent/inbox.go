package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/common"
)

func inboxCmd() *cobra.Command {
	c := boa.CmdT[struct{}]{
		Use:         "inbox",
		Short:       "Read messages addressed to the current agent",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			inboxLsCmd(),
			inboxReadCmd(),
		},
	}.ToCobra()
	// `mailbox` is the longer-form term we're standardising on (matches the
	// planned interactive `mailbox -w` view tracked in agents_todo.md).
	// Both names work.
	c.Aliases = []string{"mailbox", "mail"}
	return c
}

// --- inbox ls ---

type inboxLsParams struct {
	Limit  int  `long:"limit" short:"n" help:"Max number of messages to show" default:"20"`
	Unread bool `long:"unread" help:"Only show messages without read_at"`
	JSON   bool `long:"json" help:"Output JSON"`
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
	From      string `json:"from"`
	FromShort string `json:"from_short"`
	Group     string `json:"group"`
	Subject   string `json:"subject,omitempty"`
	Preview   string `json:"preview,omitempty"`
	CreatedAt string `json:"created_at"`
	Read      bool   `json:"read"`
}

func runInboxLs(p *inboxLsParams, stdout, stderr io.Writer) int {
	myID, err := currentConvID()
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcNotFound
	}
	msgs, err := db.ListAgentMessagesForConv(myID, p.Limit)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcIOFailure
	}

	groupNameByID := map[int64]string{}
	groups, _ := db.ListAgentGroups()
	for _, g := range groups {
		groupNameByID[g.ID] = g.Name
	}

	var out []inboxEntry
	for _, m := range msgs {
		if p.Unread && !m.ReadAt.IsZero() {
			continue
		}
		out = append(out, inboxEntry{
			ID:        m.ID,
			From:      m.FromConv,
			FromShort: short(m.FromConv),
			Group:     groupNameByID[m.GroupID],
			Subject:   m.Subject,
			Preview:   preview(m.Body),
			CreatedAt: m.CreatedAt.Format(time.RFC3339),
			Read:      !m.ReadAt.IsZero(),
		})
	}

	if p.JSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
		return rcOK
	}
	if len(out) == 0 {
		fmt.Fprintln(stdout, "(empty inbox)")
		return rcOK
	}
	for _, e := range out {
		marker := "*" // unread
		if e.Read {
			marker = " "
		}
		subj := e.Subject
		if subj == "" {
			subj = e.Preview
		}
		fmt.Fprintf(stdout, "%s #%-4d %-8s  group=%-12s  %s\n", marker, e.ID, e.FromShort, e.Group, subj)
	}
	return rcOK
}

// --- inbox read ---

type inboxReadParams struct {
	ID         string `pos:"true" help:"Message ID from inbox ls"`
	KeepUnread bool   `long:"keep-unread" help:"Don't update read_at"`
}

func inboxReadCmd() *cobra.Command {
	return boa.CmdT[inboxReadParams]{
		Use:         "read",
		Short:       "Print a message body and mark it read",
		ParamEnrich: common.DefaultParamEnricher(),
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

	groupName := ""
	if g, _ := groupByID(m.GroupID); g != nil {
		groupName = g.Name
	}
	fromAlias := aliasFor(m.GroupID, m.FromConv)

	fmt.Fprintln(stdout, "Headers:")
	fmt.Fprintf(stdout, "  Message-ID: %d\n", m.ID)
	if fromAlias != "" {
		fmt.Fprintf(stdout, "  From:       %s <%s>\n", fromAlias, m.FromConv)
	} else {
		fmt.Fprintf(stdout, "  From:       %s\n", m.FromConv)
	}
	fmt.Fprintf(stdout, "  To:         %s\n", m.ToConv)
	fmt.Fprintf(stdout, "  Group:      %s\n", groupName)
	if m.Subject != "" {
		fmt.Fprintf(stdout, "  Subject:    %s\n", m.Subject)
	}
	fmt.Fprintf(stdout, "  Date:       %s\n", m.CreatedAt.Format(time.RFC3339))
	fmt.Fprintln(stdout, "")
	fmt.Fprintln(stdout, "Body:")
	fmt.Fprintln(stdout, m.Body)

	if !p.KeepUnread && m.ReadAt.IsZero() {
		_ = db.MarkAgentMessageRead(id)
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

func preview(body string) string {
	const max = 80
	r := []rune(body)
	if len(r) <= max {
		return body
	}
	return string(r[:max]) + "…"
}
