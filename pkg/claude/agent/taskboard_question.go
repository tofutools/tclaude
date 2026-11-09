package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

// Questions for the operator (tk_aa049a67, tk_8b2c86f7): ask-operator puts
// a question to the operator on a task of the board (an entry of its
// activity) or on a new task made for it; answer answers it.

// rcAskTimeout is ask-operator --wait's exit code when the wait ran out
// before an answer; the question stays open and the answer still comes by
// mail.
const rcAskTimeout = 6

// taskboardQuestion is the wire shape of a question on a task.
type taskboardQuestion struct {
	Kind    string `json:"kind,omitempty"`
	N       int    `json:"n,omitempty"`
	Title   string `json:"title,omitempty"`
	Body    string `json:"body,omitempty"`
	Multi   bool   `json:"multi,omitempty"`
	Options []struct {
		Key         string `json:"key"`
		Label       string `json:"label"`
		Description string `json:"description,omitempty"`
		Preview     string `json:"preview,omitempty"`
	} `json:"options,omitempty"`
	Recommend string `json:"recommend,omitempty"`
	AskedBy   string `json:"asked_by,omitempty"`
	State     string `json:"state,omitempty"`
	Blocks    bool   `json:"blocks,omitempty"`
	Waiting   bool   `json:"waiting,omitempty"`
	// AttachmentIDs are the task's files the question shows the operator.
	AttachmentIDs []int64 `json:"attachment_ids,omitempty"`
	Answer        *struct {
		Picked     []string `json:"picked"`
		Text       string   `json:"text,omitempty"`
		AnsweredBy string   `json:"answered_by"`
		AnsweredAt string   `json:"answered_at"`
	} `json:"answer,omitempty"`
}

// open reports whether the question waits on its answer. A node from
// before per-task questions sends no state: open until answered.
func (q *taskboardQuestion) open() bool {
	if q.State != "" {
		return q.State == "open"
	}
	return q.Answer == nil
}

// answerLines is the answer, readable: the picked labels, then the text.
func (q *taskboardQuestion) answerLines() []string {
	if q == nil || q.Answer == nil {
		return nil
	}
	var out []string
	for _, k := range q.Answer.Picked {
		for _, o := range q.Options {
			if o.Key == k {
				out = append(out, "picked: "+o.Label)
			}
		}
	}
	if q.Answer.Text != "" {
		out = append(out, "said:   "+q.Answer.Text)
	}
	return out
}

// questionHow is how the question is answered.
func (q *taskboardQuestion) questionHow() string {
	switch {
	case len(q.Options) == 0:
		return "answer in words"
	case q.Multi:
		return "pick any"
	}
	return "pick one"
}

// printTaskboardQuestion is show's questions block: each question, its
// options and, once answered, its answer.
func printTaskboardQuestion(stdout io.Writer, t taskboardTask) {
	qs := t.Questions
	if len(qs) == 0 && t.Question != nil {
		qs = []*taskboardQuestion{t.Question}
	}
	for _, q := range qs {
		head := "  question"
		if q.N > 0 {
			head = fmt.Sprintf("  Q%d", q.N)
		}
		state := q.State
		if state == "" {
			state = "open"
			if q.Answer != nil {
				state = "answered"
			}
		}
		title := ""
		if q.Title != "" && q.Title != t.Title {
			title = " " + q.Title
		}
		fmt.Fprintf(stdout, "%s [%s]%s  (%s", head, state, title, q.questionHow())
		if q.AskedBy != "" {
			fmt.Fprintf(stdout, "; asked by %s", q.AskedBy)
		}
		fmt.Fprintln(stdout, ")")
		if q.Body != "" {
			fmt.Fprintln(stdout, indentText(q.Body, "      "))
		}
		if len(q.AttachmentIDs) > 0 {
			fmt.Fprintf(stdout, "    files: %s\n", questionFileNames(q, t.Attachments))
		}
		printQuestionOptions(stdout, q, "    ")
		if lines := q.answerLines(); len(lines) > 0 {
			fmt.Fprintf(stdout, "    answered by %s at %s\n", q.Answer.AnsweredBy, q.Answer.AnsweredAt)
			for _, l := range lines {
				fmt.Fprintf(stdout, "      %s\n", l)
			}
		} else if q.open() && (t.Status == "open" || t.Status == "blocked") {
			which := ""
			if q.N > 0 && t.OpenQuestions > 1 {
				which = fmt.Sprintf(" --question %d", q.N)
			}
			fmt.Fprintf(stdout, "    answer it: tclaude agent taskboard answer %s%s --pick <key> [--text \"…\"]\n", t.ID, which)
		}
	}
}

