package agent

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

// A task's comments and activity stream (docs/taskboard.md): `taskboard
// comment add|edit|rm|ls TASK` and `taskboard activity TASK` (`history` is
// its alias).

// taskboardComment is the daemon's wire shape for a comment.
type taskboardComment struct {
	ID          int64                 `json:"id"`
	Kind        string                `json:"kind"`
	Body        string                `json:"body"`
	Key         string                `json:"key,omitempty"`
	Author      string                `json:"author"`
	AuthorName  string                `json:"author_name,omitempty"`
	At          string                `json:"at"`
	EditedAt    string                `json:"edited_at,omitempty"`
	DeletedAt   string                `json:"deleted_at,omitempty"`
	Attachments []taskboardAttachment `json:"attachments,omitempty"`
}

// taskboardActivityEntry is one entry of a task's stream.
type taskboardActivityEntry struct {
	ID         int64  `json:"id,omitempty"`
	At         string `json:"at"`
	Actor      string `json:"actor"`
	ActorName  string `json:"actor_name,omitempty"`
	Kind       string `json:"kind"`
	Detail     string `json:"detail,omitempty"`
	DetailText string `json:"detail_text,omitempty"`
	Changes    []struct {
		Field  string `json:"field"`
		Before string `json:"before"`
		After  string `json:"after"`
	} `json:"changes,omitempty"`
	Action       string            `json:"action,omitempty"`
	Comment      *taskboardComment `json:"comment,omitempty"`
	CommentID    int64             `json:"comment_id,omitempty"`
	PreviousBody string            `json:"previous_body,omitempty"`
}

// actorText names an actor: its agent's name when known, the actor else.
func actorText(actor, name string) string {
	if name != "" {
		return name
	}
	return actor
}

// commentKindLabels name the typed comments a status change writes.
var commentKindLabels = map[string]string{
	"block_reason": "blocked because", "done_reason": "done because", "drop_reason": "dropped because",
	"report_note": "report note",
}

// indentText indents each line of s.
func indentText(s, indent string) string {
	return indent + strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n"+indent)
}

func printTaskboardComment(stdout io.Writer, c *taskboardComment) {
	head := fmt.Sprintf("  #%d  %s  %s", c.ID, c.At, actorText(c.Author, c.AuthorName))
	if label := commentKindLabels[c.Kind]; label != "" {
		head += "  (" + label + ")"
	}
	switch {
	case c.DeletedAt != "":
		head += "  [deleted " + c.DeletedAt + "]"
	case c.EditedAt != "":
		head += "  [edited " + c.EditedAt + "]"
	}
	fmt.Fprintln(stdout, head)
	if c.Body != "" {
		fmt.Fprintln(stdout, indentText(c.Body, "      "))
	}
	printTaskboardAttachments(stdout, "      ", c.Attachments)
}

// ---- activity ----

func taskboardActivityCmd() *cobra.Command {
	return boa.CmdT[taskboardShowParams]{
		Use:     "activity",
		Aliases: []string{"history"},
		Short:   "Show a task's activity: its comments and changes, oldest first",
		Long: "Show a task's activity stream, oldest first: comments (with close, drop, block and report reasons as " +
			"typed comments), field changes with their before and after, and actions such as attached, assigned or " +
			"regrouped. A failed or no-op operation records nothing. It needs taskboard.read on the task's group.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *taskboardShowParams, _ *cobra.Command, _ []string) {
			taskboardExit(runTaskboardActivity(p, cliStdout, cliStderr))
		},
	}.ToCobra()
}

func runTaskboardActivity(p *taskboardShowParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var resp struct {
		ID      string                   `json:"id"`
		Group   string                   `json:"group"`
		Title   string                   `json:"title"`
		Entries []taskboardActivityEntry `json:"entries"`
	}
	if err := remoteRequest(p.Peer, http.MethodGet, taskboardPath(p.Task, "/activity"), nil, &resp); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if p.JSON {
		return writeJSONTo(stdout, resp)
	}
	printRemoteScheduleBanner(stdout, p.Peer, "task activity")
	fmt.Fprintf(stdout, "%s  %s\n", resp.ID, resp.Title)
	for _, e := range resp.Entries {
		if e.Comment != nil {
			printTaskboardComment(stdout, e.Comment)
			continue
		}
		detail := e.Detail
		if e.DetailText != "" {
			detail = e.DetailText
		}
		if e.Kind == "comment_edited" {
			detail = fmt.Sprintf("comment #%d", e.CommentID)
		}
		line := fmt.Sprintf("  %s  %-10s  %s", e.At, e.Kind, actorText(e.Actor, e.ActorName))
		if detail != "" {
			line += "  " + detail
		}
		fmt.Fprintln(stdout, line)
		if e.Kind != "edited" {
			// Any other entry's detail already reads its change, agents
			// named; --json has every entry's changes.
			continue
		}
		for _, c := range e.Changes {
			if c.Field == "body" {
				fmt.Fprintf(stdout, "      body: changed\n")
				continue
			}
			fmt.Fprintf(stdout, "      %s: %q → %q\n", c.Field, c.Before, c.After)
		}
	}
	return rcOK
}

