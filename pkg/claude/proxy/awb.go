package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/common"
)

// awb.go is `tclaude proxy awb …` — Agent Work Board issue operations performed
// by `tclaude agentd` with ITS AWB account, so a sandboxed agent that holds no
// tracker credentials can still read the issue it is working on, claim it, and
// close it.
//
// The command tree deliberately MIRRORS awb's own, verb for verb and flag for
// flag, because an agent that has been taught `awb` should not have to be
// taught a second vocabulary to use it through the daemon. What is missing is
// missing on purpose:
//
//   - `--db` and `--attachments` name a database and a directory on the machine
//     the command runs on. The whole point of the proxy is that the agent does
//     not choose where the operator's credential is spent, so the server is the
//     operator's configuration and not a flag.
//   - `--color` / `--no-color` describe a terminal this output never reaches
//     directly, and the proxy has no human table mode for them to colour.
//   - `--no-context` turns off awb's directory-context file, and there is no
//     directory context here: the daemon is not in the agent's working tree,
//     which is why `create` requires an explicit --workspace.
//
// Everything else the daemon does. Every gate — which workspace, which
// permission slug, which of AWB's fixed vocabularies a value must be in — lives
// in `pkg/claude/agentd/awbproxy.go`, because a check made in this process is a
// check the caller could have skipped.

// awbProxyTimeout is the client-side bound on a proxied AWB call. It exceeds
// the daemon's own 60s budget so a slow AWB surfaces the daemon's answer rather
// than a client hang-up that leaves the agent unsure whether a claim landed.
const awbProxyTimeout = 90 * time.Second

// AWB bounds the canonical encoded metadata object at 64 KiB.
const maxAWBMetadataCLIBytes = 64 * 1024

func awbCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:   "awb",
		Short: "Agent Work Board issue operations performed by the daemon",
		Long: "Read and update AWB issues WITHOUT holding the tracker's credentials yourself.\n\n" +
			"`tclaude agentd` calls the operator's AWB server on the host with the operator's account. " +
			"Everything you write is attributed to that account, so treat it as writing under their " +
			"name.\n\n" +
			"The verbs and flags are awb's own, so what you know about `awb` applies here — with the " +
			"flags that name a local database or a terminal left out: there is no --db, --attachments, " +
			"--no-context, --color or --no-color, because the server is the operator's configuration " +
			"and this output is not a terminal.\n\n" +
			"Which workspaces you can reach is not something you choose. The operator may allow-list them " +
			"in agent.awb_proxy.allowed_workspaces, and your own grant may carry an awb_workspace scope; " +
			"where both exist you may act only where they agree, and where only the grant scope does it " +
			"is the whole policy. Run `tclaude proxy awb whoami` to see what that leaves you, beside the " +
			"workspaces the account can actually see.\n\n" +
			"That is what you reach unattended. When your grant's scope leaves out a workspace the " +
			"operator's list does carry, retrying the same command with --ask-human asks them to approve " +
			"it for that one command; the refusal says when this route is open. It covers one workspace " +
			"per command and remembers nothing, so a run of several commands there wants the grant " +
			"widened instead.\n\n" +
			"Reads need `proxy.awb.read`; writing needs `proxy.awb.write` AND the operator's " +
			"agent.awb_proxy.allow_write. Neither slug is granted by default.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			awbWhoamiCmd(),
			awbShowCmd(),
			awbListCmd(),
			awbReadyCmd(),
			awbBlockedCmd(),
			awbSearchCmd(),
			awbCreateCmd(),
			awbUpdateCmd(),
			awbLabelCmd(),
			awbClaimCmd(),
			awbReleaseCmd(),
			awbCloseCmd(),
			awbMakeReadyCmd(),
			awbReopenCmd(),
			awbDeleteCmd(),
			awbDepCmd(),
			awbCommentCmd(),
			awbActivityCmd(),
			awbAttachCmd(),
		},
	}.ToCobra()
}

// ---------------------------------------------------------------------------
// The wire shape and the two output modes
// ---------------------------------------------------------------------------

