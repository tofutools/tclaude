package agent

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

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
	Target  string `pos:"true" help:"Target conv: UUID, prefix, or current title"`
	Body    string `pos:"true" optional:"true" help:"Message body (or use --stdin / --file)"`
	Subject string `long:"subject" short:"s" help:"Optional subject line"`
	Stdin   bool   `long:"stdin" help:"Read body from stdin"`
	File    string `long:"file" short:"f" help:"Read body from a file"`
}

func messageCmd() *cobra.Command {
	return boa.CmdT[messageParams]{
		Use:         "message",
		Aliases:     []string{"msg", "send"},
		Short:       "Send a message to another agent in a shared group",
		Long:        "Persists the message in SQLite and, if the target has a live tmux session, injects a `[system: …]` nudge so the receiving agent sees it on its next turn.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *messageParams, _ *cobra.Command, _ []string) {
			os.Exit(runMessage(p, &messageDeps{nudge: defaultNudge}, os.Stdout, os.Stderr, os.Stdin))
		},
	}.ToCobra()
}

func runMessage(p *messageParams, d *messageDeps, stdout, stderr io.Writer, stdin io.Reader) int {
	body, status := readBody(p, stdin, stderr)
	if status != rcOK {
		return status
	}
	if strings.TrimSpace(body) == "" {
		fmt.Fprintf(stderr, "Error: message body is empty\n")
		return rcInvalidArg
	}

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
	if target.convID == fromID {
		fmt.Fprintf(stderr, "Error: cannot message self\n")
		return rcInvalidArg
	}

	// Authorisation: must share at least one group.
	shared, err := db.SharedGroupsForConvs(fromID, target.convID)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcIOFailure
	}
	if len(shared) == 0 {
		fmt.Fprintf(stderr, "Error: not in a shared group with target %s\n", short(target.convID))
		return rcAuth
	}
	via := shared[0] // first by name; deterministic per design

	// Persist.
	id, err := db.InsertAgentMessage(&db.AgentMessage{
		GroupID:  via.ID,
		FromConv: fromID,
		ToConv:   target.convID,
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
		if sess, err := db.FindSessionByConvID(target.convID); err == nil && sess != nil && sess.TmuxSession != "" {
			if session.IsTmuxSessionAlive(sess.TmuxSession) {
				fromAlias := aliasFor(via.ID, fromID)
				if fromAlias == "" {
					fromAlias = "(unnamed)"
				}
				subjectClause := ""
				if p.Subject != "" {
					subjectClause = fmt.Sprintf(" subject=%q", p.Subject)
				}
				nudge := fmt.Sprintf(
					"[system: new agent message #%d from %s (%s) in group %q.%s read with: tclaude agent inbox read %d. reply with: tclaude agent message %s \"...\".]",
					id, fromAlias, short(fromID), via.Name, subjectClause, id, short(fromID),
				)
				if err := d.nudge(sess.TmuxSession, nudge); err != nil {
					slog.Warn("failed to nudge target tmux session", "error", err, "session", sess.TmuxSession, "module", "agent")
				} else {
					delivered = true
					_ = db.MarkAgentMessageDelivered(id)
				}
			}
		}
	}

	if delivered {
		fmt.Fprintf(stdout, "Sent message #%d to %s via group %q (delivered)\n", id, short(target.convID), via.Name)
	} else {
		fmt.Fprintf(stdout, "Sent message #%d to %s via group %q (queued; target not online)\n", id, short(target.convID), via.Name)
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
	case p.File != "":
		data, err := os.ReadFile(p.File)
		if err != nil {
			fmt.Fprintf(stderr, "Error reading file: %v\n", err)
			return "", rcIOFailure
		}
		return string(data), rcOK
	}
	return p.Body, rcOK
}

// aliasFor returns the alias (or empty) recorded for (group, conv) — used
// in the nudge text so the receiver sees a friendly name.
func aliasFor(groupID int64, convID string) string {
	m, err := db.FindMemberInGroup(groupID, convID)
	if err != nil || m == nil {
		return ""
	}
	if m.Alias != "" {
		return m.Alias
	}
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