// ---- comment ----

func taskboardCommentCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:   "comment",
		Short: "Comment on a task: add, edit, rm, ls",
		Long: "Comments are Markdown, up to 16 KiB, and may carry files. Commenting needs taskboard.write, or " +
			"taskboard.own on your own group's board; reading needs taskboard.read. Only a comment's author edits or " +
			"deletes it: an edit keeps the earlier body in the task's activity, a delete leaves a tombstone. Someone " +
			"else's comment mails the task's assignee, and its creator when that is an agent.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			taskboardCommentAddCmd(),
			taskboardCommentEditCmd(),
			taskboardCommentRmCmd(),
			taskboardCommentLsCmd(),
		},
	}.ToCobra()
}

// readCommentBody is the comment's text: the argument, or --file (- for
// stdin), not both.
func readCommentBody(arg, file string) (string, error) {
	switch {
	case arg != "" && file != "":
		return "", errors.New("give the comment as an argument or with --file, not both")
	case file == "-":
		b, err := io.ReadAll(io.LimitReader(os.Stdin, 16<<10+1))
		return string(b), err
	case file != "":
		b, err := os.ReadFile(file)
		return string(b), err
	case arg == "":
		return "", errors.New("give the comment as an argument or with --file")
	}
	return arg, nil
}

// newCommentKey is a fresh idempotency key.
func newCommentKey() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return "cli-" + hex.EncodeToString(b)
}

type taskboardCommentAddParams struct {
	Task     string   `pos:"true" help:"Task id (tk_…)"`
	Body     string   `pos:"true" optional:"true" help:"The comment (Markdown); or use --file"`
	File     string   `long:"file" optional:"true" help:"Read the comment from this file (- for stdin)"`
	Key      string   `long:"key" optional:"true" help:"An idempotency key: the same key and body again returns the first comment, not a second (default: a fresh one)"`
	Attach   []string `long:"attach" optional:"true" help:"Files to add to the comment (at most 10, 4 MiB each)"`
	Peer     string   `long:"peer" optional:"true" help:"A task on a linked node's board, under the grants it gave this node (taskboard.write)"`
	AskHuman string   `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s'). Capped at 300s. Timeout = deny."`
	JSON     bool     `long:"json" help:"Output JSON"`
}

func taskboardCommentAddCmd() *cobra.Command {
	return boa.CmdT[taskboardCommentAddParams]{
		Use:         "add",
		Short:       "Comment on a task",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *taskboardCommentAddParams, _ *cobra.Command, _ []string) {
			taskboardExit(runTaskboardCommentAdd(p, cliStdout, cliStderr))
		},
	}.ToCobra()
}

type taskboardCommentAnswer struct {
	ID      string            `json:"id"`
	Group   string            `json:"group"`
	Comment *taskboardComment `json:"comment"`
}

func runTaskboardCommentAdd(p *taskboardCommentAddParams, stdout, stderr io.Writer) int {
	body, err := readCommentBody(p.Body, p.File)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	ask, rc := parseAskHumanFlag(p.AskHuman, stdout, stderr)
	if rc != rcOK {
		return rc
	}
	key := strings.TrimSpace(p.Key)
	if key == "" {
		key = newCommentKey()
	}
	req := map[string]any{"body": body, "key": key}
	path := taskboardPath(p.Task, "/comments")
	var out taskboardCommentAnswer
	if len(p.Attach) > 0 {
		err = taskboardUploadFiles(p.Peer, path, req, p.Attach, &out, ask)
	} else {
		err = remoteRequestOpts(p.Peer, http.MethodPost, path, req, &out, DaemonOpts{AskHuman: ask})
	}
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if p.JSON {
		return writeJSONTo(stdout, out)
	}
	if out.Comment == nil {
		fmt.Fprintf(stdout, "commented on %s\n", out.ID)
		return rcOK
	}
	fmt.Fprintf(stdout, "comment #%d on %s\n", out.Comment.ID, out.ID)
	printTaskboardAttachments(stdout, "  ", out.Comment.Attachments)
	return rcOK
}

type taskboardCommentEditParams struct {
	Task     string `pos:"true" help:"Task id (tk_…)"`
	Comment  string `pos:"true" help:"The comment's id (#N, as taskboard comment ls lists it)"`
	Body     string `pos:"true" optional:"true" help:"The new body (Markdown); or use --file"`
	File     string `long:"file" optional:"true" help:"Read the new body from this file (- for stdin)"`
	Peer     string `long:"peer" optional:"true" help:"A task on a linked node's board, under the grants it gave this node (taskboard.write)"`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s'). Capped at 300s. Timeout = deny."`
	JSON     bool   `long:"json" help:"Output JSON"`
}

func taskboardCommentEditCmd() *cobra.Command {
	return boa.CmdT[taskboardCommentEditParams]{
		Use:         "edit",
		Short:       "Edit your comment (the earlier body stays in the activity)",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *taskboardCommentEditParams, _ *cobra.Command, _ []string) {
			taskboardExit(runTaskboardCommentEdit(p, cliStdout, cliStderr))
		},
	}.ToCobra()
}