func printQuestionOptions(stdout io.Writer, q *taskboardQuestion, indent string) {
	for i, o := range q.Options {
		rec := ""
		if o.Key == q.Recommend {
			rec = "  (recommended)"
		}
		fmt.Fprintf(stdout, "%s%d. %s [%s]%s\n", indent, i+1, o.Label, o.Key, rec)
		if o.Description != "" {
			fmt.Fprintf(stdout, "%s   %s\n", indent, o.Description)
		}
	}
}

// ---- ask-operator ----

type taskboardAskOperatorParams struct {
	Target      string   `pos:"true" help:"A task (tk_…) to ask on, or a group: a new task in the operator's lane is made for the question"`
	Question    string   `pos:"true" help:"The question, one line"`
	Body        string   `long:"body" optional:"true" help:"Context for the operator (Markdown, up to 8 KiB)"`
	BodyFile    string   `long:"body-file" optional:"true" help:"Read the context from this file ('-' for stdin)"`
	Option      []string `long:"option" collection:"array" short:"o" optional:"true" help:"A choice: \"Label\" or \"Label: description\" (split at the first colon and space; repeat, 2 to 6). Without options the operator answers in words"`
	OptionsFile string   `long:"options-file" optional:"true" help:"The options as JSON: [{\"label\",\"description\",\"preview\",\"key\"}…] (a preview is monospace text, up to 2 KiB); not with --option"`
	Multi       bool     `long:"multi" help:"The operator may pick several options"`
	Recommend   string   `long:"recommend" optional:"true" help:"The option you recommend: its number (from 1), key or label"`
	Block       bool     `long:"block" help:"On a task: block it until the question is answered (or withdrawn)"`
	Blocks      string   `long:"blocks" optional:"true" help:"For a group: a task of the board (tk_…, typically yours) that waits on the new question task, blocked until it is answered"`
	Wait        string   `long:"wait" optional:"true" help:"Wait this long for the answer (e.g. 10m) and print it; exit 6 if it does not come in time (the question stays open, the answer still comes by mail)"`
	Idea        bool     `long:"idea" help:"For a group: put the question task in the operator's ideas, not waiting on them (not with --blocks or --wait)"`
	Attach      []string `long:"attach" collection:"array" optional:"true" help:"Show the operator a file with the question (a screenshot, a mock): it is attached to the task and previewed in the answer dialog. Repeatable; at most 10, 4 MiB each. Never paste a local path into the body instead: the operator may be on another device"`
	Existing    []string `long:"attach-existing" collection:"array" optional:"true" help:"On a task: show the operator a file already on it with the question, by number (#13) or name (repeatable)"`
	Peer        string   `long:"peer" optional:"true" help:"Ask the operator of a linked node, on its board, under the grants it gave this node"`
	AskHuman    string   `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s'). Capped at 300s. Timeout = deny."`
	JSON        bool     `long:"json" help:"Output JSON"`
}

