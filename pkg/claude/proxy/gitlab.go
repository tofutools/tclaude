package proxy

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/common"
)

// gitlab.go is `tclaude proxy gitlab {mr,issue,pipeline,job} …` — GitLab
// merge-request, issue and CI operations performed by `tclaude agentd` with
// ITS GitLab token, so a sandboxed agent that holds no credential can still
// open a merge request and read why a pipeline went red.
//
// It is the GitHub command's twin. The project is never named by the agent:
// the daemon derives it from the agent's own remote, after that remote has
// passed the operator's allow-list AND its host has been configured as a GitLab
// instance. There is no passthrough of any kind.

func gitlabCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:     "gitlab",
		Aliases: []string{"gl"},
		Short:   "GitLab merge-request, issue and pipeline operations performed by the daemon",
		Long: "Open and manage merge requests and issues, and read pipelines, WITHOUT holding a GitLab token " +
			"yourself.\n\n" +
			"`tclaude agentd` calls the GitLab instance on the host with its own token. Everything you create " +
			"is attributed to that token's owner, so treat it as writing under their name.\n\n" +
			"The project is not something you choose: the daemon derives it from your own repository's " +
			"remote, and only after that remote passes the operator's allow-list " +
			"(agent.git_proxy.allowed_remotes) and its host is a configured GitLab instance " +
			"(agent.git_proxy.gitlab_hosts, or gitlab.com). Run `tclaude proxy git remotes` to see it.\n\n" +
			"Reads need `proxy.gitlab.read`; creating and commenting needs `proxy.gitlab.write`; merging needs " +
			"`proxy.gitlab.merge`, which `proxy.gitlab.write` does NOT confer. None is granted by default.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			gitlabMRCmd(),
			gitlabIssueCmd(),
			gitlabPipelineCmd(),
			gitlabJobCmd(),
		},
	}.ToCobra()
}

// glProxyCall is the shared tail of every gitlab verb; the daemon answers in
// the GitHub proxy's wire shape, so the renderer is shared.
func glProxyCall(path string, body map[string]any, askHuman, what string, stdout, stderr io.Writer) int {
	return forgeProxyCallTimeout("gitlab", path, body, askHuman, what, ghProxyTimeout, stdout, stderr)
}

// ---------------------------------------------------------------------------
// gitlab mr
// ---------------------------------------------------------------------------

func gitlabMRCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:         "mr",
		Short:       "Merge requests",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			gitlabMRCreateCmd(),
			gitlabMRListCmd(),
			gitlabMRViewCmd(),
			gitlabMRChecksCmd(),
			gitlabMRCommentsCmd(),
			gitlabMRCommentCmd(),
			gitlabMREditCmd(),
			gitlabMRReadyCmd(),
			gitlabMRMergeCmd(),
		},
	}.ToCobra()
}

type gitlabMRCreateParams struct {
	Title    string `long:"title" short:"t" help:"Merge-request title."`
	Body     string `long:"body" optional:"true" help:"Merge-request description. Prefer --body-file for anything multi-line."`
	BodyFile string `long:"body-file" short:"F" optional:"true" help:"Read the description from this file ('-' reads stdin). Sidesteps shell quoting and keeps the text out of /proc's cmdline."`
	Base     string `long:"base" optional:"true" help:"Target branch (default: the project's default branch)."`
	Head     string `long:"head" optional:"true" help:"Source branch. Defaults to the branch your work tree is on, which the daemon reads for you."`
	Draft    bool   `long:"draft" optional:"true" help:"Open as a draft (GitLab marks it with a 'Draft:' title prefix)."`
	Remote   string `long:"remote" optional:"true" help:"Remote naming the project to act on (default: origin)."`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '60s'). Capped at 300s. Timeout = deny."`
}

