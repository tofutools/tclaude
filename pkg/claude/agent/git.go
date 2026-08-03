package agent

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

// git.go is `tclaude agent git {remotes,ls-remote,fetch,pull,push}` — Git
// remote operations performed by `tclaude agentd` on the host with ITS
// credentials, so a sandboxed agent that cannot read ~/.ssh can still sync
// with, and publish to, its remote.
//
// What the agent supplies is a semantic request — "push my branch" — not a
// command line. There is no passthrough flag and no way to influence git's
// argv; the daemon builds it from validated scalars. See
// pkg/claude/agentd/gitproxy.go for the hardening, and docs/git-proxy.md for
// the operator-facing picture.
//
// `pull` is the one verb that is split across the boundary. See runGitPull.

// gitProxyTimeout is the client-side timeout for a proxied network operation.
// It must exceed the daemon's own gitProxyNetworkTimeout (120s) so a slow push
// surfaces the daemon's timed_out answer rather than a client-side hang-up
// that leaves the agent unsure whether the push landed.
const gitProxyTimeout = 150 * time.Second

func gitCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:   "git",
		Short: "Git remote operations performed by the daemon with its own credentials",
		Long: "Fetch from and push to a Git remote WITHOUT holding the credential yourself.\n\n" +
			"`tclaude agentd` runs git on the host, where the SSH key lives; your sandbox never needs " +
			"access to ~/.ssh and never needs the network. You describe the operation (\"push my branch\"); " +
			"the daemon builds the command.\n\n" +
			"Two things bound what this can do, and both are the operator's to set:\n" +
			"  * the repository is always YOUR OWN — the git work tree containing the directory you were " +
			"launched in. There is no --repo flag.\n" +
			"  * the remote must be on the operator's allow-list (agent.git_proxy.allowed_remotes). " +
			"Run `tclaude agent git remotes` to see the verdict for each of your remotes.\n\n" +
			"Reads (remotes, ls-remote, fetch) need the `git.read` permission; push needs `git.push`. " +
			"Neither is granted by default — ask the operator, or pass --ask-human for a one-off approval.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			gitRemotesCmd(),
			gitLsRemoteCmd(),
			gitFetchCmd(),
			gitPullCmd(),
			gitPushCmd(),
		},
	}.ToCobra()
}

// gitProxyOutcome mirrors the daemon's wire shape. HTTP success means the
// daemon RAN git; ExitCode is git's own verdict, and the CLI's exit code
// follows it. That split matters: "non-fast-forward, pull first" is a real
// answer the agent must act on, not a daemon failure.
type gitProxyOutcome struct {
	Repo      string `json:"repo"`
	Remote    string `json:"remote"`
	RemoteRef string `json:"remote_ref"`
	Branch    string `json:"branch"`
	ExitCode  int    `json:"exit_code"`
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	Truncated bool   `json:"truncated"`
	TimedOut  bool   `json:"timed_out"`
}

// render prints a proxied git result and returns the process exit code.
func (o *gitProxyOutcome) render(stdout, stderr io.Writer, what string) int {
	if s := strings.TrimRight(o.Stdout, "\n"); s != "" {
		fmt.Fprintln(stdout, s)
	}
	// git writes its ref-update summary to stderr even on success, so it is
	// echoed either way rather than only on failure.
	if s := strings.TrimRight(o.Stderr, "\n"); s != "" {
		fmt.Fprintln(stderr, s)
	}
	if o.Truncated {
		fmt.Fprintln(stderr, "(output truncated by the daemon; the tail is shown)")
	}
	if o.TimedOut {
		fmt.Fprintf(stderr, "Error: %s timed out in the daemon; the operation may or may not have completed remotely.\n", what)
		return rcIOFailure
	}
	if o.ExitCode != 0 {
		fmt.Fprintf(stderr, "Error: git %s failed (exit %d) against %s.\n", what, o.ExitCode, o.RemoteRef)
		return rcIOFailure
	}
	return rcOK
}

// --- git remotes ---

type gitRemotesParams struct {
	JSON bool `long:"json" optional:"true" help:"Emit the raw daemon response instead of a table."`
}