func taskboardAskOperatorCmd() *cobra.Command {
	return boa.CmdT[taskboardAskOperatorParams]{
		Use:   "ask-operator",
		Short: "Ask the operator a question on a task (or on a new one): options to pick and/or free text; the answer comes back to you",
		Long: "Puts a question to the operator. On a task (tk_…), the question is an entry of the task's activity, " +
			"numbered Q1, Q2…, and shows in the operator's lane until it is answered; the task keeps its assignee. " +
			"--block blocks the task until the answer. On a group, a new task in the operator's lane is made for the " +
			"question, and answering it closes that task as done (--blocks makes your task wait on it). Up to 6 " +
			"--option choices (or none, for an answer in words), --multi to allow several, --recommend to mark the one " +
			"you suggest. The operator answers in the dashboard or with `taskboard answer`; the answer comes to your " +
			"inbox as `[taskboard] answered…` with the picks, the text and the answer as JSON, and is recorded under " +
			"the question in the task's activity. --wait holds the command until the answer comes and prints it. " +
			"To show the operator an image or any file, attach it with --attach (or name one already on the task " +
			"with --attach-existing): the answer dialog previews it with the question. A local file path in the body " +
			"is useless to the operator, who may be on another device. " +
			"Withdraw an open question by deleting its entry (`taskboard comment rm <task> <#id>`). Asking on your own " +
			"group's board needs taskboard.own. The board stays quiet: no popup, no Messages entry.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *taskboardAskOperatorParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Target).SetAlternativesFunc(completeGroupNames)
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *taskboardAskOperatorParams, _ *cobra.Command, _ []string) {
			taskboardExit(runTaskboardAskOperator(p, os.Stdin, cliStdout, cliStderr))
		},
	}.ToCobra()
}

// parseQuestionOptions reads --option ("Label" or "Label: description") or
// --options-file into the question's options.
func parseQuestionOptions(opts []string, file string) ([]map[string]any, error) {
	if len(opts) > 0 && file != "" {
		return nil, fmt.Errorf("give --option or --options-file, not both")
	}
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		var out []map[string]any
		if err := json.Unmarshal(b, &out); err != nil {
			return nil, fmt.Errorf("--options-file: %w", err)
		}
		return out, nil
	}
	out := []map[string]any{}
	for _, o := range opts {
		label, desc, _ := strings.Cut(o, ": ")
		m := map[string]any{"label": strings.TrimSpace(label)}
		if d := strings.TrimSpace(desc); d != "" {
			m["description"] = d
		}
		out = append(out, m)
	}
	return out, nil
}

// isTaskRef reports whether s names a task (tk_…) rather than a group.
func isTaskRef(s string) bool { return strings.HasPrefix(strings.TrimSpace(s), "tk_") }

func runTaskboardAskOperator(p *taskboardAskOperatorParams, stdin io.Reader, stdout, stderr io.Writer) int {
	var wait time.Duration
	if s := strings.TrimSpace(p.Wait); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil || d <= 0 {
			fmt.Fprintln(stderr, "Error: --wait is a duration such as 10m")
			return rcInvalidArg
		}
		wait = d
	}
	onTask := isTaskRef(p.Target)
	switch {
	case onTask && (p.Blocks != "" || p.Idea):
		fmt.Fprintln(stderr, "Error: on a task, --blocks and --idea do not apply (--block blocks the task itself)")
		return rcInvalidArg
	case !onTask && len(p.Existing) > 0:
		fmt.Fprintln(stderr, "Error: --attach-existing names a file already on the task; a new question task has none: use --attach")
		return rcInvalidArg
	case !onTask && p.Block:
		fmt.Fprintln(stderr, "Error: --block is for a question on a task; for a group, --blocks names the task that waits")
		return rcInvalidArg
	case p.Idea && (p.Blocks != "" || wait > 0):
		fmt.Fprintln(stderr, "Error: an idea for the operator is not waited on; --idea takes no --blocks or --wait")
		return rcInvalidArg
	}
	body, _, err := readTaskBody(p.Body, p.BodyFile, stdin)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcInvalidArg
	}
	if len(p.Attach) == 0 && len(p.Existing) == 0 {
		if path := localImagePath(body); path != "" {
			fmt.Fprintf(stderr, "Warning: the body names a local image (%s), which the operator cannot open from the dashboard "+
				"(they may be on another device). Attach it instead: --attach %s\n", path, path)
		}
	}
	opts, err := parseQuestionOptions(p.Option, p.OptionsFile)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcInvalidArg
	}
	q := map[string]any{"kind": "question", "multi": p.Multi, "options": opts}
	if r := strings.TrimSpace(p.Recommend); r != "" {
		var n int
		if _, err := fmt.Sscanf(r, "%d", &n); err == nil && fmt.Sprint(n) == r {
			if n < 1 || n > len(opts) {
				fmt.Fprintf(stderr, "Error: --recommend %d: there are %d options\n", n, len(opts))
				return rcInvalidArg
			}
			r, _ = opts[n-1]["label"].(string)
			if k, ok := opts[n-1]["key"].(string); ok && k != "" {
				r = k
			}
		}
		q["recommend"] = r
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	ask, rc := parseAskHumanFlag(p.AskHuman, stdout, stderr)
	if rc != rcOK {
		return rc
	}
	if onTask {
		return askOperatorOnTask(p, q, body, wait, ask, stdout, stderr)
	}
	req := map[string]any{"title": p.Question, "body": body, "to": "operator", "question": q}
	if b := strings.TrimSpace(p.Blocks); b != "" {
		req["blocks"] = b
	}
	if p.Idea {
		req["start"], req["stage"] = false, "idea"
	}
	path := "/v1/groups/" + url.PathEscape(strings.TrimSpace(p.Target)) + "/taskboard/tasks"
	var t taskboardTask
	if len(p.Attach) > 0 {
		err = taskboardUploadFiles(p.Peer, path, req, p.Attach, &t, ask)
	} else {
		err = remoteRequestOpts(p.Peer, http.MethodPost, path, req, &t, DaemonOpts{AskHuman: ask})
	}
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if wait == 0 {
		if p.JSON {
			return writeJSONTo(stdout, t)
		}
		fmt.Fprintf(stdout, "asked %s: %s  [%s]; %s\n", t.ID, t.Title, t.Stage, answerWhere(p.Peer))
		return rcOK
	}
	if !p.JSON {
		fmt.Fprintf(stdout, "asked %s: %s; waiting up to %s for the answer…\n", t.ID, t.Title, wait)
	}
	// Its question by number; a node from before per-task questions sends
	// none, and is waited on through the question task's own route.
	n := 0
	if t.Question != nil {
		n = t.Question.N
	}
	return waitTaskboardAnswer(p.Peer, t.ID, n, wait, p.JSON, stdout, stderr)
}