// awbProxyOutcome mirrors the daemon's wire shape.
//
// There is no ExitCode here, unlike the git and GitHub outcomes: no subprocess
// runs, so there is no second verdict to report. A 2xx means the operation
// happened; anything else arrives as an error with a code and a message.
type awbProxyOutcome struct {
	// Workspaces is the caller's EFFECTIVE workspace set, echoed on every response
	// — already narrowed by the caller's own grant scope, so it is what this
	// agent may reach rather than what the operator allows in general.
	Workspaces     []string        `json:"workspaces"`
	LegacyProjects []string        `json:"projects"`
	JSON           json.RawMessage `json:"json"`
	Text           string          `json:"text"`
	// Content is an attachment's bytes, for `attach get` alone.
	Content    []byte `json:"content"`
	HasContent bool   `json:"has_content"`
}

func (o *awbProxyOutcome) normalizeCompatibility() {
	if len(o.Workspaces) == 0 {
		o.Workspaces = o.LegacyProjects
	}
}

// awbOutputMode resolves the two output modes, and the ONLY two.
//
// awb has three — a boxed human table as well — and that one is deliberately
// absent: it is drawn to a terminal's width, and this output has crossed a
// socket to get here. --json is therefore the DEFAULT rather than one of three
// choices, which is also the mode an agent wants; --json itself exists so that
// an awb command line copies over unchanged.
//
// Every params struct below spells the two flags out rather than embedding a
// shared one, because boa registers no flag for an embedded struct's fields at
// all — see TestAWBEveryVerbOffersBothOutputModes, which is what would catch a
// new verb quietly losing them. This function is where the rule they share
// lives, so it is stated once even though the flags are declared many times.
func awbOutputMode(asJSON, compact bool, stderr io.Writer) (bool, int) {
	if asJSON && compact {
		fmt.Fprintln(stderr, "Error: --json and --compact are mutually exclusive.")
		return false, rcInvalidArg
	}
	return compact, rcOK
}

func (o *awbProxyOutcome) render(stdout, stderr io.Writer) int {
	if o.HasContent {
		if _, err := stdout.Write(o.Content); err != nil {
			fmt.Fprintln(stderr, "Error: could not write the attachment content")
			return rcIOFailure
		}
		return rcOK
	}
	if o.Text != "" {
		// The write is checked because a closed pipe would otherwise discard a
		// listing while the command reported success, and an agent would read
		// the empty output as "nothing matched".
		if _, err := fmt.Fprint(stdout, ensureTrailingNewline(o.Text)); err != nil {
			fmt.Fprintln(stderr, "Error: could not write the AWB response")
			return rcIOFailure
		}
		return rcOK
	}
	if len(o.JSON) == 0 {
		// A mutation in compact mode: awb prints nothing on success, and so
		// does this.
		return rcOK
	}
	// json.Indent rather than unmarshal-then-remarshal, for the same reason the
	// GitHub and Linear halves use it: decoding into `any` turns every number
	// into a float64 and re-orders object keys alphabetically. Indent rewrites
	// only the whitespace, so an issue's JSON is byte-comparable with awb's own.
	var pretty bytes.Buffer
	if json.Indent(&pretty, o.JSON, "", "  ") != nil {
		fmt.Fprintln(stdout, string(o.JSON))
		return rcOK
	}
	pretty.WriteByte('\n')
	if _, err := stdout.Write(pretty.Bytes()); err != nil {
		fmt.Fprintln(stderr, "Error: could not write the AWB response")
		return rcIOFailure
	}
	return rcOK
}

// ensureTrailingNewline is what keeps a compact listing one-line-per-issue
// whether or not the daemon's renderer ended on one.
func ensureTrailingNewline(s string) string {
	if strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}

// awbProxyCall is the shared tail of every awb verb.
func awbProxyCall(path string, body map[string]any, askHuman string, stdout, stderr io.Writer) int {
	if rc := rejectInvalidUTF8(body, stderr); rc != rcOK {
		return rc
	}
	ask, err := agent.ParseAskHuman(askHuman)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcInvalidArg
	}
	if rc := agent.RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var resp awbProxyOutcome
	if err := agent.DaemonRequest(http.MethodPost, path, body, &resp,
		agent.DaemonOpts{AskHuman: ask, Timeout: awbProxyTimeout}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return agent.MapDaemonErrorToRC(err)
	}
	resp.normalizeCompatibility()
	return resp.render(stdout, stderr)
}

