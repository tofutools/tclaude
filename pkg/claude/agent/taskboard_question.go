package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

// Question tasks (tk_aa049a67): ask-operator puts a question to the operator
// on the board, answer answers it.

// rcAskTimeout is ask-operator --wait's exit code when the wait ran out
// before an answer; the question stays open and the answer still comes by
// mail.
const rcAskTimeout = 6

// taskboardQuestion is the wire shape of a question task's question.
type taskboardQuestion struct {
	Kind    string `json:"kind,omitempty"`
	Multi   bool   `json:"multi,omitempty"`
	Options []struct {
		Key         string `json:"key"`
		Label       string `json:"label"`
		Description string `json:"description,omitempty"`
		Preview     string `json:"preview,omitempty"`
	} `json:"options,omitempty"`
	Recommend string `json:"recommend,omitempty"`
	Answer    *struct {
		Picked     []string `json:"picked"`
		Text       string   `json:"text,omitempty"`
		AnsweredBy string   `json:"answered_by"`
		AnsweredAt string   `json:"answered_at"`
	} `json:"answer,omitempty"`
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

// printTaskboardQuestion is show's question block.
func printTaskboardQuestion(stdout io.Writer, t taskboardTask) {
	q := t.Question
	if q == nil {
		return
	}
	pick := "pick one"
	if q.Multi {
		pick = "pick any"
	}
	if len(q.Options) == 0 {
		pick = "answer in words"
	}
	fmt.Fprintf(stdout, "  question: %s\n", pick)
	for i, o := range q.Options {
		rec := ""
		if o.Key == q.Recommend {
			rec = "  (recommended)"
		}
		fmt.Fprintf(stdout, "    %d. %s [%s]%s\n", i+1, o.Label, o.Key, rec)
		if o.Description != "" {
			fmt.Fprintf(stdout, "       %s\n", o.Description)
		}
	}
	if lines := q.answerLines(); len(lines) > 0 {
		fmt.Fprintf(stdout, "  answered by %s at %s\n", q.Answer.AnsweredBy, q.Answer.AnsweredAt)
		for _, l := range lines {
			fmt.Fprintf(stdout, "    %s\n", l)
		}
	} else if t.Status == "open" || t.Status == "blocked" {
		fmt.Fprintf(stdout, "  answer it: tclaude agent taskboard answer %s --pick <key> [--text \"…\"]\n", t.ID)
	}
}

// ---- ask-operator ----

type taskboardAskOperatorParams struct {
	Group       string   `pos:"true" help:"Group whose board the question goes on"`
	Question    string   `pos:"true" help:"The question, one line (the task's title)"`
	Body        string   `long:"body" optional:"true" help:"Context for the operator (Markdown, up to 8 KiB)"`
	BodyFile    string   `long:"body-file" optional:"true" help:"Read the context from this file ('-' for stdin)"`
	Option      []string `long:"option" short:"o" optional:"true" help:"A choice: \"Label\" or \"Label: description\" (split at the first colon and space; repeat, 2 to 6). Without options the operator answers in words"`
	OptionsFile string   `long:"options-file" optional:"true" help:"The options as JSON: [{\"label\",\"description\",\"preview\",\"key\"}…] (a preview is monospace text, up to 2 KiB); not with --option"`
	Multi       bool     `long:"multi" help:"The operator may pick several options"`
	Recommend   string   `long:"recommend" optional:"true" help:"The option you recommend: its number (from 1), key or label"`
	Blocks      string   `long:"blocks" optional:"true" help:"A task of the board (tk_…, typically yours) that waits on the answer: blocked on the question until it is answered"`
	Wait        string   `long:"wait" optional:"true" help:"Wait this long for the answer (e.g. 10m) and print it; exit 6 if it does not come in time (the question stays open, the answer still comes by mail; on a --peer board, read it with taskboard show)"`
	Idea        bool     `long:"idea" help:"Put it in the operator's ideas, not waiting on them (not with --blocks or --wait)"`
	Attach      []string `long:"attach" optional:"true" help:"Attach a file you can read (repeatable; at most 10, 4 MiB each)"`
	Peer        string   `long:"peer" optional:"true" help:"Ask the operator of a linked node, on its board, under the grants it gave this node"`
	AskHuman    string   `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s'). Capped at 300s. Timeout = deny."`
	JSON        bool     `long:"json" help:"Output JSON"`
}

func taskboardAskOperatorCmd() *cobra.Command {
	return boa.CmdT[taskboardAskOperatorParams]{
		Use:   "ask-operator",
		Short: "Ask the operator a question on the board: options to pick and/or free text; the answer comes back to you",
		Long: "Puts a question to the operator as a task in their lane: up to 6 --option choices (or none, for an answer in " +
			"words), --multi to allow several, --recommend to mark the one you suggest. The operator answers in the dashboard " +
			"or with `taskboard answer`: the task closes as done, and the answer comes to your inbox as " +
			"`[taskboard] answered: …` with the picks, the text and the answer as JSON. --blocks makes your task wait on it " +
			"(reopened when it is answered); --wait holds the command until the answer comes and prints it. Asking about " +
			"your own work needs taskboard.own. The board stays quiet: no popup, no Messages entry.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *taskboardAskOperatorParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Group).SetAlternativesFunc(completeGroupNames)
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
	if p.Idea && (p.Blocks != "" || wait > 0) {
		fmt.Fprintln(stderr, "Error: an idea for the operator is not waited on; --idea takes no --blocks or --wait")
		return rcInvalidArg
	}
	body, _, err := readTaskBody(p.Body, p.BodyFile, stdin)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcInvalidArg
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
	req := map[string]any{"title": p.Question, "body": body, "to": "operator", "question": q}
	if b := strings.TrimSpace(p.Blocks); b != "" {
		req["blocks"] = b
	}
	if p.Idea {
		req["start"], req["stage"] = false, "idea"
	}
	path := "/v1/groups/" + url.PathEscape(strings.TrimSpace(p.Group)) + "/taskboard/tasks"
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	ask, rc := parseAskHumanFlag(p.AskHuman, stdout, stderr)
	if rc != rcOK {
		return rc
	}
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
		fmt.Fprintf(stdout, "asked %s: %s  [%s]; %s\n", t.ID, t.Title, t.Stage, answerWhere(p.Peer, t.ID))
		return rcOK
	}
	if !p.JSON {
		fmt.Fprintf(stdout, "asked %s: %s; waiting up to %s for the answer…\n", t.ID, t.Title, wait)
	}
	return waitTaskboardAnswer(p.Peer, t.ID, wait, p.JSON, stdout, stderr)
}

// answerWhere says where the answer to a question comes: the inbox for a
// local question; a linked node's daemon cannot mail this one, so a --peer
// question's answer is read on its board.
func answerWhere(peer, id string) string {
	if peer == "" {
		return "the answer comes to your inbox"
	}
	return fmt.Sprintf("read the answer with `tclaude agent taskboard show %s --peer %s`", id, peer)
}

// taskboardAnswerState is GET …/answer.
type taskboardAnswerState struct {
	Task     string             `json:"task"`
	Status   string             `json:"status"`
	Answered bool               `json:"answered"`
	Question *taskboardQuestion `json:"question"`
}

// waitTaskboardAnswer long-polls the question until it is answered or
// closed, or wait runs out.
func waitTaskboardAnswer(peer, id string, wait time.Duration, asJSON bool, stdout, stderr io.Writer) int {
	deadline := time.Now().Add(wait)
	for {
		left := time.Until(deadline)
		if left <= 0 {
			fmt.Fprintf(stderr, "no answer within %s; %s stays open: %s\n", wait, id, answerWhere(peer, id))
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
		if err := remoteRequestOpts(peer, http.MethodGet, taskboardPath(id, "/answer")+"?wait="+step.String(), nil, &st, opts); err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return MapDaemonErrorToRC(err)
		}
		if st.Answered || (st.Status != "open" && st.Status != "blocked") {
			if asJSON {
				return writeJSONTo(stdout, st)
			}
			if !st.Answered {
				fmt.Fprintf(stdout, "%s was %s without an answer\n", id, st.Status)
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
	Task     string `pos:"true" help:"Question task id (tk_…)"`
	Pick     string `long:"pick" optional:"true" help:"The option(s) picked: key, label or number (from 1), comma-separated for a multi-select question"`
	Text     string `long:"text" optional:"true" help:"An answer in words, with the pick or instead of one"`
	Peer     string `long:"peer" optional:"true" help:"Answer on a linked node's board, under the grants it gave this node (taskboard.assign)"`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s'). Capped at 300s. Timeout = deny."`
	JSON     bool   `long:"json" help:"Output JSON"`
}

func taskboardAnswerCmd() *cobra.Command {
	return boa.CmdT[taskboardAnswerParams]{
		Use:   "answer",
		Short: "Answer a question put to the operator: it closes as done and the answer goes to the agent that asked",
		Long: "Answers a question task (`ask-operator`): --pick names the option(s), --text answers in words, with a pick or " +
			"instead of one. The task closes as done, the tasks waiting on it reopen, and the agent that asked gets the " +
			"answer by mail. Answering is the operator's, or a taskboard.assign holder's; never the asking agent's.",
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

func runTaskboardAnswer(p *taskboardAnswerParams, stdout, stderr io.Writer) int {
	picks := splitTaskIDs(p.Pick)
	if len(picks) == 0 && strings.TrimSpace(p.Text) == "" {
		fmt.Fprintln(stderr, "Error: give --pick, --text, or both")
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	// A number picks by position: read the question first.
	numbered := false
	for _, k := range picks {
		var n int
		if _, err := fmt.Sscanf(k, "%d", &n); err == nil && fmt.Sprint(n) == k {
			numbered = true
		}
	}
	if numbered {
		var t taskboardTask
		if err := remoteRequest(p.Peer, http.MethodGet, taskboardPath(p.Task, ""), nil, &t); err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return MapDaemonErrorToRC(err)
		}
		if t.Question == nil {
			fmt.Fprintf(stderr, "Error: %s is not a question\n", t.ID)
			return rcInvalidArg
		}
		for i, k := range picks {
			var n int
			if _, err := fmt.Sscanf(k, "%d", &n); err == nil && fmt.Sprint(n) == k {
				if n < 1 || n > len(t.Question.Options) {
					fmt.Fprintf(stderr, "Error: --pick %d: the question has %d options\n", n, len(t.Question.Options))
					return rcInvalidArg
				}
				picks[i] = t.Question.Options[n-1].Key
			}
		}
	}
	req := map[string]any{"picked": picks, "text": strings.TrimSpace(p.Text)}
	return taskboardWrite(p.Peer, http.MethodPost, taskboardPath(p.Task, "/answer"), req, p.AskHuman, p.JSON, "answered", stdout, stderr)
}