// questionFileNames names the files a question shows, "#13 a.png, #14
// b.png", from the task's files; one detached since shows by number.
func questionFileNames(q *taskboardQuestion, files []taskboardAttachment) string {
	names := make([]string, 0, len(q.AttachmentIDs))
	for _, id := range q.AttachmentIDs {
		name := fmt.Sprintf("#%d", id)
		for _, f := range files {
			if f.ID == id {
				name += " " + f.Name
			}
		}
		names = append(names, name)
	}
	return strings.Join(names, ", ")
}

// filesNote says how many files a question shows, as " (2 files)".
func filesNote(q *taskboardQuestion) string {
	switch n := len(q.AttachmentIDs); n {
	case 0:
		return ""
	case 1:
		return " (1 file)"
	default:
		return fmt.Sprintf(" (%d files)", n)
	}
}

// localImagePathRE finds an absolute or home-relative path to an image
// file in a question's body; a URL's "//" is no path.
var localImagePathRE = regexp.MustCompile(`(?i)(?:^|[\s(\[<"'` + "`" + `=:])((?:/[^/\s]|~/|[A-Z]:\\)[^\s()\[\]<>"'` + "`" + `]*\.(?:png|jpe?g|gif|webp|svg|bmp|avif))\b`)

// localImagePath is the first local image path body names, or "": a path
// the operator cannot open from the dashboard (ask-operator warns).
func localImagePath(body string) string {
	if m := localImagePathRE.FindStringSubmatch(body); m != nil {
		return m[1]
	}
	return ""
}

// taskboardAsked is POST …/questions's answer.
type taskboardAsked struct {
	Task     taskboardTask      `json:"task"`
	Question *taskboardQuestion `json:"question"`
}