// ---------------------------------------------------------------------------
// whoami
// ---------------------------------------------------------------------------

type awbWhoamiParams struct {
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout. Capped at 300s. Timeout = deny."`
	JSON     bool   `long:"json" optional:"true" help:"Print the stable JSON representation. This is the DEFAULT; the flag exists so an awb command line copies over unchanged."`
	Compact  bool   `long:"compact" optional:"true" help:"Print awb's one terse line per issue instead. Cheapest output there is, and the one to prefer when you only need to see what is there."`
}

func awbWhoamiCmd() *cobra.Command {
	return boa.CmdT[awbWhoamiParams]{
		Use:   "whoami",
		Short: "Show which AWB server and account the daemon uses, and which workspaces you may reach",
		Long: "Reports the AWB server the operator configured, the account it authenticates as, every " +
			"workspace that account can see, and whether YOU may reach each one.\n\n" +
			"Up to two lists bound you and the answer breaks out both, because they need different " +
			"fixes: operator_workspaces is agent.awb_proxy.allowed_workspaces (absent when the operator " +
			"configured none), grant_workspaces is the awb_workspace scope on your own grant (absent when " +
			"it is unscoped), and allowed_workspaces is what the ones that ARE present leave you.\n\n" +
			"allow_write is the operator's own ceiling: false means every mutating verb is refused " +
			"however your grants are spelled.\n\n" +
			"This is the command to run FIRST, and the command to run when something is refused: it " +
			"tells you the exact workspace key — and which of the two lists — to ask the operator to " +
			"widen, rather than leaving you to guess from a refusal.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *awbWhoamiParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(agent.CompleteAskHumanDurations)
			return nil
		},
		RunFunc: func(p *awbWhoamiParams, _ *cobra.Command, _ []string) {
			compact, rc := awbOutputMode(p.JSON, p.Compact, os.Stderr)
			if rc != rcOK {
				os.Exit(rc)
			}
			os.Exit(awbProxyCall("/v1/awb/whoami",
				map[string]any{"compact": compact}, p.AskHuman, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

// ---------------------------------------------------------------------------
// Reads addressed by one issue
// ---------------------------------------------------------------------------

// awbIDParams is the shape of every verb addressed by one issue.
//
// The id must carry its workspace — "awb-a3f9c1", or an unambiguous prefix of the
// hash such as "awb-a3f". awb also accepts a BARE hash, and this deliberately
// does not: the workspace is what the proxy's gate is checked against, and a bare
// hash names none, so accepting one would mean fetching the issue before
// deciding whether the caller may see it.
type awbIDParams struct {
	ID       string `pos:"true" help:"Issue id, e.g. awb-a3f9c1. A hash prefix works (awb-a3f); a BARE hash does not — the workspace is what the workspace gate is checked against."`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout. Capped at 300s. Timeout = deny."`
	JSON     bool   `long:"json" optional:"true" help:"Print the stable JSON representation. This is the DEFAULT; the flag exists so an awb command line copies over unchanged."`
	Compact  bool   `long:"compact" optional:"true" help:"Print awb's one terse line per issue instead. Cheapest output there is, and the one to prefer when you only need to see what is there."`
}

// awbIDCmd builds the verbs that take an id and nothing else.
func awbIDCmd(use, short, long, path string) *cobra.Command {
	return boa.CmdT[awbIDParams]{
		Use:         use,
		Short:       short,
		Long:        long,
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *awbIDParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(agent.CompleteAskHumanDurations)
			return nil
		},
		RunFunc: func(p *awbIDParams, _ *cobra.Command, _ []string) {
			compact, rc := awbOutputMode(p.JSON, p.Compact, os.Stderr)
			if rc != rcOK {
				os.Exit(rc)
			}
			os.Exit(awbProxyCall(path, map[string]any{
				"id": strings.TrimSpace(p.ID), "compact": compact,
			}, p.AskHuman, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func awbShowCmd() *cobra.Command {
	return awbIDCmd("show", "Print one issue in full",
		"Print an issue with its relations, derived blocked state, attachments and the Markdown "+
			"links found in its description.\n\n"+
			"Under --compact this prints the same single line a listing would and nothing else; the "+
			"default JSON is what you want when you need the description.",
		"/v1/awb/issue/show")
}

func awbReopenCmd() *cobra.Command {
	return awbIDCmd("reopen", "Set the status to open and clear every assignee",
		"Reopen a closed issue, returning it to the pool `ready` draws from. Its historical "+
			"close-reason comment stays in the activity timeline.\n\n"+
			"It acts only on a closed issue: on one that is not closed it succeeds and changes "+
			"nothing, whatever its assignees, so it can never take a claim away from somebody who is "+
			"working.\n\n"+
			"Needs proxy.awb.write and the operator's agent.awb_proxy.allow_write.",
		"/v1/awb/issue/reopen")
}

func awbMakeReadyCmd() *cobra.Command {
	return awbIDCmd("make-ready", "Move a backlog issue to open",
		"Make parked work active by moving it from backlog to open.\n\n"+
			"An issue already open succeeds unchanged. In-progress and closed issues are refused. "+
			"Blockers are left alone, so the issue may still not appear in ready.\n\n"+
			"Needs proxy.awb.write and the operator's agent.awb_proxy.allow_write.",
		"/v1/awb/issue/make-ready")
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// rejectInvalidUTF8 refuses a request carrying text that is not valid UTF-8,
// BEFORE it is marshalled.
//
// This is the one rule that cannot be left to the daemon, and the reason is the
// transport rather than the policy. encoding/json replaces an invalid byte
// sequence with U+FFFD on the way out, so by the time the daemon's own
// validator sees the string it is necessarily valid and the original bytes are
// gone. AWB refuses such a body outright — `awb comment add` reports "not valid
// UTF-8" — so without this check the proxy would silently store a replacement
// character where the native command reported an error, and would do it in an
// append-only timeline nothing can edit afterwards.
//
// Every other rule (blank, control characters, length) survives the round trip
// intact and stays where the gates belong: in the daemon, where a caller
// talking to the socket directly cannot skip it.
//
// It walks the whole body rather than the fields one verb knows about, so a
// verb added later inherits the check instead of having to remember it. []byte
// values are skipped: attachment content is base64 on the wire and arrives
// byte-exact, which is the whole reason it travels that way.
func rejectInvalidUTF8(body map[string]any, stderr io.Writer) int {
	// Sorted, so a body with two bad fields names the same one every run.
	keys := make([]string, 0, len(body))
	for key := range body {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		switch v := body[key].(type) {
		case string:
			if !utf8.ValidString(v) {
				return reportInvalidUTF8(key, stderr)
			}
		case []string:
			for _, item := range v {
				if !utf8.ValidString(item) {
					return reportInvalidUTF8(key, stderr)
				}
			}
		}
	}
	return rcOK
}

func reportInvalidUTF8(field string, stderr io.Writer) int {
	fmt.Fprintf(stderr,
		"Error: %s is not valid UTF-8. AWB stores text byte for byte and refuses anything else, "+
			"and sending it would replace the offending bytes rather than report them.\n", field)
	return rcInvalidArg
}

// awbFlagGiven reports whether the caller actually typed a flag, as opposed to
// leaving it at its zero value. It tolerates a nil command because the build
// helpers are exercised directly by tests as well as through cobra; with no
// command there is nothing that could have been typed, so nothing is sent.
func awbFlagGiven(cmd *cobra.Command, name string) bool {
	return cmd != nil && cmd.Flags().Changed(name)
}

// trimmedNonEmptyStrings drops blanks from a repeatable flag's values, so a
// stray `--label ”` is left out rather than sent for the daemon to refuse.
func trimmedNonEmptyStrings(in []string) []string {
	out := make([]string, 0, len(in))
	for _, raw := range in {
		if v := strings.TrimSpace(raw); v != "" {
			out = append(out, v)
		}
	}
	return out
}