// commentPath is a comment's route, from #N or N.
func commentPath(task, ref string) (string, error) {
	id, err := strconv.ParseInt(strings.TrimPrefix(strings.TrimSpace(ref), "#"), 10, 64)
	if err != nil || id <= 0 {
		return "", fmt.Errorf("%q is not a comment id (#N)", ref)
	}
	return taskboardPath(task, "/comments/"+strconv.FormatInt(id, 10)), nil
}

func runTaskboardCommentEdit(p *taskboardCommentEditParams, stdout, stderr io.Writer) int {
	path, err := commentPath(p.Task, p.Comment)
	var body string
	if err == nil {
		body, err = readCommentBody(p.Body, p.File)
	}
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	ask, rc := parseAskHumanFlag(p.AskHuman, stdout, stderr)
	if rc != rcOK {
		return rc
	}
	var out taskboardCommentAnswer
	if err := remoteRequestOpts(p.Peer, http.MethodPatch, path, map[string]any{"body": body}, &out, DaemonOpts{AskHuman: ask}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if p.JSON {
		return writeJSONTo(stdout, out)
	}
	fmt.Fprintf(stdout, "edited comment %s on %s\n", p.Comment, out.ID)
	return rcOK
}

type taskboardCommentRmParams struct {
	Task     string `pos:"true" help:"Task id (tk_…)"`
	Comment  string `pos:"true" help:"The comment's id (#N)"`
	Peer     string `long:"peer" optional:"true" help:"A task on a linked node's board, under the grants it gave this node (taskboard.write)"`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s'). Capped at 300s. Timeout = deny."`
	JSON     bool   `long:"json" help:"Output JSON"`
}

func taskboardCommentRmCmd() *cobra.Command {
	return boa.CmdT[taskboardCommentRmParams]{
		Use:         "rm",
		Short:       "Delete your comment (a tombstone stays in the activity)",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *taskboardCommentRmParams, _ *cobra.Command, _ []string) {
			taskboardExit(runTaskboardCommentRm(p, cliStdout, cliStderr))
		},
	}.ToCobra()
}

func runTaskboardCommentRm(p *taskboardCommentRmParams, stdout, stderr io.Writer) int {
	path, err := commentPath(p.Task, p.Comment)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	ask, rc := parseAskHumanFlag(p.AskHuman, stdout, stderr)
	if rc != rcOK {
		return rc
	}
	var out taskboardCommentAnswer
	if err := remoteRequestOpts(p.Peer, http.MethodDelete, path, nil, &out, DaemonOpts{AskHuman: ask}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if p.JSON {
		return writeJSONTo(stdout, out)
	}
	fmt.Fprintf(stdout, "deleted comment %s on %s\n", p.Comment, out.ID)
	return rcOK
}

type taskboardCommentLsParams struct {
	Task string `pos:"true" help:"Task id (tk_…)"`
	Save string `long:"save" optional:"true" help:"Save the comments' files into this directory, one subdirectory per comment (never overwriting)"`
	Peer string `long:"peer" optional:"true" help:"A task on a linked node's board, under the grants it gave this node (taskboard.read)"`
	JSON bool   `long:"json" help:"Output JSON"`
}

func taskboardCommentLsCmd() *cobra.Command {
	return boa.CmdT[taskboardCommentLsParams]{
		Use:         "ls",
		Short:       "List a task's comments, or save their files with --save DIR",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *taskboardCommentLsParams, _ *cobra.Command, _ []string) {
			taskboardExit(runTaskboardCommentLs(p, cliStdout, cliStderr))
		},
	}.ToCobra()
}

func runTaskboardCommentLs(p *taskboardCommentLsParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var resp struct {
		ID       string              `json:"id"`
		Group    string              `json:"group"`
		Comments []*taskboardComment `json:"comments"`
	}
	if err := remoteRequest(p.Peer, http.MethodGet, taskboardPath(p.Task, "/comments"), nil, &resp); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if p.Save != "" {
		var saved []string
		for _, c := range resp.Comments {
			if len(c.Attachments) == 0 {
				continue
			}
			got, err := saveTaskboardAttachments(p.Peer, p.Task, filepath.Join(p.Save, "comment-"+strconv.FormatInt(c.ID, 10)), c.Attachments)
			saved = append(saved, got...)
			if err != nil {
				fmt.Fprintf(stderr, "Error: %v\n", err)
				var de *DaemonError
				if errors.As(err, &de) {
					return MapDaemonErrorToRC(err)
				}
				return rcIOFailure
			}
		}
		if p.JSON {
			return writeJSONTo(stdout, map[string]any{"task": resp.ID, "saved": saved})
		}
		for _, path := range saved {
			fmt.Fprintf(stdout, "saved %s\n", path)
		}
		return rcOK
	}
	if p.JSON {
		return writeJSONTo(stdout, resp)
	}
	printRemoteScheduleBanner(stdout, p.Peer, "task comments")
	if len(resp.Comments) == 0 {
		fmt.Fprintf(stdout, "%s has no comments\n", resp.ID)
		return rcOK
	}
	for _, c := range resp.Comments {
		printTaskboardComment(stdout, c)
	}
	return rcOK
}