// askOperatorOnTask asks on an existing task: an entry of its activity.
func askOperatorOnTask(p *taskboardAskOperatorParams, q map[string]any, body string, wait, ask time.Duration, stdout, stderr io.Writer) int {
	q["title"], q["body"], q["block"] = p.Question, body, p.Block
	if len(p.Existing) > 0 {
		q["attach_existing"] = p.Existing
	}
	var out taskboardAsked
	var err error
	if path := taskboardPath(p.Target, "/questions"); len(p.Attach) > 0 {
		err = taskboardUploadFiles(p.Peer, path, q, p.Attach, &out, ask)
	} else {
		err = remoteRequestOpts(p.Peer, http.MethodPost, path, q, &out, DaemonOpts{AskHuman: ask})
	}
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if out.Question == nil || out.Question.N < 1 {
		fmt.Fprintln(stderr, "Error: the daemon did not return the question")
		return rcIOFailure
	}
	if wait == 0 {
		if p.JSON {
			return writeJSONTo(stdout, out)
		}
		blocked := ""
		if p.Block {
			blocked = "; the task is blocked until the answer"
		}
		fmt.Fprintf(stdout, "asked Q%d on %s: %s%s%s; %s\n", out.Question.N, out.Task.ID, out.Question.Title, filesNote(out.Question), blocked, answerWhere(p.Peer))
		return rcOK
	}
	if !p.JSON {
		fmt.Fprintf(stdout, "asked Q%d on %s: %s; waiting up to %s for the answer…\n", out.Question.N, out.Task.ID, out.Question.Title, wait)
	}
	return waitTaskboardAnswer(p.Peer, out.Task.ID, out.Question.N, wait, p.JSON, stdout, stderr)
}

// answerWhere says where the answer to a question comes: the inbox, also
// from a linked node's board (as federation mail).
func answerWhere(peer string) string {
	if peer == "" {
		return "the answer comes to your inbox"
	}
	return "the answer comes to your inbox as remote mail from " + peer
}

// taskboardAnswerState is GET …/questions/{n} (or …/answer).
type taskboardAnswerState struct {
	Task     string             `json:"task"`
	N        int                `json:"n,omitempty"`
	Status   string             `json:"status"`
	State    string             `json:"state,omitempty"`
	Answered bool               `json:"answered"`
	Question *taskboardQuestion `json:"question"`
}

// questionPath is question n's route, or with n 0 the task's question
// route (its only open question).
func questionPath(id string, n int) string {
	if n <= 0 {
		return taskboardPath(id, "/answer")
	}
	return taskboardPath(id, "/questions/"+strconv.Itoa(n))
}

// waitTaskboardAnswer long-polls question n (0: a question task's) until
// it is answered or closed, or wait runs out.
func waitTaskboardAnswer(peer, id string, n int, wait time.Duration, asJSON bool, stdout, stderr io.Writer) int {
	deadline := time.Now().Add(wait)
	for {
		left := time.Until(deadline)
		if left <= 0 {
			fmt.Fprintf(stderr, "no answer within %s; the question on %s stays open: %s\n", wait, id, answerWhere(peer))
			return rcAskTimeout
		}
		step := min(left, 30*time.Second).Round(time.Second)
		if step <= 0 {
			step = time.Second
		}
		var st taskboardAnswerState
		// The daemon holds the request for up to step: the client budget
		// is step plus headroom, not the default 10s.
		opts := DaemonOpts{Timeout: step + 15*time.Second}
		if err := remoteRequestOpts(peer, http.MethodGet, questionPath(id, n)+"?wait="+step.String(), nil, &st, opts); err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return MapDaemonErrorToRC(err)
		}
		ended := st.State != "" && st.State != "open"
		if st.Answered || ended || (st.Status != "open" && st.Status != "blocked") {
			if asJSON {
				return writeJSONTo(stdout, st)
			}
			if !st.Answered {
				what := st.State
				if what == "" || what == "open" {
					what = st.Status
				}
				fmt.Fprintf(stdout, "the question on %s was %s without an answer\n", id, what)
				return rcOK
			}
			for _, l := range st.Question.answerLines() {
				fmt.Fprintln(stdout, l)
			}
			return rcOK
		}
	}
}

// ---- answer ----

type taskboardAnswerParams struct {
	Task     string `pos:"true" help:"Task id (tk_…) the question is on"`
	Question string `long:"question" short:"q" optional:"true" help:"Which question (Q2 or 2); needed only when the task has several open"`
	Pick     string `long:"pick" optional:"true" help:"The option(s) picked: key, label or number (from 1), comma-separated for a multi-select question"`
	Text     string `long:"text" optional:"true" help:"An answer in words, with the pick or instead of one"`
	Peer     string `long:"peer" optional:"true" help:"Answer on a linked node's board, under the grants it gave this node (taskboard.assign)"`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s'). Capped at 300s. Timeout = deny."`
	JSON     bool   `long:"json" help:"Output JSON"`
}

