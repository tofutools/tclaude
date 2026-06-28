package agent

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"unicode"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/common"
)

// nudgeFn is a seam so tests can stub the tmux side-effect.
type nudgeFn func(tmuxSession, msg string) error

// defaultNudge mirrors task/run.go's send pattern: a textual line followed
// by Enter. It targets pane :0.0 — the same pane CC's input box lives in.
func defaultNudge(tmuxSession, msg string) error {
	target := tmuxSession + ":0.0"
	if err := clcommon.TmuxCommand("send-keys", "-t", target, msg, "Enter").Run(); err != nil {
		return err
	}
	return nil
}

// messageDeps lets tests override DB lookups + the tmux nudge. Production
// path uses the package-level functions directly.
type messageDeps struct {
	nudge nudgeFn
}

type messageParams struct {
	Target  string   `pos:"true" help:"Target conv (UUID/prefix/title), 'group:<name|id>' to broadcast, or bare 'group:' for your own group"`
	Body    string   `pos:"true" optional:"true" help:"Message body (or use --stdin / --file)"`
	Subject string   `long:"subject" short:"s" optional:"true" help:"Optional subject line"`
	Stdin   bool     `long:"stdin" help:"Read body from stdin"`
	File    string   `long:"file" short:"f" optional:"true" help:"Read body from a file ('-' reads stdin). Sidesteps shell quoting — best for long, multi-line, or backtick-containing bodies (the shell eats backticks from an inline body)."`
	Cc      []string `long:"cc" optional:"true" help:"CC recipient (title / conv-id / 8+-char prefix). Repeatable. Each gets its own row + nudge; the To and CC audience appears on every recipient's view."`
	Role    string   `long:"role" optional:"true" help:"With a 'group:' target, broadcast only to members holding this role (case-insensitive). Error on a 1:1 target."`
}

func messageCmd() *cobra.Command {
	return boa.CmdT[messageParams]{
		Use:     "message",
		Aliases: []string{"msg", "send"},
		Short:   "Send a message to another agent (or 'group:<name|id>' to broadcast)",
		Long: "Persists the message in SQLite and, if the target has a live tmux session, injects a `[system: …]` nudge so the receiving agent sees it on its next turn. " +
			"A target prefixed with 'group:' fans out: one row per non-sender member, nudging each one online. The group can be named ('group:reviewers'), given by numeric id ('group:7'), or left empty ('group:') to mean your own group — the latter is an error unless you are a member of exactly one group. " +
			"The sender must be a member or owner of the group to broadcast. Add --role to broadcast only to members holding a given role.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *messageParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Target).SetAlternativesFunc(completeMessageTargets)
			boa.GetParamT(ctx, &p.Cc).SetAlternativesFunc(completeConvSelectors)
			boa.GetParamT(ctx, &p.Role).SetAlternativesFunc(completeRoles)
			return nil
		},
		RunFunc: func(p *messageParams, _ *cobra.Command, _ []string) {
			os.Exit(runMessage(p, &messageDeps{nudge: defaultNudge}, os.Stdout, os.Stderr, os.Stdin))
		},
	}.ToCobra()
}

func runMessage(p *messageParams, d *messageDeps, stdout, stderr io.Writer, stdin io.Reader) int {
	// --role only narrows a group: multicast. On a 1:1 target there is
	// no member set to filter — fail fast with a clear message before
	// the daemon round-trip (the daemon enforces the same rule).
	if p.Role != "" && !strings.HasPrefix(p.Target, "group:") {
		fmt.Fprintf(stderr, "Error: --role is only valid with a 'group:' multicast target\n")
		return rcInvalidArg
	}
	body, status := readBody(p, stdin, stderr)
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
	_ = d // direct path is exercised by tests via runMessageDirect.
	return runMessageDaemon(p, body, stdout, stderr)
}

