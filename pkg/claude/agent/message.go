package agent

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"unicode"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/common"
)

// queuedState renders durable inbox acceptance. pending is the recipient's
// unprocessed regular-message backlog INCLUDING this message, so pending-1 is
// how many earlier messages remain. Nudge delivery happens asynchronously and
// may be deliberately skipped while the target is offline.
func queuedState(pending int) string {
	if pending > 1 {
		return fmt.Sprintf("saved; %d earlier message(s) pending", pending-1)
	}
	return "saved to target inbox"
}

type messageParams struct {
	Target  string   `pos:"true" help:"Target conv (UUID/prefix/title), 'group:<name|id>' to broadcast, or bare 'group:' for your own group"`
	Text    string   `pos:"true" optional:"true" help:"Message body (or use --body / --stdin / --file)"`
	Body    string   `long:"body" optional:"true" help:"Message body as a flag instead of positional text"`
	Subject string   `long:"subject" short:"s" optional:"true" help:"Optional subject line"`
	Stdin   bool     `long:"stdin" help:"Read body from stdin"`
	File    string   `long:"file" short:"f" optional:"true" help:"Read body from a file ('-' reads stdin). Sidesteps shell quoting — best for long, multi-line, or backtick-containing bodies (the shell eats backticks from an inline body)."`
	Cc      []string `long:"cc" optional:"true" help:"CC recipient (title / conv-id / 8+-char prefix). Repeatable. Each gets its own row + nudge; the To and CC audience appears on every recipient's view."`
	Role    string   `long:"role" optional:"true" help:"With a 'group:' target, broadcast only to members holding this role (case-insensitive). Error on a 1:1 target."`
	Gen     string   `long:"gen" optional:"true" help:"Deliver to a SPECIFIC previous generation of the target agent: a conv-id that must belong to the agent the target resolves to. Normally a message follows the agent to its current generation; --gen pins it to that exact past conv. Direct (non-group, non-cc) sends only."`
}

func messageCmd() *cobra.Command {
	return boa.CmdT[messageParams]{
		Use:     "message",
		Aliases: []string{"msg", "send"},
		Short:   "Send a message to another agent (or 'group:<name|id>' to broadcast)",
		Long: "Persists the message in SQLite and asks the daemon to nudge a ready pane. Offline notification attempts are skipped while the durable unread inbox row remains available. " +
			"A target with 10 unprocessed regular messages applies backpressure: direct sends are rejected without writing a row, while group sends warn per full recipient and continue for available members. " +
			"A target prefixed with 'group:' fans out: one row per non-sender member, queueing each for delivery. The group can be named ('group:reviewers'), given by numeric id ('group:7'), or left empty ('group:') to mean your own group — the latter is an error unless you are a member of exactly one group. " +
			"The sender must be a member or owner of the group to broadcast. Add --role to broadcast only to members holding a given role.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *messageParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Target).SetAlternativesFunc(completeMessageTargets)
			boa.GetParamT(ctx, &p.Cc).SetAlternativesFunc(completeConvSelectors)
			boa.GetParamT(ctx, &p.Role).SetAlternativesFunc(completeRoles)
			return nil
		},
		RunFunc: func(p *messageParams, _ *cobra.Command, _ []string) {
			os.Exit(runMessage(p, os.Stdout, os.Stderr, os.Stdin))
		},
	}.ToCobra()
}

func runMessage(p *messageParams, stdout, stderr io.Writer, stdin io.Reader) int {
	// --role only narrows a group: multicast. On a 1:1 target there is
	// no member set to filter — fail fast with a clear message before
	// the daemon round-trip (the daemon enforces the same rule).
	if p.Role != "" && !strings.HasPrefix(p.Target, "group:") {
		fmt.Fprintf(stderr, "Error: --role is only valid with a 'group:' multicast target\n")
		return rcInvalidArg
	}
	// --gen pins delivery to one agent's specific past generation; it is
	// meaningless for a group: fan-out or a --cc multi-send. Fail fast before
	// the daemon round-trip (the daemon enforces the same rule).
	if p.Gen != "" && (strings.HasPrefix(p.Target, "group:") || len(p.Cc) > 0) {
		fmt.Fprintf(stderr, "Error: --gen is only valid on a direct (non-group, non-cc) send\n")
		return rcInvalidArg
	}
	body, status := readBody(p, true, stdin, stderr)
	if status != rcOK {
		return status
	}
	if strings.TrimSpace(body) == "" {
		fmt.Fprintf(stderr, "Error: message body is empty\n")
		return rcInvalidArg
	}

	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	return runMessageDaemon(p, body, stdout, stderr)
}