func gitlabMRCreateCmd() *cobra.Command {
	return boa.CmdT[gitlabMRCreateParams]{
		Use:   "create",
		Short: "Open a merge request",
		Long: "Opens a merge request on the project your remote points at. Push the branch first " +
			"(`tclaude proxy git push -u`), or GitLab has nothing to compare.\n\n" +
			"The merge request is authored by the OPERATOR's GitLab token — the daemon holds the credential, not you.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *gitlabMRCreateParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(agent.CompleteAskHumanDurations)
			return nil
		},
		RunFunc: func(p *gitlabMRCreateParams, _ *cobra.Command, _ []string) {
			os.Exit(runGitLabMRCreate(p, os.Stdin, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runGitLabMRCreate(p *gitlabMRCreateParams, stdin io.Reader, stdout, stderr io.Writer) int {
	body, rc := agent.ResolveBodyInput(p.Body, p.BodyFile, "--body", stdin, stderr)
	if rc != rcOK {
		return rc
	}
	if strings.TrimSpace(p.Title) == "" {
		fmt.Fprintln(stderr, "Error: --title is required.")
		return rcInvalidArg
	}
	return glProxyCall("/v1/gitlab/mr/create", map[string]any{
		"remote": strings.TrimSpace(p.Remote),
		"title":  strings.TrimSpace(p.Title),
		"body":   body,
		"base":   strings.TrimSpace(p.Base),
		"head":   strings.TrimSpace(p.Head),
		"draft":  p.Draft,
	}, p.AskHuman, "mr create", stdout, stderr)
}

type gitlabListParams struct {
	State    string `long:"state" optional:"true" help:"Filter by state."`
	Limit    int    `long:"limit" optional:"true" help:"Maximum rows (1-100, default 20)."`
	Remote   string `long:"remote" optional:"true" help:"Remote naming the project to act on (default: origin)."`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout. Capped at 300s. Timeout = deny."`
}

type gitlabMRListParams struct {
	State    string `long:"state" optional:"true" help:"Filter by state: open, closed, merged or all."`
	Head     string `long:"head" short:"H" optional:"true" help:"Only merge requests opened FROM this source branch. This is how you find the MR for a branch when you know its name and not its number."`
	Limit    int    `long:"limit" optional:"true" help:"Maximum rows (1-100, default 20)."`
	Remote   string `long:"remote" optional:"true" help:"Remote naming the project to act on (default: origin)."`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout. Capped at 300s. Timeout = deny."`
}

func gitlabMRListCmd() *cobra.Command {
	return boa.CmdT[gitlabMRListParams]{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List merge requests (open|closed|merged|all)",
		Long: "Lists merge requests, newest-created first.\n\n" +
			"`--head` answers the question the number-taking verbs cannot: which merge request belongs " +
			"to the branch I am on?\n\n" +
			"    tclaude proxy gitlab mr ls --head my-branch --state all\n\n" +
			"The `number` in a row is the merge request's iid — the `!42` in GitLab's UI — and is what " +
			"`mr view`, `mr checks` and `mr comments` take.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *gitlabMRListParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(agent.CompleteAskHumanDurations)
			boa.GetParamT(ctx, &p.State).SetAlternatives([]string{"open", "closed", "merged", "all"})
			return nil
		},
		RunFunc: func(p *gitlabMRListParams, _ *cobra.Command, _ []string) {
			os.Exit(glProxyCall("/v1/gitlab/mr/list", map[string]any{
				"remote": strings.TrimSpace(p.Remote),
				"state":  strings.TrimSpace(p.State),
				"head":   strings.TrimSpace(p.Head),
				"limit":  p.Limit,
			}, p.AskHuman, "mr list", os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

type gitlabNumberParams struct {
	Number   int    `pos:"true" help:"Merge-request or issue number (the iid shown as !N or #N)."`
	Remote   string `long:"remote" optional:"true" help:"Remote naming the project to act on (default: origin)."`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout. Capped at 300s. Timeout = deny."`
}

func glNumberBody(p *gitlabNumberParams) map[string]any {
	return map[string]any{"remote": strings.TrimSpace(p.Remote), "number": p.Number}
}

func gitlabNumberCmd(use, short, long, path, what string, timeout bool) *cobra.Command {
	return boa.CmdT[gitlabNumberParams]{
		Use:         use,
		Short:       short,
		Long:        long,
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *gitlabNumberParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(agent.CompleteAskHumanDurations)
			return nil
		},
		RunFunc: func(p *gitlabNumberParams, _ *cobra.Command, _ []string) {
			if timeout {
				os.Exit(forgeProxyCallTimeout("gitlab", path, glNumberBody(p), p.AskHuman, what,
					ghProxyBulkTimeout, os.Stdout, os.Stderr))
			}
			os.Exit(glProxyCall(path, glNumberBody(p), p.AskHuman, what, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func gitlabMRViewCmd() *cobra.Command {
	return gitlabNumberCmd("view", "Show one merge request", "", "/v1/gitlab/mr/view", "mr view", false)
}

func gitlabMRChecksCmd() *cobra.Command {
	return gitlabNumberCmd("checks", "Show the head pipeline and its jobs for a merge request",
		"Reports the merge request's head pipeline and its jobs. A pipeline with more jobs than the proxy "+
			"reads sets jobs_truncated, and its own status is then the verdict. A running pipeline is an answer, "+
			"not a failure — the command succeeds and the state is in the output. The pipeline `id` is what "+
			"`job log-failed` takes; a job's `id` is what `job artifacts` and `job download` take.",
		"/v1/gitlab/mr/checks", "mr checks", false)
}

func gitlabMRCommentsCmd() *cobra.Command {
	cmd := gitlabNumberCmd("comments", "Read all review feedback on a merge request",
		"Prints everything said on a merge request, in two sections. This is the READ; `mr comment` is "+
			"the write.\n\n"+
			"  1. the conversation — every note on the merge request, oldest first\n"+
			"  2. the inline diff notes — line-level comments, each with its file, line and permalink\n\n"+
			"System notes (\"added 2 commits\", \"changed the title\") are left out. Each section is bounded "+
			"separately; if one is too large you get its tail, the newest entries.",
		"/v1/gitlab/mr/comments", "mr comments", true)
	cmd.Aliases = []string{"thread"}
	return cmd
}

func gitlabMRReadyCmd() *cobra.Command {
	return gitlabNumberCmd("ready", "Mark a draft merge request ready for review",
		"Removes the 'Draft:' marker from the merge request's title, which is how GitLab represents the "+
			"draft state.",
		"/v1/gitlab/mr/ready", "mr ready", false)
}

type gitlabCommentParams struct {
	Number   int    `pos:"true" help:"Merge-request or issue number (iid)."`
	Body     string `long:"body" optional:"true" help:"Comment text. Prefer --body-file for anything multi-line."`
	BodyFile string `long:"body-file" short:"F" optional:"true" help:"Read the comment from this file ('-' reads stdin)."`
	Remote   string `long:"remote" optional:"true" help:"Remote naming the project to act on (default: origin)."`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout. Capped at 300s. Timeout = deny."`
}

func gitlabCommentCmd(short, path, what string) *cobra.Command {
	return boa.CmdT[gitlabCommentParams]{
		Use:         "comment",
		Short:       short,
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *gitlabCommentParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(agent.CompleteAskHumanDurations)
			return nil
		},
		RunFunc: func(p *gitlabCommentParams, _ *cobra.Command, _ []string) {
			os.Exit(runGitLabComment(path, p, what, os.Stdin, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runGitLabComment(path string, p *gitlabCommentParams, what string, stdin io.Reader, stdout, stderr io.Writer) int {
	body, rc := agent.ResolveBodyInput(p.Body, p.BodyFile, "--body", stdin, stderr)
	if rc != rcOK {
		return rc
	}
	if strings.TrimSpace(body) == "" {
		fmt.Fprintln(stderr, "Error: a comment body is required (--body or --body-file).")
		return rcInvalidArg
	}
	return glProxyCall(path, map[string]any{
		"remote": strings.TrimSpace(p.Remote),
		"number": p.Number,
		"body":   body,
	}, p.AskHuman, what, stdout, stderr)
}

func gitlabMRCommentCmd() *cobra.Command {
	return gitlabCommentCmd("Comment on a merge request (to read the thread, see `mr comments`)",
		"/v1/gitlab/mr/comment", "mr comment")
}

type gitlabMREditParams struct {
	Number   int    `pos:"true" help:"Merge-request number (iid)."`
	Title    string `long:"title" short:"t" optional:"true" help:"New title. Omit to leave it unchanged."`
	Body     string `long:"body" optional:"true" help:"New description. Prefer --body-file for anything multi-line."`
	BodyFile string `long:"body-file" short:"F" optional:"true" help:"Read the new description from this file ('-' reads stdin)."`
	Remote   string `long:"remote" optional:"true" help:"Remote naming the project to act on (default: origin)."`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout. Capped at 300s. Timeout = deny."`
}

func gitlabMREditCmd() *cobra.Command {
	return boa.CmdT[gitlabMREditParams]{
		Use:   "edit",
		Short: "Edit a merge request's title or description",
		Long: "Replaces the title and/or the description of an existing merge request. Whichever you omit " +
			"is left alone. Only the title and description can be changed — target branch, reviewers and " +
			"labels are outside what the proxy will do on your behalf.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *gitlabMREditParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(agent.CompleteAskHumanDurations)
			return nil
		},
		RunFunc: func(p *gitlabMREditParams, _ *cobra.Command, _ []string) {
			os.Exit(runGitLabMREdit(p, os.Stdin, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runGitLabMREdit(p *gitlabMREditParams, stdin io.Reader, stdout, stderr io.Writer) int {
	body, rc := agent.ResolveBodyInput(p.Body, p.BodyFile, "--body", stdin, stderr)
	if rc != rcOK {
		return rc
	}
	if strings.TrimSpace(p.Title) == "" && strings.TrimSpace(body) == "" {
		fmt.Fprintln(stderr, "Error: nothing to edit — pass --title, --body/--body-file, or both")
		return rcInvalidArg
	}
	return glProxyCall("/v1/gitlab/mr/edit", map[string]any{
		"remote": strings.TrimSpace(p.Remote),
		"number": p.Number,
		"title":  strings.TrimSpace(p.Title),
		"body":   body,
	}, p.AskHuman, "mr edit", stdout, stderr)
}

type gitlabMRMergeParams struct {
	Number   int    `pos:"true" help:"Merge-request number (iid)."`
	Method   string `long:"method" short:"m" optional:"true" help:"How to merge: merge (default, a merge commit) or squash."`
	Subject  string `long:"subject" optional:"true" help:"First line of the merge/squash commit message. Omit to let GitLab compose it."`
	Body     string `long:"body" optional:"true" help:"Rest of the commit message. Prefer --body-file for anything multi-line."`
	BodyFile string `long:"body-file" short:"F" optional:"true" help:"Read the rest of the commit message from this file ('-' reads stdin)."`
	Remote   string `long:"remote" optional:"true" help:"Remote naming the project to act on (default: origin)."`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout. Capped at 300s. Timeout = deny."`
}

// glMergeMethodAlternatives mirrors the daemon's `mr merge --method` vocabulary
// for shell completion; TestGLMergeMethodCompletionMatchesTheGate pins it.
var glMergeMethodAlternatives = []string{"merge", "squash"}

// GLMergeMethodAlternativesForTest exposes the completion vocabulary.
func GLMergeMethodAlternativesForTest() []string {
	return append([]string(nil), glMergeMethodAlternatives...)
}

func gitlabMRMergeCmd() *cobra.Command {
	return boa.CmdT[gitlabMRMergeParams]{
		Use:   "merge",
		Short: "Merge a merge request (needs `proxy.gitlab.merge`)",
		Long: "Merges a merge request into its target branch, as the OPERATOR's GitLab token.\n\n" +
			"This is the one verb `proxy.gitlab.write` does not cover. It has its own grant, " +
			"`proxy.gitlab.merge`, because opening a merge request proposes a change while merging one lands " +
			"it.\n\n" +
			"GitLab's own rules still decide: protected branches, required approvals and a required " +
			"pipeline are evaluated against the token's owner, and a merge they refuse is refused here, in " +
			"GitLab's words.\n\n" +
			"Merging a merge request that is ALREADY merged succeeds and says so. A draft, a closed merge " +
			"request, or one that conflicts with its target is refused with which of those it is.\n\n" +
			"This proxy never asks GitLab to delete the source branch. GitLab still deletes it when the " +
			"merge request itself is set to (its \"Delete source branch\" option, which a project can turn on " +
			"by default).",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *gitlabMRMergeParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(agent.CompleteAskHumanDurations)
			boa.GetParamT(ctx, &p.Method).SetAlternatives(glMergeMethodAlternatives)
			return nil
		},
		RunFunc: func(p *gitlabMRMergeParams, _ *cobra.Command, _ []string) {
			os.Exit(runGitLabMRMerge(p, os.Stdin, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runGitLabMRMerge(p *gitlabMRMergeParams, stdin io.Reader, stdout, stderr io.Writer) int {
	body, rc := agent.ResolveBodyInput(p.Body, p.BodyFile, "--body", stdin, stderr)
	if rc != rcOK {
		return rc
	}
	return glProxyCall("/v1/gitlab/mr/merge", map[string]any{
		"remote":  strings.TrimSpace(p.Remote),
		"number":  p.Number,
		"method":  strings.TrimSpace(p.Method),
		"subject": strings.TrimSpace(p.Subject),
		"body":    body,
	}, p.AskHuman, "mr merge", stdout, stderr)
}

// ---------------------------------------------------------------------------
// gitlab issue
// ---------------------------------------------------------------------------

func gitlabIssueCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:         "issue",
		Short:       "Issues",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			gitlabIssueListCmd(),
			gitlabNumberCmd("view", "Show one issue", "", "/v1/gitlab/issue/view", "issue view", false),
			gitlabCommentCmd("Comment on an issue", "/v1/gitlab/issue/comment", "issue comment"),
		},
	}.ToCobra()
}

func gitlabIssueListCmd() *cobra.Command {
	return boa.CmdT[gitlabListParams]{
		Use:         "ls",
		Aliases:     []string{"list"},
		Short:       "List issues (open|closed|all)",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *gitlabListParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(agent.CompleteAskHumanDurations)
			boa.GetParamT(ctx, &p.State).SetAlternatives([]string{"open", "closed", "all"})
			return nil
		},
		RunFunc: func(p *gitlabListParams, _ *cobra.Command, _ []string) {
			os.Exit(glProxyCall("/v1/gitlab/issue/list", map[string]any{
				"remote": strings.TrimSpace(p.Remote),
				"state":  strings.TrimSpace(p.State),
				"limit":  p.Limit,
			}, p.AskHuman, "issue list", os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

// ---------------------------------------------------------------------------
// gitlab pipeline / job
// ---------------------------------------------------------------------------

func gitlabPipelineCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:         "pipeline",
		Aliases:     []string{"ci"},
		Short:       "GitLab CI pipelines",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds:     []*cobra.Command{gitlabPipelineListCmd()},
	}.ToCobra()
}

type gitlabPipelineListParams struct {
	Branch   string `long:"branch" short:"b" optional:"true" help:"Only pipelines for this branch (ref)."`
	Status   string `long:"status" short:"s" optional:"true" help:"Only pipelines with this status (e.g. 'failed', 'running', 'success')."`
	Limit    int    `long:"limit" optional:"true" help:"Maximum rows (1-100, default 20)."`
	Remote   string `long:"remote" optional:"true" help:"Remote naming the project to act on (default: origin)."`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout. Capped at 300s. Timeout = deny."`
}

// glPipelineStatusAlternatives mirrors the daemon's `pipeline ls --status`
// vocabulary for shell completion; a test pins it to the authority in
// pkg/claude/agentd.
var glPipelineStatusAlternatives = []string{
	"created", "waiting_for_resource", "preparing", "pending", "running",
	"success", "failed", "canceled", "skipped", "manual", "scheduled",
}

// GLPipelineStatusAlternativesForTest exposes the completion vocabulary.
func GLPipelineStatusAlternativesForTest() []string {
	return append([]string(nil), glPipelineStatusAlternatives...)
}

func gitlabPipelineListCmd() *cobra.Command {
	return boa.CmdT[gitlabPipelineListParams]{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List pipelines",
		Long: "Lists pipelines, most recent first. The `id` in each row is what `job log-failed` takes.\n\n" +
			"This is how you find a failed pipeline:\n\n" +
			"    tclaude proxy gitlab pipeline ls --branch my-branch --status failed",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *gitlabPipelineListParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(agent.CompleteAskHumanDurations)
			boa.GetParamT(ctx, &p.Status).SetAlternatives(glPipelineStatusAlternatives)
			return nil
		},
		RunFunc: func(p *gitlabPipelineListParams, _ *cobra.Command, _ []string) {
			os.Exit(glProxyCall("/v1/gitlab/pipeline/list", map[string]any{
				"remote": strings.TrimSpace(p.Remote),
				"branch": strings.TrimSpace(p.Branch),
				"status": strings.TrimSpace(p.Status),
				"limit":  p.Limit,
			}, p.AskHuman, "pipeline ls", os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func gitlabJobCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:         "job",
		Short:       "GitLab CI jobs: failed traces and artifacts",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			gitlabJobLogFailedCmd(),
			gitlabJobArtifactsCmd(),
			gitlabJobDownloadCmd(),
		},
	}.ToCobra()
}

type gitlabPipelineParams struct {
	PipelineID int64  `pos:"true" name:"pipeline-id" help:"Pipeline id (the number in a pipeline URL, .../-/pipelines/<pipeline-id>)."`
	Remote     string `long:"remote" optional:"true" help:"Remote naming the project to act on (default: origin)."`
	AskHuman   string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout. Capped at 300s. Timeout = deny."`
}

func gitlabJobLogFailedCmd() *cobra.Command {
	return boa.CmdT[gitlabPipelineParams]{
		Use:     "log-failed",
		Aliases: []string{"failed-log"},
		Short:   "Show the trace of every failed job in a pipeline",
		Long: "Prints the trace of each job that failed in a pipeline. This is the follow-up to " +
			"`mr checks`: checks tells you which job went red, this tells you why.\n\n" +
			"Get the pipeline id from `mr checks` or `pipeline ls`. GitLab has no per-step log, so you get " +
			"the whole trace of each failed job, tailed to the daemon's bound — which is where the error is. " +
			"Jobs that are allowed to fail are included and labelled.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *gitlabPipelineParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(agent.CompleteAskHumanDurations)
			return nil
		},
		RunFunc: func(p *gitlabPipelineParams, _ *cobra.Command, _ []string) {
			os.Exit(forgeProxyCallTimeout("gitlab", "/v1/gitlab/job/log-failed", map[string]any{
				"remote":      strings.TrimSpace(p.Remote),
				"pipeline_id": p.PipelineID,
			}, p.AskHuman, "job log-failed", ghProxyBulkTimeout, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

type gitlabJobParams struct {
	JobID    int64  `pos:"true" name:"job-id" help:"Job id (the number in a job URL, .../-/jobs/<job-id>; also the id in the output of mr checks)."`
	Remote   string `long:"remote" optional:"true" help:"Remote naming the project to act on (default: origin)."`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout. Capped at 300s. Timeout = deny."`
}

func gitlabJobArtifactsCmd() *cobra.Command {
	return boa.CmdT[gitlabJobParams]{
		Use:     "artifacts",
		Aliases: []string{"artifact-ls"},
		Short:   "List what a job uploaded",
		Long: "Lists a job's artifact archive — its size, when it expires, and the artifact entries the job " +
			"uploaded (the archive, its metadata, reports), not the files inside the archive — without " +
			"downloading anything. Run it BEFORE `job download`: the proxy refuses archives over 512 MiB, " +
			"and this is how you find out.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *gitlabJobParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(agent.CompleteAskHumanDurations)
			return nil
		},
		RunFunc: func(p *gitlabJobParams, _ *cobra.Command, _ []string) {
			os.Exit(glProxyCall("/v1/gitlab/job/artifacts", map[string]any{
				"remote": strings.TrimSpace(p.Remote),
				"job_id": p.JobID,
			}, p.AskHuman, "job artifacts", os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func gitlabJobDownloadCmd() *cobra.Command {
	return boa.CmdT[gitlabJobParams]{
		Use:     "download",
		Aliases: []string{"dl"},
		Short:   "Download a job's artifacts into your work tree",
		Long: "Downloads and unzips a job's artifact archive, then prints where it landed and what is in " +
			"it.\n\n" +
			"YOU DO NOT CHOOSE THE DESTINATION, and there is no flag for it. Everything lands in " +
			"`.tclaude-artifacts/job-<job-id>/` at the root of your own work tree, for the reason the " +
			"GitHub download has the same rule: the daemon runs unsandboxed, and a path it took from you " +
			"would be a path it writes to as itself. The directory ignores itself in git.\n\n" +
			"It is emptied first, and only the three most recently used download directories are kept " +
			"(GitHub runs and GitLab jobs share the cap). COPY ANYTHING YOU WANT TO KEEP somewhere else.\n\n" +
			"The proxy refuses an archive over 512 MiB before fetching a byte, and one that unpacks past " +
			"2 GiB is deleted rather than kept. Run `job artifacts` first.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *gitlabJobParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(agent.CompleteAskHumanDurations)
			return nil
		},
		RunFunc: func(p *gitlabJobParams, _ *cobra.Command, _ []string) {
			os.Exit(forgeProxyCallTimeout("gitlab", "/v1/gitlab/job/download", map[string]any{
				"remote": strings.TrimSpace(p.Remote),
				"job_id": p.JobID,
			}, p.AskHuman, "job download", ghProxyDownloadTimeout, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}