func taskboardAnswerCmd() *cobra.Command {
	return boa.CmdT[taskboardAnswerParams]{
		Use:   "answer",
		Short: "Answer a question put to the operator: the answer goes to the agent that asked",
		Long: "Answers a question on a task (`ask-operator`): --pick names the option(s), --text answers in words, with a " +
			"pick or instead of one, and --question names the question when the task has several open. The answer is " +
			"recorded under the question in the task's activity and goes to the agent that asked by mail; a task the " +
			"question blocked is unblocked, and a task made for the question closes as done (the tasks waiting on it " +
			"reopen). Answering is the operator's, or a taskboard.assign holder's; never the asking agent's.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *taskboardAnswerParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *taskboardAnswerParams, _ *cobra.Command, _ []string) {
			taskboardExit(runTaskboardAnswer(p, cliStdout, cliStderr))
		},
	}.ToCobra()
}

// parseQuestionN reads --question: "", "2" or "Q2".
func parseQuestionN(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(strings.TrimPrefix(strings.TrimPrefix(s, "Q"), "q"))
	if err != nil || n < 1 {
		return 0, fmt.Errorf("--question %q: name it Q2 or 2", s)
	}
	return n, nil
}

func runTaskboardAnswer(p *taskboardAnswerParams, stdout, stderr io.Writer) int {
	picks := splitTaskIDs(p.Pick)
	if len(picks) == 0 && strings.TrimSpace(p.Text) == "" {
		fmt.Fprintln(stderr, "Error: give --pick, --text, or both")
		return rcInvalidArg
	}
	n, err := parseQuestionN(p.Question)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	// A number picks by position: read the question first.
	numbered := false
	for _, k := range picks {
		if v, err := strconv.Atoi(k); err == nil && strconv.Itoa(v) == k {
			numbered = true
		}
	}
	if numbered {
		var t taskboardTask
		if err := remoteRequest(p.Peer, http.MethodGet, taskboardPath(p.Task, ""), nil, &t); err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return MapDaemonErrorToRC(err)
		}
		q, msg := pickQuestion(t, n)
		if q == nil {
			fmt.Fprintf(stderr, "Error: %s\n", msg)
			return rcInvalidArg
		}
		for i, k := range picks {
			if v, err := strconv.Atoi(k); err == nil && strconv.Itoa(v) == k {
				if v < 1 || v > len(q.Options) {
					fmt.Fprintf(stderr, "Error: --pick %d: the question has %d options\n", v, len(q.Options))
					return rcInvalidArg
				}
				picks[i] = q.Options[v-1].Key
			}
		}
	}
	req := map[string]any{"picked": picks, "text": strings.TrimSpace(p.Text)}
	path := taskboardPath(p.Task, "/answer")
	if n > 0 {
		path = questionPath(p.Task, n) + "/answer"
	}
	return taskboardWrite(p.Peer, http.MethodPost, path, req, p.AskHuman, p.JSON, "answered", stdout, stderr)
}

// pickQuestion is the question an answer names: n, or with n 0 the task's
// only open question; else why not.
func pickQuestion(t taskboardTask, n int) (*taskboardQuestion, string) {
	qs := t.Questions
	if len(qs) == 0 && t.Question != nil {
		qs = []*taskboardQuestion{t.Question}
	}
	var open []*taskboardQuestion
	for _, q := range qs {
		if n > 0 && q.N == n {
			return q, ""
		}
		if q.open() {
			open = append(open, q)
		}
	}
	switch {
	case n > 0:
		return nil, fmt.Sprintf("%s has no question Q%d", t.ID, n)
	case len(qs) == 0:
		return nil, t.ID + " has no question"
	case len(open) == 1:
		return open[0], ""
	case len(open) == 0:
		return nil, t.ID + " has no open question"
	}
	return nil, fmt.Sprintf("%s has %d open questions; name one with --question", t.ID, len(open))
}