func gitRemotesCmd() *cobra.Command {
	return boa.CmdT[gitRemotesParams]{
		Use:   "remotes",
		Short: "List your repository's remotes and whether the proxy may reach them",
		Long: "Shows every remote configured in your repository, the URL it resolves to, and whether it is " +
			"allowed by the operator's allow-list — with the reason when it is not. This is the command to " +
			"run FIRST: it needs no network and tells you exactly what to ask the operator for if a remote " +
			"is refused.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *gitRemotesParams, _ *cobra.Command, _ []string) {
			os.Exit(runGitRemotes(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

type gitRemoteView struct {
	Name       string `json:"name"`
	FetchURL   string `json:"fetch_url"`
	PushURL    string `json:"push_url"`
	RemoteRef  string `json:"remote_ref"`
	Allowed    bool   `json:"allowed"`
	RefusedFor string `json:"refused_for"`
}

type gitRemotesResponse struct {
	Repo           string          `json:"repo"`
	Branch         string          `json:"branch"`
	Remotes        []gitRemoteView `json:"remotes"`
	AllowedRemotes []string        `json:"allowed_remotes"`
	ProtectedRefs  []string        `json:"protected_refs"`
	AllowForcePush bool            `json:"allow_force_push"`
}

func runGitRemotes(p *gitRemotesParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var resp gitRemotesResponse
	if err := DaemonRequest(http.MethodGet, "/v1/git/remotes", nil, &resp, DaemonOpts{}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if p.JSON {
		if rc := writeJSONIndentAlias(stdout, resp); rc != rcOK {
			fmt.Fprintln(stderr, "Error: could not encode the response as JSON")
			return rc
		}
		return rcOK
	}
	fmt.Fprintf(stdout, "Repository: %s\n", resp.Repo)
	if resp.Branch != "" {
		fmt.Fprintf(stdout, "Branch:     %s\n", resp.Branch)
	}
	fmt.Fprintf(stdout, "Allowed:    %s\n", strings.Join(resp.AllowedRemotes, ", "))
	protection := "none"
	if len(resp.ProtectedRefs) > 0 {
		protection = strings.Join(resp.ProtectedRefs, ", ")
	}
	fmt.Fprintf(stdout, "Protected:  %s (force-push %s)\n", protection,
		map[bool]string{true: "allowed", false: "disabled"}[resp.AllowForcePush])
	if len(resp.Remotes) == 0 {
		fmt.Fprintln(stdout, "\nThis repository has no remotes configured.")
		return rcOK
	}
	fmt.Fprintln(stdout)
	for _, r := range resp.Remotes {
		mark := "✗"
		if r.Allowed {
			mark = "✓"
		}
		fmt.Fprintf(stdout, "%s %s  %s\n", mark, r.Name, r.FetchURL)
		if r.PushURL != "" && r.PushURL != r.FetchURL {
			fmt.Fprintf(stdout, "    push: %s\n", r.PushURL)
		}
		if r.RefusedFor != "" {
			fmt.Fprintf(stdout, "    refused: %s\n", r.RefusedFor)
		}
	}
	return rcOK
}

// --- git ls-remote ---

type gitLsRemoteParams struct {
	Remote   string `long:"remote" optional:"true" help:"Remote name (default: origin)."`
	Heads    bool   `long:"heads" optional:"true" help:"Only branches."`
	Tags     bool   `long:"tags" optional:"true" help:"Only tags."`
	Pattern  string `pos:"true" optional:"true" help:"Optional ref pattern, e.g. 'refs/heads/feat-*'."`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '60s'). Capped at 300s. Timeout = deny."`
}

func gitLsRemoteCmd() *cobra.Command {
	return boa.CmdT[gitLsRemoteParams]{
		Use:         "ls-remote",
		Short:       "List refs on the remote (cheapest way to check whether a branch exists there)",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *gitLsRemoteParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *gitLsRemoteParams, _ *cobra.Command, _ []string) {
			os.Exit(runGitLsRemote(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runGitLsRemote(p *gitLsRemoteParams, stdout, stderr io.Writer) int {
	body := map[string]any{
		"remote":  strings.TrimSpace(p.Remote),
		"heads":   p.Heads,
		"tags":    p.Tags,
		"pattern": strings.TrimSpace(p.Pattern),
	}
	return gitProxyCall("/v1/git/ls-remote", body, p.AskHuman, "ls-remote", stdout, stderr)
}

// --- git fetch ---

type gitFetchParams struct {
	Remote   string `long:"remote" optional:"true" help:"Remote name (default: origin)."`
	Branch   string `long:"branch" short:"b" optional:"true" help:"Fetch only this branch into refs/remotes/<remote>/<branch>."`
	Prune    bool   `long:"prune" optional:"true" help:"Delete remote-tracking refs that no longer exist on the remote."`
	Tags     bool   `long:"tags" optional:"true" help:"Also fetch tags."`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '60s'). Capped at 300s. Timeout = deny."`
}

func gitFetchCmd() *cobra.Command {
	return boa.CmdT[gitFetchParams]{
		Use:   "fetch",
		Short: "Fetch from the remote through the daemon",
		Long: "Updates your remote-tracking refs. It never touches your working tree, so nothing is " +
			"merged or checked out — use `tclaude agent git pull` for that, or merge yourself once " +
			"this has run.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *gitFetchParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *gitFetchParams, _ *cobra.Command, _ []string) {
			os.Exit(runGitFetch(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runGitFetch(p *gitFetchParams, stdout, stderr io.Writer) int {
	body := map[string]any{
		"remote": strings.TrimSpace(p.Remote),
		"branch": strings.TrimSpace(p.Branch),
		"prune":  p.Prune,
		"tags":   p.Tags,
	}
	return gitProxyCall("/v1/git/fetch", body, p.AskHuman, "fetch", stdout, stderr)
}

// --- git pull ---

type gitPullParams struct {
	Remote   string `long:"remote" optional:"true" help:"Remote name (default: origin)."`
	Branch   string `long:"branch" short:"b" optional:"true" help:"Branch to fetch and fast-forward (default: the current branch)."`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '60s'). Capped at 300s. Timeout = deny."`
}

func gitPullCmd() *cobra.Command {
	return boa.CmdT[gitPullParams]{
		Use:   "pull",
		Short: "Fetch through the daemon, then fast-forward locally",
		Long: "Fetches from the remote through the daemon (which holds the credential), then fast-forwards " +
			"your branch with a plain local `git merge --ff-only` run as YOU, in your own sandbox.\n\n" +
			"The split is deliberate. Updating the working tree runs .gitattributes filter programs, which " +
			"are named by a file in your own repository — so that half stays on your side of the boundary " +
			"where it always was, and only the half that needs a credential crosses it. A merge that is not " +
			"a fast-forward is left for you to resolve.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *gitPullParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *gitPullParams, _ *cobra.Command, _ []string) {
			os.Exit(runGitPull(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

// localGitRun is the seam for the LOCAL half of `pull`. It runs in the agent's
// own process with the agent's own privileges — deliberately, since it needs
// no credential — so it is a plain exec, not a daemon call.
var localGitRun = func(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func runGitPull(p *gitPullParams, stdout, stderr io.Writer) int {
	branch := strings.TrimSpace(p.Branch)
	remote := strings.TrimSpace(p.Remote)
	if remote == "" {
		remote = "origin"
	}
	if branch == "" {
		out, err := localGitRun("rev-parse", "--abbrev-ref", "HEAD")
		branch = strings.TrimSpace(out)
		if err != nil || branch == "" || branch == "HEAD" {
			fmt.Fprintln(stderr, "Error: could not determine the current branch (detached HEAD?); pass --branch.")
			return rcInvalidArg
		}
	}
	body := map[string]any{"remote": remote, "branch": branch}
	if rc := gitProxyCall("/v1/git/fetch", body, p.AskHuman, "fetch", stdout, stderr); rc != rcOK {
		return rc
	}
	// The local half. No credential is involved, so this is an ordinary git
	// command in the agent's own sandbox.
	target := remote + "/" + branch
	out, err := localGitRun("merge", "--ff-only", "refs/remotes/"+target)
	if s := strings.TrimSpace(out); s != "" {
		fmt.Fprintln(stdout, s)
	}
	if err != nil {
		fmt.Fprintf(stderr,
			"Error: fetched %s, but the local fast-forward failed: %v\n"+
				"Your branch has diverged; resolve it yourself (rebase or merge) — the daemon does not merge for you.\n",
			target, err)
		return rcIOFailure
	}
	return rcOK
}

// --- git push ---

type gitPushParams struct {
	Remote         string `long:"remote" optional:"true" help:"Remote name (default: origin)."`
	Branch         string `long:"branch" short:"b" optional:"true" help:"Branch to push (default: the current branch)."`
	SetUpstream    bool   `long:"set-upstream" short:"u" optional:"true" help:"Set the pushed branch as this branch's upstream."`
	ForceWithLease bool   `long:"force-with-lease" optional:"true" help:"Overwrite the remote branch, but only if it still matches what you last saw. Requires the operator to have enabled agent.git_proxy.allow_force_push."`
	AskHuman       string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '60s'). Capped at 300s. Timeout = deny."`
}

func gitPushCmd() *cobra.Command {
	return boa.CmdT[gitPushParams]{
		Use:   "push",
		Short: "Push a branch to the remote through the daemon",
		Long: "Pushes one branch, by name, to an allow-listed remote.\n\n" +
			"Two refusals are worth knowing before you try: operator-protected branches (usually main and " +
			"master) are never pushable through the proxy — push a feature branch and open a pull request — " +
			"and force-pushing is off unless the operator enabled it. Plain `--force` does not exist here; " +
			"only `--force-with-lease`, which refuses to discard commits you have not seen.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *gitPushParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *gitPushParams, _ *cobra.Command, _ []string) {
			os.Exit(runGitPush(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runGitPush(p *gitPushParams, stdout, stderr io.Writer) int {
	body := map[string]any{
		"remote":           strings.TrimSpace(p.Remote),
		"branch":           strings.TrimSpace(p.Branch),
		"set_upstream":     p.SetUpstream,
		"force_with_lease": p.ForceWithLease,
	}
	return gitProxyCall("/v1/git/push", body, p.AskHuman, "push", stdout, stderr)
}

// gitProxyCall is the shared tail of every network verb: parse --ask-human,
// require the daemon, POST, then render git's own verdict.
func gitProxyCall(path string, body map[string]any, askHuman, what string, stdout, stderr io.Writer) int {
	ask, err := ParseAskHuman(askHuman)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	if ask > 0 {
		fmt.Fprintf(stdout, "Waiting up to %s for human approval...\n", ask)
	}
	var resp gitProxyOutcome
	if err := DaemonRequest(http.MethodPost, path, body, &resp,
		DaemonOpts{AskHuman: ask, Timeout: gitProxyTimeout}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	return resp.render(stdout, stderr, what)
}
