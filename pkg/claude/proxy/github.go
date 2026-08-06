package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/common"
)

// github.go is `tclaude proxy github {pr,issue} …` — GitHub pull-request and
// issue operations performed by `tclaude agentd` with ITS `gh` credentials, so
// a sandboxed agent that cannot read ~/.config/gh can still open a PR and
// answer review comments.
//
// The repository is never named by the agent. The daemon derives it from the
// agent's own remote, after that remote has passed the operator's allow-list.
// There is no `gh` passthrough: each verb below is a fixed argv the daemon
// builds from validated scalars.

// ghProxyTimeout is the client-side bound on a proxied gh call. It exceeds the
// daemon's own 60s so a slow GitHub surfaces the daemon's answer rather than a
// client hang-up that leaves the agent unsure whether a PR was created.
const ghProxyTimeout = 90 * time.Second

// ghProxyBulkTimeout is the bound for the two reads the daemon allows longer
// than its own 60s default: `run log-failed` (180s there — it downloads a
// run's whole log archive) and `pr comments` (90s — two calls under one
// budget). A client cap below the daemon's would turn a slow-but-succeeding
// read into a hang-up that reports nothing, which is the failure that leaves
// an agent unsure whether it saw all the feedback. One generous constant
// rather than one per verb: this is a backstop against a hung daemon, and the
// daemon's own bound is what actually ends a slow call.
const ghProxyBulkTimeout = 210 * time.Second

func githubCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:     "github",
		Aliases: []string{"gh"},
		Short:   "GitHub pull-request and issue operations performed by the daemon",
		Long: "Open and manage pull requests and issues WITHOUT holding a GitHub token yourself.\n\n" +
			"`tclaude agentd` runs `gh` on the host with its own credentials. Everything you create is " +
			"attributed to the operator's GitHub account, so treat it as writing under their name.\n\n" +
			"The repository is not something you choose: the daemon derives it from your own repository's " +
			"remote, and only after that remote passes the operator's allow-list " +
			"(agent.git_proxy.allowed_remotes). Run `tclaude proxy git remotes` to see it.\n\n" +
			"Reads need `github.read`; creating and commenting needs `github.write`. Neither is granted " +
			"by default.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			githubPRCmd(),
			githubIssueCmd(),
			githubRunCmd(),
		},
	}.ToCobra()
}

// ghProxyOutcome mirrors the daemon's wire shape. As with the git proxy, an
// HTTP success means the daemon RAN gh; ExitCode is gh's verdict.
//
// JSON is passed through unmodelled — the daemon does not model gh's schemas
// either, so a new GitHub field reaches the agent without a release on either
// side.
type ghProxyOutcome struct {
	Repo      string          `json:"repo"`
	ExitCode  int             `json:"exit_code"`
	JSON      json.RawMessage `json:"json"`
	Stdout    string          `json:"stdout"`
	Stderr    string          `json:"stderr"`
	Truncated bool            `json:"truncated"`
	TimedOut  bool            `json:"timed_out"`
}

func (o *ghProxyOutcome) render(stdout, stderr io.Writer, what string) int {
	if len(o.JSON) > 0 {
		// json.Indent, NOT unmarshal-into-any-then-re-marshal. Decoding gh's
		// response into `any` would turn every JSON number into a float64, so a
		// value past 2^53 comes back out changed — and it would also re-order
		// object keys alphabetically, because that is what Marshal does with a
		// map. Indent rewrites only the whitespace: same numbers, same field
		// order, same string escaping GitHub sent.
		var pretty bytes.Buffer
		if json.Indent(&pretty, o.JSON, "", "  ") == nil {
			pretty.WriteByte('\n')
			if _, err := stdout.Write(pretty.Bytes()); err != nil {
				fmt.Fprintln(stderr, "Error: could not write the GitHub response")
				return rcIOFailure
			}
		} else {
			fmt.Fprintln(stdout, string(o.JSON))
		}
	} else if s := strings.TrimRight(o.Stdout, "\n"); s != "" {
		fmt.Fprintln(stdout, s)
	}
	if s := strings.TrimRight(o.Stderr, "\n"); s != "" && o.ExitCode != 0 {
		fmt.Fprintln(stderr, s)
	}
	if o.Truncated {
		fmt.Fprintln(stderr, "(output truncated by the daemon; the tail is shown)")
	}
	if o.TimedOut {
		fmt.Fprintf(stderr, "Error: gh %s timed out in the daemon; it may or may not have taken effect.\n", what)
		return rcIOFailure
	}
	if o.ExitCode != 0 {
		fmt.Fprintf(stderr, "Error: gh %s failed (exit %d) against %s.\n", what, o.ExitCode, o.Repo)
		return rcIOFailure
	}
	return rcOK
}