func runMessageDaemon(p *messageParams, body string, stdout, stderr io.Writer) int {
	var resp struct {
		ID             int64  `json:"id,omitempty"`
		Delivered      bool   `json:"delivered,omitempty"`
		Held           bool   `json:"held,omitempty"`
		ViaGroup       string `json:"via_group"`
		RedirectedFrom string `json:"redirected_from,omitempty"`
		Recipients     []struct {
			ConvID         string `json:"conv_id"`
			AgentID        string `json:"agent_id,omitempty"`
			Title          string `json:"title,omitempty"`
			MessageID      int64  `json:"message_id"`
			Delivered      bool   `json:"delivered"`
			Held           bool   `json:"held,omitempty"`
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
	err := DaemonPost("/v1/messages", payload, &resp)
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
		delivered := 0
		held := 0
		for _, rcp := range resp.Recipients {
			if rcp.Delivered {
				delivered++
			} else if rcp.Held {
				held++
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
		// "held" (recipient blocked on a human) is a distinct bucket from
		// "queued" (recipient offline); only print it when it happened so
		// the common line stays terse.
		heldNote := ""
		if held > 0 {
			heldNote = fmt.Sprintf(", %d held (awaiting human input)", held)
		}
		fmt.Fprintf(stdout, "%s%s: %d recipients (%d delivered, %d queued%s).\n",
			header, scope, len(resp.Recipients), delivered, len(resp.Recipients)-delivered-held, heldNote)
		for _, rcp := range resp.Recipients {
			name := rcp.Title
			if name == "" {
				name = "(unnamed)"
			}
			state := "queued"
			if rcp.Delivered {
				state = "delivered"
			} else if rcp.Held {
				state = "held (awaiting human input)"
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
		return rcOK
	}
	state := "queued; target not online"
	if resp.Delivered {
		state = "delivered"
	} else if resp.Held {
		// The recipient is alive but blocked on a human (a permission
		// prompt or elicitation dialog). We deliberately did NOT nudge —
		// the keystrokes would be captured as the human's answer. The row
		// is in their mailbox and delivers once they are back to working.
		state = "held; recipient is waiting on human input — placed in their mailbox, " +
			"delivers when they resume"
	}
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

func runMessageDirect(p *messageParams, d *messageDeps, body string, stdout, stderr io.Writer) int {
	fromID, err := currentConvID()
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcNotFound
	}

	target, matches, err := resolveSelector(p.Target)
	if errors.Is(err, errAmbiguous) {
		printAmbiguous(stderr, p.Target, matches)
		return rcAmbiguous
	}
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcNotFound
	}
	if target.ConvID == fromID {
		fmt.Fprintf(stderr, "Error: cannot message self\n")
		return rcInvalidArg
	}

	// Authorisation: must share at least one group.
	shared, err := db.SharedGroupsForConvs(fromID, target.ConvID)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcIOFailure
	}
	if len(shared) == 0 {
		fmt.Fprintf(stderr, "Error: not in a shared group with target %s\n", short(target.ConvID))
		return rcAuth
	}
	via := shared[0] // first by name; deterministic per design

	// Persist.
	id, err := db.InsertAgentMessage(&db.AgentMessage{
		GroupID:  via.ID,
		FromConv: fromID,
		ToConv:   target.ConvID,
		Subject:  p.Subject,
		Body:     body,
	})
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcIOFailure
	}

	// Nudge target's tmux pane if it has a live session.
	delivered := false
	if d != nil && d.nudge != nil {
		if sess, err := db.FindSessionByConvID(target.ConvID); err == nil && sess != nil && sess.TmuxSession != "" {
			if session.IsTmuxSessionAlive(sess.TmuxSession) {
				fromAgent, _ := db.AgentIDForConv(fromID)
				sender := MessageSenderLabel(fromID, fromAgent)
				replySel := shortAgentID(fromAgent, fromID)
				subjectClause := ""
				if p.Subject != "" {
					subjectClause = fmt.Sprintf(" subject=%q", p.Subject)
				}
				nudge := fmt.Sprintf(
					"[system: new agent message #%d from %s in group %q.%s read with: tclaude agent inbox read %d. reply with: tclaude agent message %s \"...\".]",
					id, sender, via.Name, subjectClause, id, replySel,
				)
				if err := d.nudge(sess.TmuxSession, nudge); err != nil {
					slog.Warn("failed to nudge target tmux session", "error", err, "session", sess.TmuxSession, "module", "agent")
				} else {
					delivered = true
					if err := db.MarkAgentMessageDelivered(id); err != nil {
						slog.Warn("failed to record delivered_at", "error", err, "msg_id", id, "module", "agent")
					}
				}
			}
		}
	}

	if delivered {
		fmt.Fprintf(stdout, "Sent message #%d to %s via group %q (delivered)\n", id, short(target.ConvID), via.Name)
	} else {
		fmt.Fprintf(stdout, "Sent message #%d to %s via group %q (queued; target not online)\n", id, short(target.ConvID), via.Name)
	}
	return rcOK
}

func readBody(p *messageParams, stdin io.Reader, stderr io.Writer) (string, int) {
	count := 0
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
		fmt.Fprintf(stderr, "Error: provide a body, --stdin, or --file\n")
		return "", rcInvalidArg
	}
	if count > 1 {
		fmt.Fprintf(stderr, "Error: pass only one of body / --stdin / --file\n")
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
	}
	return p.Body, rcOK
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