func runMessageDaemon(p *messageParams, body string, stdout, stderr io.Writer) int {
	var resp struct {
		ID             int64  `json:"id,omitempty"`
		Queued         bool   `json:"queued,omitempty"`
		Pending        int    `json:"pending,omitempty"`
		ViaGroup       string `json:"via_group"`
		RedirectedFrom string `json:"redirected_from,omitempty"`
		Recipients     []struct {
			ConvID         string `json:"conv_id"`
			AgentID        string `json:"agent_id,omitempty"`
			Title          string `json:"title,omitempty"`
			MessageID      int64  `json:"message_id"`
			Queued         bool   `json:"queued"`
			Pending        int    `json:"pending,omitempty"`
			QueueFull      bool   `json:"queue_full,omitempty"`
			Limit          int    `json:"limit,omitempty"`
			Error          string `json:"error,omitempty"`
			RedirectedFrom string `json:"redirected_from,omitempty"`
		} `json:"recipients,omitempty"`
	}
	payload := map[string]any{
		"to":      p.Target,
		"subject": p.Subject,
		"body":    body,
	}
	if len(p.Cc) > 0 {
		payload["cc"] = p.Cc
	}
	if p.Role != "" {
		payload["role"] = p.Role
	}
	if p.Gen != "" {
		payload["gen"] = p.Gen
	}
	err := DaemonRequest(http.MethodPost, "/v1/messages", payload, &resp, DaemonOpts{})
	if de, ok := err.(*DaemonError); ok && de.Code == "ambiguous" {
		fmt.Fprintf(stderr, "%s\n", de.Msg)
		return rcAmbiguous
	}
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	// Discriminate on the request, not the response. The daemon's
	// Recipients slice gets `omitempty`'d when empty (single-member
	// group), so checking response shape would misroute that case
	// into the direct-send branch. The CLI knows what it sent.
	//
	// `--cc` also fans out per-recipient (one row per To + each CC),
	// so we reuse the multicast rendering path in that case too.
	isMulticast := strings.HasPrefix(p.Target, "group:")
	hasCC := len(p.Cc) > 0
	if isMulticast || hasCC {
		if len(resp.Recipients) == 0 {
			// Always state the resolved count so a typo'd --role reads
			// as a visible no-op rather than a silent one.
			if p.Role != "" {
				fmt.Fprintf(stdout, "0 recipients: no other members with role %q in group %q; nothing sent.\n", p.Role, resp.ViaGroup)
			} else {
				fmt.Fprintf(stdout, "0 recipients in group %q (you're the only member); nothing sent.\n", resp.ViaGroup)
			}
			return rcOK
		}
		// Each queued recipient has a durable inbox row. Nudge delivery is
		// asynchronous and independent from this per-recipient acceptance result.
		queued := 0
		for _, rcp := range resp.Recipients {
			if rcp.Queued {
				queued++
			}
		}
		header := "Broadcast"
		if hasCC && !isMulticast {
			header = "Sent (multi-recipient)"
		}
		// A --cc send whose primary routed off-group has an empty
		// via_group; multicast always has a real group.
		scope := ""
		if resp.ViaGroup != "" {
			scope = fmt.Sprintf(" via group %q", resp.ViaGroup)
		}
		notQueued := len(resp.Recipients) - queued
		failNote := ""
		if notQueued > 0 {
			failNote = fmt.Sprintf(", %d failed", notQueued)
		}
		fmt.Fprintf(stdout, "%s%s: %d recipients (%d saved to inbox%s).\n",
			header, scope, len(resp.Recipients), queued, failNote)
		for _, rcp := range resp.Recipients {
			name := rcp.Title
			if name == "" {
				name = "(unnamed)"
			}
			state := "not queued"
			if rcp.Queued {
				state = queuedState(rcp.Pending)
			} else if rcp.Error != "" {
				state = "not queued: " + rcp.Error
			}
			redirect := ""
			if rcp.RedirectedFrom != "" {
				// You addressed an old conv-id; the daemon walked the
				// chain and routed to the live successor. Surface the
				// hop so the sender can update their selector if they
				// were typing a stale UUID.
				redirect = fmt.Sprintf("  [redirected from %s, superseded]", short(rcp.RedirectedFrom))
			}
			fmt.Fprintf(stdout, "  #%-6d %s  %s  (%s)%s\n", rcp.MessageID, shortAgentID(rcp.AgentID, rcp.ConvID), name, state, redirect)
		}
		if notQueued > 0 {
			fmt.Fprintf(stderr, "Warning: %d recipient(s) were not queued; see per-recipient details above.\n", notQueued)
			if queued == 0 {
				return rcIOFailure
			}
		}
		return rcOK
	}
	// The row is durable before this returns. Pending describes the recipient's
	// regular-message backlog, independently of the best-effort tmux nudge.
	state := queuedState(resp.Pending)
	// An off-group send (the message.direct path) has no routing group;
	// render it as a direct message rather than `via group ""`.
	via := "directly"
	if resp.ViaGroup != "" {
		via = fmt.Sprintf("via group %q", resp.ViaGroup)
	}
	if resp.RedirectedFrom != "" {
		// Direct-send redirect: addressed conv was superseded; daemon
		// re-routed to the live successor. Surface the hop so the
		// sender can correct stale selectors.
		fmt.Fprintf(stdout, "Sent message #%d %s (%s) → redirected from %s, superseded by current target\n",
			resp.ID, via, state, short(resp.RedirectedFrom))
	} else {
		fmt.Fprintf(stdout, "Sent message #%d %s (%s)\n", resp.ID, via, state)
	}
	return rcOK
}