// ghProxyCall is the shared tail of every github verb.
func ghProxyCall(path string, body map[string]any, askHuman, what string, stdout, stderr io.Writer) int {
	return ghProxyCallTimeout(path, body, askHuman, what, ghProxyTimeout, stdout, stderr)
}

// ghProxyCallTimeout is ghProxyCall for the verbs the daemon allows longer
// than its own 60s default.
func ghProxyCallTimeout(path string, body map[string]any, askHuman, what string, timeout time.Duration, stdout, stderr io.Writer) int {
	ask, err := agent.ParseAskHuman(askHuman)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcInvalidArg
	}
	if rc := agent.RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	// Same as gitProxyCall: --ask-human arms a fallback for the denied case, it
	// does not mean this call waits. It matters a little more here — these
	// verbs render gh's JSON on stdout, and a banner ahead of it would break a
	// caller that pipes the output into a parser.
	var resp ghProxyOutcome
	if err := agent.DaemonRequest(http.MethodPost, path, body, &resp,
		agent.DaemonOpts{AskHuman: ask, Timeout: timeout}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return agent.MapDaemonErrorToRC(err)
	}
	return resp.render(stdout, stderr, what)
}

// ---------------------------------------------------------------------------
// github pr
// ---------------------------------------------------------------------------

func githubPRCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:         "pr",
		Short:       "Pull requests",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			githubPRCreateCmd(),
			githubPRListCmd(),
			githubPRViewCmd(),
			githubPRChecksCmd(),
			githubPRCommentsCmd(),
			githubPRCommentCmd(),
			githubPREditCmd(),
			githubPRReadyCmd(),
		},
	}.ToCobra()
}

type githubPRCreateParams struct {
	Title    string `long:"title" short:"t" help:"Pull-request title."`
	Body     string `long:"body" optional:"true" help:"Pull-request body. Prefer --body-file for anything multi-line."`
	BodyFile string `long:"body-file" short:"F" optional:"true" help:"Read the body from this file ('-' reads stdin). Sidesteps shell quoting and keeps the text out of /proc's cmdline."`
	Base     string `long:"base" optional:"true" help:"Branch to merge into (default: the repository's default branch)."`
	Head     string `long:"head" optional:"true" help:"Branch to merge from. Defaults to the branch your work tree is on, which the daemon reads for you."`
	Draft    bool   `long:"draft" optional:"true" help:"Open as a draft."`
	Remote   string `long:"remote" optional:"true" help:"Remote naming the repository to act on (default: origin)."`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '60s'). Capped at 300s. Timeout = deny."`
}