func readBody(p *messageParams, allowBodyFlag bool, stdin io.Reader, stderr io.Writer) (string, int) {
	count := 0
	if p.Text != "" {
		count++
	}
	if p.Body != "" {
		count++
	}
	if p.Stdin {
		count++
	}
	if p.File != "" {
		count++
	}
	if count == 0 {
		if allowBodyFlag {
			fmt.Fprintf(stderr, "Error: provide positional text, --body, --stdin, or --file\n")
		} else {
			fmt.Fprintf(stderr, "Error: provide a body, --stdin, or --file\n")
		}
		return "", rcInvalidArg
	}
	if count > 1 {
		if allowBodyFlag {
			fmt.Fprintf(stderr, "Error: pass only one of positional text / --body / --stdin / --file\n")
		} else {
			fmt.Fprintf(stderr, "Error: pass only one of body / --stdin / --file\n")
		}
		return "", rcInvalidArg
	}
	switch {
	case p.Stdin:
		data, err := io.ReadAll(stdin)
		if err != nil {
			fmt.Fprintf(stderr, "Error reading stdin: %v\n", err)
			return "", rcIOFailure
		}
		return string(data), rcOK
	case p.File == "-":
		// `--file -` is the universal "read stdin" convention, shared
		// with spawn / reincarnate / clone / cron add via resolveBodyInput.
		data, err := io.ReadAll(stdin)
		if err != nil {
			fmt.Fprintf(stderr, "Error reading stdin: %v\n", err)
			return "", rcIOFailure
		}
		return string(data), rcOK
	case p.File != "":
		data, err := os.ReadFile(p.File)
		if err != nil {
			fmt.Fprintf(stderr, "Error reading file %q: %v\n", p.File, err)
			return "", rcIOFailure
		}
		return string(data), rcOK
	case p.Body != "":
		return p.Body, rcOK
	}
	return p.Text, rcOK
}

// titleFor returns convID's display title from the conv_index cache,
// or "" when the conv isn't indexed. Cheap (no .jsonl rescan) — used
// to decorate message nudges and inbox headers with a friendly name.
func titleFor(convID string) string {
	row, _ := db.GetConvIndex(convID)
	if row != nil {
		return displayTitle(row)
	}
	return ""
}

func short(convID string) string {
	if len(convID) >= 8 {
		return convID[:8]
	}
	return convID
}

// shortAgentID is the narrow-table form of a stable agent_id: the `agt_`
// tag plus the first 8 hex of the suffix (12 chars). The full id is the
// canonical, copy-pasteable handle (shown in JSON / single-value outputs);
// this is the superficial display form used only where a wide column would
// wreck a multi-column table. Falls back to the conv-id prefix when the row
// carries no agent_id (a resolved candidate that isn't an agent).
func shortAgentID(agentID, convID string) string {
	if agentID == "" {
		return short(convID)
	}
	if len(agentID) >= 12 {
		return agentID[:12]
	}
	return agentID
}

// ShortAgentID is the exported form of shortAgentID, for daemon-side
// callers (e.g. the inbox/outbox list columns) that render a message's
// actor in a wide table by its stable short id.
func ShortAgentID(agentID, convID string) string {
	return shortAgentID(agentID, convID)
}

// nudgeSenderNameMax bounds the (mutable) sender title injected into a tmux
// nudge, so a pathologically long /rename can't blow up the bracketed line.
// The stable short agent_id that follows is already length-bounded.
const nudgeSenderNameMax = 32

// MessageSenderLabel renders a message's sender for an incoming-message
// tmux nudge as "name (agt_xxxxxxxx)" — the (truncated) current title plus
// the stable short agent_id. The agent_id is the durable, rotation-immune
// handle (JOH-27): unlike the old conv-id prefix it is safe to surface,
// since it does not become stale on the sender's next reincarnate/clear.
// The title is a friendly decoration, truncated so it can't dominate the
// line. Falls back to the bare short id when the sender has no indexed
// title, so the label is never empty.
func MessageSenderLabel(fromConv, fromAgent string) string {
	id := shortAgentID(fromAgent, fromConv)
	name := truncate(sanitizeNudgeTitle(TitleFor(fromConv)), nudgeSenderNameMax)
	if name == "" {
		return id
	}
	return name + " (" + id + ")"
}

// sanitizeNudgeTitle strips control characters from a title before it is
// interpolated into a send-keys nudge. send-keys is an injection sink:
// CustomTitle is charset-gated at the /rename boundary, but the Summary /
// FirstPrompt fallbacks displayTitle can return are NOT — a freshly spawned
// agent that messages before its /rename lands carries a (often multi-line)
// spawn brief as its title, and a raw newline would submit a premature Enter
// in the recipient's pane. Control runes collapse to a space; surrounding
// space is trimmed. Length is bounded separately by truncate().
func sanitizeNudgeTitle(s string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s))
}

// onlineMark returns the single-cell glyph used in agent ls / groups
// members tables to flag online (live tmux session) conversations.
func onlineMark(online bool) string {
	if online {
		return "●"
	}
	return ""
}