func githubPRCreateCmd() *cobra.Command {
	return boa.CmdT[githubPRCreateParams]{
		Use:   "create",
		Short: "Open a pull request",
		Long: "Opens a pull request on the repository your remote points at. Push the branch first " +
			"(`tclaude proxy git push -u`), or GitHub has nothing to compare.\n\n" +
			"The PR is authored by the OPERATOR's GitHub account — the daemon holds the credential, not you.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *githubPRCreateParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(agent.CompleteAskHumanDurations)
			return nil
		},
		RunFunc: func(p *githubPRCreateParams, _ *cobra.Command, _ []string) {
			os.Exit(runGitHubPRCreate(p, os.Stdin, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runGitHubPRCreate(p *githubPRCreateParams, stdin io.Reader, stdout, stderr io.Writer) int {
	body, rc := agent.ResolveBodyInput(p.Body, p.BodyFile, "--body", stdin, stderr)
	if rc != rcOK {
		return rc
	}
	if strings.TrimSpace(p.Title) == "" {
		fmt.Fprintln(stderr, "Error: --title is required.")
		return rcInvalidArg
	}
	return ghProxyCall("/v1/github/pr/create", map[string]any{
		"remote": strings.TrimSpace(p.Remote),
		"title":  strings.TrimSpace(p.Title),
		"body":   body,
		"base":   strings.TrimSpace(p.Base),
		"head":   strings.TrimSpace(p.Head),
		"draft":  p.Draft,
	}, p.AskHuman, "pr create", stdout, stderr)
}

type githubListParams struct {
	State    string `long:"state" optional:"true" help:"Filter by state."`
	Limit    int    `long:"limit" optional:"true" help:"Maximum rows (1-100, default 20)."`
	Remote   string `long:"remote" optional:"true" help:"Remote naming the repository to act on (default: origin)."`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout. Capped at 300s. Timeout = deny."`
}

func githubPRListCmd() *cobra.Command {
	return boa.CmdT[githubListParams]{
		Use:         "ls",
		Aliases:     []string{"list"},
		Short:       "List pull requests (open|closed|merged|all)",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *githubListParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(agent.CompleteAskHumanDurations)
			return nil
		},
		RunFunc: func(p *githubListParams, _ *cobra.Command, _ []string) {
			os.Exit(ghProxyCall("/v1/github/pr/list", listBody(p), p.AskHuman, "pr list", os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func listBody(p *githubListParams) map[string]any {
	return map[string]any{
		"remote": strings.TrimSpace(p.Remote),
		"state":  strings.TrimSpace(p.State),
		"limit":  p.Limit,
	}
}

type githubNumberParams struct {
	Number   int    `pos:"true" help:"Pull-request or issue number."`
	Remote   string `long:"remote" optional:"true" help:"Remote naming the repository to act on (default: origin)."`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout. Capped at 300s. Timeout = deny."`
}

func numberBody(p *githubNumberParams) map[string]any {
	return map[string]any{"remote": strings.TrimSpace(p.Remote), "number": p.Number}
}

func githubPRViewCmd() *cobra.Command {
	return boa.CmdT[githubNumberParams]{
		Use:         "view",
		Short:       "Show one pull request",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *githubNumberParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(agent.CompleteAskHumanDurations)
			return nil
		},
		RunFunc: func(p *githubNumberParams, _ *cobra.Command, _ []string) {
			os.Exit(ghProxyCall("/v1/github/pr/view", numberBody(p), p.AskHuman, "pr view", os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func githubPRChecksCmd() *cobra.Command {
	return boa.CmdT[githubNumberParams]{
		Use:   "checks",
		Short: "Show CI check state for a pull request",
		Long: "Reports the status-check rollup for a pull request. Pending checks are an answer, not a " +
			"failure — the command succeeds and the state is in the output.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *githubNumberParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(agent.CompleteAskHumanDurations)
			return nil
		},
		RunFunc: func(p *githubNumberParams, _ *cobra.Command, _ []string) {
			os.Exit(ghProxyCall("/v1/github/pr/checks", numberBody(p), p.AskHuman, "pr checks", os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func githubPRCommentsCmd() *cobra.Command {
	return boa.CmdT[githubNumberParams]{
		Use:     "comments",
		Aliases: []string{"thread"},
		Short:   "Read all review feedback on a pull request",
		Long: "Prints everything said on a pull request, in two sections. This is the READ; `pr comment` " +
			"is the write.\n\n" +
			"  1. the conversation — issue comments and the body of each review submission, oldest first " +
			"(what `gh pr view N --comments` shows)\n" +
			"  2. the inline review comments — the line-level notes inside each review's diff threads, each " +
			"with its file, line and permalink\n\n" +
			"Both sections matter for a review bot: CodeRabbit posts its summary as a review body, but " +
			"every actionable finding is an inline comment. Section 1 alone tells you the PR was reviewed " +
			"and not what the review said.\n\n" +
			"The output is text, not JSON. Each section is bounded separately, so a long conversation " +
			"cannot squeeze out the inline findings; if one is too large you get its tail — the newest " +
			"comments, which are the ones you are usually here for.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *githubNumberParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(agent.CompleteAskHumanDurations)
			return nil
		},
		RunFunc: func(p *githubNumberParams, _ *cobra.Command, _ []string) {
			os.Exit(ghProxyCallTimeout("/v1/github/pr/comments", numberBody(p), p.AskHuman,
				"pr comments", ghProxyBulkTimeout, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func githubPRReadyCmd() *cobra.Command {
	return boa.CmdT[githubNumberParams]{
		Use:         "ready",
		Short:       "Mark a draft pull request ready for review",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *githubNumberParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(agent.CompleteAskHumanDurations)
			return nil
		},
		RunFunc: func(p *githubNumberParams, _ *cobra.Command, _ []string) {
			os.Exit(ghProxyCall("/v1/github/pr/ready", numberBody(p), p.AskHuman, "pr ready", os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

type githubCommentParams struct {
	Number   int    `pos:"true" help:"Pull-request or issue number."`
	Body     string `long:"body" optional:"true" help:"Comment text. Prefer --body-file for anything multi-line."`
	BodyFile string `long:"body-file" short:"F" optional:"true" help:"Read the comment from this file ('-' reads stdin)."`
	Remote   string `long:"remote" optional:"true" help:"Remote naming the repository to act on (default: origin)."`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout. Capped at 300s. Timeout = deny."`
}

func githubPRCommentCmd() *cobra.Command {
	return boa.CmdT[githubCommentParams]{
		Use:         "comment",
		Short:       "Comment on a pull request (to read the thread, see `pr comments`)",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *githubCommentParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(agent.CompleteAskHumanDurations)
			return nil
		},
		RunFunc: func(p *githubCommentParams, _ *cobra.Command, _ []string) {
			os.Exit(runGitHubComment("/v1/github/pr/comment", p, "pr comment", os.Stdin, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

type githubPREditParams struct {
	Number   int    `pos:"true" help:"Pull-request number."`
	Title    string `long:"title" short:"t" optional:"true" help:"New title. Omit to leave it unchanged."`
	Body     string `long:"body" optional:"true" help:"New description. Prefer --body-file for anything multi-line."`
	BodyFile string `long:"body-file" short:"F" optional:"true" help:"Read the new description from this file ('-' reads stdin)."`
	Remote   string `long:"remote" optional:"true" help:"Remote naming the repository to act on (default: origin)."`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout. Capped at 300s. Timeout = deny."`
}

func githubPREditCmd() *cobra.Command {
	return boa.CmdT[githubPREditParams]{
		Use:   "edit",
		Short: "Edit a pull request's title or description",
		Long: "Replaces the title and/or the description of an existing pull request. Whichever you omit " +
			"is left alone.\n\n" +
			"The edit is attributed to the OPERATOR's GitHub account, like everything else this command " +
			"writes. Only the title and body can be changed — base branch, reviewers and labels are " +
			"outside what the proxy will do on your behalf.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *githubPREditParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(agent.CompleteAskHumanDurations)
			return nil
		},
		RunFunc: func(p *githubPREditParams, _ *cobra.Command, _ []string) {
			os.Exit(runGitHubPREdit(p, os.Stdin, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runGitHubPREdit(p *githubPREditParams, stdin io.Reader, stdout, stderr io.Writer) int {
	body, rc := agent.ResolveBodyInput(p.Body, p.BodyFile, "--body", stdin, stderr)
	if rc != rcOK {
		return rc
	}
	if strings.TrimSpace(p.Title) == "" && strings.TrimSpace(body) == "" {
		fmt.Fprintln(stderr, "Error: nothing to edit — pass --title, --body/--body-file, or both")
		return rcInvalidArg
	}
	return ghProxyCall("/v1/github/pr/edit", map[string]any{
		"remote": strings.TrimSpace(p.Remote),
		"number": p.Number,
		"title":  strings.TrimSpace(p.Title),
		"body":   body,
	}, p.AskHuman, "pr edit", stdout, stderr)
}

func runGitHubComment(path string, p *githubCommentParams, what string, stdin io.Reader, stdout, stderr io.Writer) int {
	body, rc := agent.ResolveBodyInput(p.Body, p.BodyFile, "--body", stdin, stderr)
	if rc != rcOK {
		return rc
	}
	if strings.TrimSpace(body) == "" {
		fmt.Fprintln(stderr, "Error: a comment body is required (--body or --body-file).")
		return rcInvalidArg
	}
	return ghProxyCall(path, map[string]any{
		"remote": strings.TrimSpace(p.Remote),
		"number": p.Number,
		"body":   body,
	}, p.AskHuman, what, stdout, stderr)
}

// ---------------------------------------------------------------------------
// github issue
// ---------------------------------------------------------------------------

func githubIssueCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:         "issue",
		Short:       "Issues",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			githubIssueListCmd(),
			githubIssueViewCmd(),
			githubIssueCommentCmd(),
		},
	}.ToCobra()
}

func githubIssueListCmd() *cobra.Command {
	return boa.CmdT[githubListParams]{
		Use:         "ls",
		Aliases:     []string{"list"},
		Short:       "List issues (open|closed|all)",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *githubListParams, _ *cobra.Command, _ []string) {
			os.Exit(ghProxyCall("/v1/github/issue/list", listBody(p), p.AskHuman, "issue list", os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func githubIssueViewCmd() *cobra.Command {
	return boa.CmdT[githubNumberParams]{
		Use:         "view",
		Short:       "Show one issue",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *githubNumberParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(agent.CompleteAskHumanDurations)
			return nil
		},
		RunFunc: func(p *githubNumberParams, _ *cobra.Command, _ []string) {
			os.Exit(ghProxyCall("/v1/github/issue/view", numberBody(p), p.AskHuman, "issue view", os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func githubIssueCommentCmd() *cobra.Command {
	return boa.CmdT[githubCommentParams]{
		Use:         "comment",
		Short:       "Comment on an issue",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *githubCommentParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(agent.CompleteAskHumanDurations)
			return nil
		},
		RunFunc: func(p *githubCommentParams, _ *cobra.Command, _ []string) {
			os.Exit(runGitHubComment("/v1/github/issue/comment", p, "issue comment", os.Stdin, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

// ---------------------------------------------------------------------------
// github run
// ---------------------------------------------------------------------------

func githubRunCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:         "run",
		Aliases:     []string{"ci"},
		Short:       "GitHub Actions workflow runs",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			githubRunLogFailedCmd(),
		},
	}.ToCobra()
}

type githubRunParams struct {
	RunID    int64  `pos:"true" name:"run-id" help:"Workflow-run id (the number in an Actions URL, .../actions/runs/<run-id>)."`
	Remote   string `long:"remote" optional:"true" help:"Remote naming the repository to act on (default: origin)."`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout. Capped at 300s. Timeout = deny."`
}

func githubRunLogFailedCmd() *cobra.Command {
	return boa.CmdT[githubRunParams]{
		Use:     "log-failed",
		Aliases: []string{"failed-log"},
		Short:   "Show the log of the failed steps in a workflow run",
		Long: "Prints the log of whatever steps failed in a GitHub Actions run — the same thing " +
			"`gh run view <run-id> --log-failed` shows. This is the follow-up to `pr checks`: checks tells " +
			"you which job went red, this tells you why.\n\n" +
			"Get the run id from the `detailsUrl` of a failed check in `pr checks` output; it is the number " +
			"in `…/actions/runs/<run-id>/job/<job-id>`.\n\n" +
			"Only the failed steps are available — never the full log of a run, which for a green matrix " +
			"build is megabytes that say nothing the check rollup did not. Output is text; if it exceeds " +
			"the daemon's bound the tail is returned, which is where a failing step's error is.\n\n" +
			"The daemon downloads the run's log archive, so this is slower than the other reads.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *githubRunParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(agent.CompleteAskHumanDurations)
			return nil
		},
		RunFunc: func(p *githubRunParams, _ *cobra.Command, _ []string) {
			os.Exit(ghProxyCallTimeout("/v1/github/run/log-failed", map[string]any{
				"remote": strings.TrimSpace(p.Remote),
				"run_id": p.RunID,
			}, p.AskHuman, "run log-failed", ghProxyBulkTimeout, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}
