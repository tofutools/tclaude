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

// ---------------------------------------------------------------------------
// create
// ---------------------------------------------------------------------------

type awbCreateParams struct {
	Title           string `pos:"true" help:"Issue title, one line."`
	AskHuman        string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout. Capped at 300s. Timeout = deny."`
	Description     string `long:"description" optional:"true" help:"Markdown description of the issue. Prefer --description-file for anything multi-line."`
	DescriptionFile string `long:"description-file" short:"F" optional:"true" help:"Read the description from this file (\"-\" reads stdin)."`
	CommitHash      string `long:"commit-hash" short:"H" optional:"true" help:"Implementing commit hash."`
	PullRequestURL  string `long:"pull-request-url" short:"U" optional:"true" help:"Implementing pull request URL."`
	Metadata        string `long:"metadata" optional:"true" help:"Caller-owned JSON object to carry on the issue."`
	Type            string `long:"type" optional:"true" help:"epic, feature, bug, task or chore (default: task)."`
	Priority        *int   `long:"priority" help:"0 (highest) to 4 (lowest). Default 2."`
	Workspace       string `long:"workspace" optional:"true" help:"The workspace to create the issue in. Optional when exactly one visible workspace is within your proxy gate."`
	JSON            bool   `long:"json" optional:"true" help:"Print the stable JSON representation. This is the DEFAULT; the flag exists so an awb command line copies over unchanged."`
	Compact         bool   `long:"compact" optional:"true" help:"Print awb's one terse line per issue instead. Cheapest output there is, and the one to prefer when you only need to see what is there."`
	// Declared after --ask-human for the shorthand reason awbFilterParams gives.
	Claim          bool     `long:"claim" optional:"true" help:"Atomically assign the issue to the daemon's AWB account and set status to in_progress."`
	Backlog        bool     `long:"backlog" optional:"true" help:"Create in backlog; cannot be combined with --claim or --assignee."`
	Assignees      []string `long:"assignee" optional:"true" help:"Create and claim in one step. Repeat to assign several people; any assignee sets status to in_progress."`
	Labels         []string `long:"label" optional:"true" help:"Add this label. Repeat for several."`
	HasParent      string   `long:"has-parent" optional:"true" help:"The new issue is part of decomposing this one."`
	BlockedBy      []string `long:"blocked-by" optional:"true" help:"The new issue cannot start until this one is closed. Repeat for several."`
	DiscoveredFrom []string `long:"discovered-from" optional:"true" help:"The new issue was found while working on this one. Repeat for several."`
	Related        []string `long:"related" optional:"true" help:"Loose association. Repeat for several."`
}

func awbCreateCmd() *cobra.Command {
	return boa.CmdT[awbCreateParams]{
		Use:   "create",
		Short: "Create an issue, with its labels and relations, in one transaction",
		Long: "Create an issue and print its id.\n\n" +
			"The relation flags read \"the new issue — relation — the named issue\", the single " +
			"convention of the whole tool. Every issue they name goes through the same workspace gate " +
			"the new issue does: a relation shows up at both ends, so relating to an issue you cannot " +
			"reach would write into a workspace you cannot reach.\n\n" +
			"Creating with an assignee is an atomic create-and-claim.\n\n" +
			"The issue is created by the OPERATOR's AWB account and is a real ticket in their tracker. " +
			"Prefer commenting on — or updating — an existing issue when that says the same thing.\n\n" +
			"Needs proxy.awb.write and the operator's agent.awb_proxy.allow_write.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *awbCreateParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(agent.CompleteAskHumanDurations)
			boa.GetParamT(ctx, &p.Type).SetAlternatives(
				[]string{"epic", "feature", "bug", "task", "chore"})
			boa.GetParamT(ctx, &p.Priority).SetAlternatives([]string{"0", "1", "2", "3", "4"})
			return nil
		},
		RunFunc: func(p *awbCreateParams, cmd *cobra.Command, _ []string) {
			os.Exit(runAWBCreate(p, cmd, os.Stdin, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runAWBCreate(p *awbCreateParams, cmd *cobra.Command, stdin io.Reader, stdout, stderr io.Writer) int {
	body, rc := buildAWBCreateBody(p, cmd, stdin, stderr)
	if rc != rcOK {
		return rc
	}
	return awbProxyCall("/v1/awb/issue/create", body, p.AskHuman, stdout, stderr)
}

// buildAWBCreateBody is runAWBCreate without the call, so what the CLI decides
// to send can be asserted on without a daemon behind it.
func buildAWBCreateBody(
	p *awbCreateParams, cmd *cobra.Command, stdin io.Reader, stderr io.Writer,
) (map[string]any, int) {
	compact, rc := awbOutputMode(p.JSON, p.Compact, stderr)
	if rc != rcOK {
		return nil, rc
	}
	if strings.TrimSpace(p.Title) == "" {
		fmt.Fprintln(stderr, "Error: a title is required.")
		return nil, rcInvalidArg
	}
	body := map[string]any{"title": strings.TrimSpace(p.Title), "compact": compact}
	if workspace := strings.TrimSpace(p.Workspace); workspace != "" {
		body["workspace"] = workspace
	}
	// Sent only when actually given. A create has nothing to clear, and AWB
	// supplies its own defaults for type and priority, so an omitted flag must
	// arrive as an absent field rather than as a zero it would then store.
	if awbFlagGiven(cmd, "description") || awbFlagGiven(cmd, "description-file") {
		description, rc := agent.ResolveBodyInput(
			p.Description, p.DescriptionFile, "--description", stdin, stderr)
		if rc != rcOK {
			return nil, rc
		}
		body["description"] = description
	}
	if p.CommitHash != "" {
		body["commit_hash"] = p.CommitHash
	}
	if p.PullRequestURL != "" {
		body["pull_request_url"] = p.PullRequestURL
	}
	if awbFlagGiven(cmd, "metadata") {
		metadata, err := parseAWBMetadata(p.Metadata)
		if err != nil {
			fmt.Fprintf(stderr, "Error: metadata %v.\n", err)
			return nil, rcInvalidArg
		}
		body["metadata"] = metadata
	}
	if v := strings.TrimSpace(p.Type); v != "" {
		body["type"] = v
	}
	if p.Priority != nil {
		body["priority"] = *p.Priority
	}
	if p.Claim {
		body["claim"] = true
	}
	if p.Backlog {
		body["backlog"] = true
	}
	if values := trimmedNonEmptyStrings(p.Assignees); len(values) > 0 {
		body["assignees"] = values
	}
	if values := trimmedNonEmptyStrings(p.Labels); len(values) > 0 {
		body["labels"] = values
	}
	if v := strings.TrimSpace(p.HasParent); v != "" {
		body["has_parent"] = v
	}
	for key, values := range map[string][]string{
		"blocked_by":      p.BlockedBy,
		"discovered_from": p.DiscoveredFrom,
		"related":         p.Related,
	} {
		if trimmed := trimmedNonEmptyStrings(values); len(trimmed) > 0 {
			body[key] = trimmed
		}
	}
	return body, rcOK
}

// ---------------------------------------------------------------------------
// update
// ---------------------------------------------------------------------------

type awbUpdateParams struct {
	ID              string  `pos:"true" help:"Issue id, e.g. awb-a3f9c1."`
	AskHuman        string  `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout. Capped at 300s. Timeout = deny."`
	Title           *string `long:"title" help:"New title. Omit to leave it unchanged."`
	Description     string  `long:"description" optional:"true" help:"New description, replacing the old one. Pass an empty string to clear it. Prefer --description-file for anything multi-line."`
	DescriptionFile string  `long:"description-file" short:"F" optional:"true" help:"Read the new description from this file (\"-\" reads stdin)."`
	CommitHash      *string `long:"commit-hash" short:"H" help:"Implementing commit hash; empty clears it."`
	PullRequestURL  *string `long:"pull-request-url" short:"U" help:"Implementing pull request URL; empty clears it."`
	Metadata        *string `long:"metadata" help:"JSON object merged into the existing metadata, top level only."`
	Type            *string `long:"type" help:"epic, feature, bug, task or chore."`
	Priority        *int    `long:"priority" help:"0 (highest) to 4 (lowest)."`
	JSON            bool    `long:"json" optional:"true" help:"Print the stable JSON representation. This is the DEFAULT; the flag exists so an awb command line copies over unchanged."`
	Compact         bool    `long:"compact" optional:"true" help:"Print awb's one terse line per issue instead. Cheapest output there is, and the one to prefer when you only need to see what is there."`
}

func awbUpdateCmd() *cobra.Command {
	return boa.CmdT[awbUpdateParams]{
		Use:   "update",
		Short: "Change an issue's fields",
		Long: "Change the title, description, implementation links, metadata, type or priority. Whichever you omit is left alone.\n\n" +
			"--metadata takes a JSON object and merges it into the issue's existing metadata at the top level. " +
			"Keys it names are replaced whole; keys it omits are kept.\n\n" +
			"update cannot change the status or the assignee: claim, release, close, make-ready and " +
			"reopen are the transitions of either, which keeps in_progress and an assignee from drifting apart " +
			"and keeps a claim from being taken silently. It cannot change the labels either — that is " +
			"`label add` and `label rm`, one at a time, so a whole-set replace cannot discard a " +
			"concurrent edit.\n\n" +
			"Giving no field flag at all succeeds and changes nothing.\n\n" +
			"Needs proxy.awb.write and the operator's agent.awb_proxy.allow_write.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *awbUpdateParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(agent.CompleteAskHumanDurations)
			boa.GetParamT(ctx, &p.Type).SetAlternatives(
				[]string{"epic", "feature", "bug", "task", "chore"})
			boa.GetParamT(ctx, &p.Priority).SetAlternatives([]string{"0", "1", "2", "3", "4"})
			return nil
		},
		RunFunc: func(p *awbUpdateParams, cmd *cobra.Command, _ []string) {
			os.Exit(runAWBUpdate(p, cmd, os.Stdin, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runAWBUpdate(p *awbUpdateParams, cmd *cobra.Command, stdin io.Reader, stdout, stderr io.Writer) int {
	body, rc := buildAWBUpdateBody(p, cmd, stdin, stderr)
	if rc != rcOK {
		return rc
	}
	return awbProxyCall("/v1/awb/issue/update", body, p.AskHuman, stdout, stderr)
}

// buildAWBUpdateBody is runAWBUpdate without the call.
//
// It is worth isolating because this is where "clear it" and "leave it alone"
// are told apart, and for the description the two differ only in whether a key
// is present in the map — a distinction no amount of reading the params struct
// can confirm.
func buildAWBUpdateBody(
	p *awbUpdateParams, cmd *cobra.Command, stdin io.Reader, stderr io.Writer,
) (map[string]any, int) {
	compact, rc := awbOutputMode(p.JSON, p.Compact, stderr)
	if rc != rcOK {
		return nil, rc
	}
	if strings.TrimSpace(p.ID) == "" {
		fmt.Fprintln(stderr, "Error: an issue id is required.")
		return nil, rcInvalidArg
	}
	body := map[string]any{"id": strings.TrimSpace(p.ID), "compact": compact}
	if p.Title != nil {
		body["title"] = *p.Title
	}
	if p.Type != nil {
		body["type"] = *p.Type
	}
	if p.Priority != nil {
		body["priority"] = *p.Priority
	}
	if p.CommitHash != nil {
		body["commit_hash"] = *p.CommitHash
	}
	if p.PullRequestURL != nil {
		body["pull_request_url"] = *p.PullRequestURL
	}
	if p.Metadata != nil {
		metadata, err := parseAWBMetadata(*p.Metadata)
		if err != nil {
			fmt.Fprintf(stderr, "Error: metadata %v.\n", err)
			return nil, rcInvalidArg
		}
		body["metadata"] = metadata
	}
	// `--description ""` means "clear it", which is not the same request as
	// omitting the flag, and only cobra can tell the two apart — so it is sent
	// exactly when the caller typed it.
	if awbFlagGiven(cmd, "description") || awbFlagGiven(cmd, "description-file") {
		description, rc := agent.ResolveBodyInput(
			p.Description, p.DescriptionFile, "--description", stdin, stderr)
		if rc != rcOK {
			return nil, rc
		}
		body["description"] = description
	}
	return body, rcOK
}

// ---------------------------------------------------------------------------
// Status transitions
// ---------------------------------------------------------------------------

type awbClaimParams struct {
	ID       string `pos:"true" help:"Issue id, e.g. awb-a3f9c1."`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout. Capped at 300s. Timeout = deny."`
	Force    bool   `long:"force" optional:"true" help:"Override a blocked or closed issue."`
	JSON     bool   `long:"json" optional:"true" help:"Print the stable JSON representation. This is the DEFAULT; the flag exists so an awb command line copies over unchanged."`
	Compact  bool   `long:"compact" optional:"true" help:"Print awb's one terse line per issue instead. Cheapest output there is, and the one to prefer when you only need to see what is there."`
}

func awbClaimCmd() *cobra.Command {
	return boa.CmdT[awbClaimParams]{
		Use:   "claim",
		Short: "Atomically join the assignees and set status to in_progress",
		Long: "Claim an issue.\n\n" +
			"Claiming one already held by the same name succeeds, and another claimant joins without " +
			"replacing anyone. It fails if blocked or closed; --force overrides both. A close reason stays in the " +
			"issue's activity timeline either way — `comment list` still shows it.\n\n" +
			"The assignee is always the OPERATOR's AWB account: the daemon holds it, and agents " +
			"have no AWB identity of their own. Agents sharing that account must coordinate through " +
			"other means.\n\n" +
			"Needs proxy.awb.write and the operator's agent.awb_proxy.allow_write.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *awbClaimParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(agent.CompleteAskHumanDurations)
			return nil
		},
		RunFunc: func(p *awbClaimParams, _ *cobra.Command, _ []string) {
			compact, rc := awbOutputMode(p.JSON, p.Compact, os.Stderr)
			if rc != rcOK {
				os.Exit(rc)
			}
			body := map[string]any{
				"id": strings.TrimSpace(p.ID), "force": p.Force, "compact": compact,
			}
			os.Exit(awbProxyCall("/v1/awb/issue/claim", body, p.AskHuman, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

type awbForceParams struct {
	ID       string `pos:"true" help:"Issue id, e.g. awb-a3f9c1."`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout. Capped at 300s. Timeout = deny."`
	Force    bool   `long:"force" optional:"true"`
	JSON     bool   `long:"json" optional:"true" help:"Print the stable JSON representation. This is the DEFAULT; the flag exists so an awb command line copies over unchanged."`
	Compact  bool   `long:"compact" optional:"true" help:"Print awb's one terse line per issue instead. Cheapest output there is, and the one to prefer when you only need to see what is there."`
}

func awbReleaseCmd() *cobra.Command {
	return boa.CmdT[awbForceParams]{
		Use:   "release",
		Short: "Leave the assignees, reopening the issue when the last one leaves",
		Long: "Release an issue.\n\n" +
			"Releasing one that is already open and unassigned succeeds. It fails on a closed issue, " +
			"or on one assigned to somebody else, unless --force.\n\n" +
			"Needs proxy.awb.write and the operator's agent.awb_proxy.allow_write.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *awbForceParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(agent.CompleteAskHumanDurations)
			boa.GetParamT(ctx, &p.Force).SetDescription("Release a closed issue, or somebody else's.")
			return nil
		},
		RunFunc: func(p *awbForceParams, _ *cobra.Command, _ []string) {
			compact, rc := awbOutputMode(p.JSON, p.Compact, os.Stderr)
			if rc != rcOK {
				os.Exit(rc)
			}
			os.Exit(awbProxyCall("/v1/awb/issue/release", map[string]any{
				"id": strings.TrimSpace(p.ID), "force": p.Force, "compact": compact,
			}, p.AskHuman, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func awbDeleteCmd() *cobra.Command {
	return boa.CmdT[awbForceParams]{
		Use:   "delete",
		Short: "Hard delete an issue and its relations",
		Long: "Delete an issue. This is NOT recoverable and AWB cannot undo it.\n\n" +
			"It never refuses on account of dependents and has no --cascade: it orphans any children " +
			"and drops every relation, and reports how many went, since removing a blocker silently " +
			"makes other issues ready and orphaning children makes a decomposed parent's work " +
			"top-level.\n\n" +
			"--force is required, and it is a confirmation rather than an override: there is nothing " +
			"it overrides.\n\n" +
			"Needs proxy.awb.write and the operator's agent.awb_proxy.allow_write. Consider `close` " +
			"instead — it records why, and it is reversible.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *awbForceParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(agent.CompleteAskHumanDurations)
			boa.GetParamT(ctx, &p.Force).SetDescription("Confirm the deletion. Required.")
			return nil
		},
		RunFunc: func(p *awbForceParams, _ *cobra.Command, _ []string) {
			os.Exit(runAWBDelete(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runAWBDelete(p *awbForceParams, stdout, stderr io.Writer) int {
	compact, rc := awbOutputMode(p.JSON, p.Compact, stderr)
	if rc != rcOK {
		return rc
	}
	// Caught here as well as in the daemon because this one depends on the
	// arguments alone: saying so before the call spares a round trip against
	// the operator's tracker. The daemon checks it independently regardless.
	if !p.Force {
		fmt.Fprintln(stderr, "Error: delete needs --force: it is not recoverable.")
		return rcInvalidArg
	}
	return awbProxyCall("/v1/awb/issue/delete", map[string]any{
		"id": strings.TrimSpace(p.ID), "force": true, "compact": compact,
	}, p.AskHuman, stdout, stderr)
}

type awbCloseParams struct {
	ID       string  `pos:"true" help:"Issue id, e.g. awb-a3f9c1."`
	AskHuman string  `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout. Capped at 300s. Timeout = deny."`
	Reason   *string `long:"reason" help:"Record why it was closed, as a typed comment on the closing transition. An empty or omitted reason records nothing."`
	JSON     bool    `long:"json" optional:"true" help:"Print the stable JSON representation. This is the DEFAULT; the flag exists so an awb command line copies over unchanged."`
	Compact  bool    `long:"compact" optional:"true" help:"Print awb's one terse line per issue instead. Cheapest output there is, and the one to prefer when you only need to see what is there."`
}

func awbCloseCmd() *cobra.Command {
	return boa.CmdT[awbCloseParams]{
		Use:   "close",
		Short: "Set the status to closed",
		Long: "Close an issue.\n\n" +
			"A non-empty --reason is recorded as a typed comment on the closing transition, in the same " +
			"transaction — it is not a field on the issue, and it stays in the timeline if the issue is " +
			"reopened. An empty or omitted reason records nothing; there is no reason to \"clear\". Read " +
			"it back with `comment list`.\n\n" +
			"Closing a closed issue succeeds. The assignees are left alone, since they record who did the " +
			"work.\n\n" +
			"Needs proxy.awb.write and the operator's agent.awb_proxy.allow_write.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *awbCloseParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(agent.CompleteAskHumanDurations)
			return nil
		},
		RunFunc: func(p *awbCloseParams, _ *cobra.Command, _ []string) {
			compact, rc := awbOutputMode(p.JSON, p.Compact, os.Stderr)
			if rc != rcOK {
				os.Exit(rc)
			}
			body := map[string]any{"id": strings.TrimSpace(p.ID), "compact": compact}
			if p.Reason != nil {
				body["reason"] = *p.Reason
			}
			os.Exit(awbProxyCall("/v1/awb/issue/close", body, p.AskHuman, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

// ---------------------------------------------------------------------------
// label
// ---------------------------------------------------------------------------

type awbLabelParams struct {
	ID       string `pos:"true" help:"Issue id, e.g. awb-a3f9c1."`
	Label    string `pos:"true" help:"The label. Lowercase letters, digits, hyphens, underscores, dots and slashes."`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout. Capped at 300s. Timeout = deny."`
	JSON     bool   `long:"json" optional:"true" help:"Print the stable JSON representation. This is the DEFAULT; the flag exists so an awb command line copies over unchanged."`
	Compact  bool   `long:"compact" optional:"true" help:"Print awb's one terse line per issue instead. Cheapest output there is, and the one to prefer when you only need to see what is there."`
}

func awbLabelCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:   "label",
		Short: "Add or remove a label",
		Long: "Labels are managed ONE PER INVOCATION, which is AWB's design rather than a limitation " +
			"of this proxy: a whole-set replace would silently discard a concurrent edit.\n\n" +
			"Both verbs need proxy.awb.write and the operator's agent.awb_proxy.allow_write.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			awbLabelVerbCmd("add",
				"Add a label; adding one the issue already carries changes nothing",
				"/v1/awb/label/add"),
			awbLabelVerbCmd("rm",
				"Remove a label; removing one it does not carry changes nothing",
				"/v1/awb/label/rm"),
		},
	}.ToCobra()
}

func awbLabelVerbCmd(use, short, path string) *cobra.Command {
	return boa.CmdT[awbLabelParams]{
		Use: use, Short: short,
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *awbLabelParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(agent.CompleteAskHumanDurations)
			return nil
		},
		RunFunc: func(p *awbLabelParams, _ *cobra.Command, _ []string) {
			compact, rc := awbOutputMode(p.JSON, p.Compact, os.Stderr)
			if rc != rcOK {
				os.Exit(rc)
			}
			os.Exit(awbProxyCall(path, map[string]any{
				"id":      strings.TrimSpace(p.ID),
				"label":   strings.TrimSpace(p.Label),
				"compact": compact,
			}, p.AskHuman, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

// ---------------------------------------------------------------------------
// dep
// ---------------------------------------------------------------------------

type awbDepParams struct {
	ID             string  `pos:"true" help:"The subject issue — the one the relation is read FROM."`
	AskHuman       string  `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout. Capped at 300s. Timeout = deny."`
	BlockedBy      *string `long:"blocked-by" help:"The first issue cannot start until this one is closed."`
	HasParent      *string `long:"has-parent" help:"The second issue is the parent of the first."`
	DiscoveredFrom *string `long:"discovered-from" help:"The first issue was found while working on this one."`
	Related        *string `long:"related" help:"Loose, symmetric association."`
	Force          bool    `long:"force" optional:"true" help:"Replace an existing parent, which is the only refusal it overrides."`
	JSON           bool    `long:"json" optional:"true" help:"Print the stable JSON representation. This is the DEFAULT; the flag exists so an awb command line copies over unchanged."`
	Compact        bool    `long:"compact" optional:"true" help:"Print awb's one terse line per issue instead. Cheapest output there is, and the one to prefer when you only need to see what is there."`
}

// relation returns the single relation the invocation names, refusing zero and
// refusing two.
func (p *awbDepParams) relation(stderr io.Writer) (relType, other string, rc int) {
	found := make([][2]string, 0, 4)
	for _, c := range []struct {
		typ   string
		value *string
	}{
		{"blocked-by", p.BlockedBy},
		{"has-parent", p.HasParent},
		{"discovered-from", p.DiscoveredFrom},
		{"related", p.Related},
	} {
		if c.value != nil {
			found = append(found, [2]string{c.typ, strings.TrimSpace(*c.value)})
		}
	}
	switch len(found) {
	case 1:
		return found[0][0], found[0][1], rcOK
	case 0:
		fmt.Fprintln(stderr,
			"Error: give exactly one of --blocked-by, --has-parent, --discovered-from or --related.")
		return "", "", rcInvalidArg
	default:
		fmt.Fprintf(stderr, "Error: give exactly one relation flag, not %d.\n", len(found))
		return "", "", rcInvalidArg
	}
}

func awbDepCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:   "dep",
		Short: "Manage relations between issues",
		Long: "Every relation reads \"first id — relation — second id\", the single convention of the " +
			"whole tool. `dep rm` takes the same flag and the same two ids in the same order as " +
			"`dep add`, so removing a relation is literally the add command with rm substituted.\n\n" +
			"BOTH issues go through the workspace gate. A relation is read from either end — it appears " +
			"in the other issue's relations too — so relating to an issue in a workspace you cannot " +
			"reach is refused rather than allowed on the grounds that only the first one is \"yours\".",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			awbDepVerbCmd("add", "Record a relation between two issues",
				"Record a relation, read with the first issue as the subject.\n\n"+
					"An issue has at most one parent, so --has-parent on an issue that already has a "+
					"different one fails unless --force, which replaces it. Naming the parent it "+
					"already has succeeds and changes nothing. Adding a relation that already exists "+
					"succeeds and changes nothing.\n\n"+
					"Needs proxy.awb.write and the operator's agent.awb_proxy.allow_write.",
				"/v1/awb/dep/add"),
			awbDepVerbCmd("rm", "Remove a relation between two issues",
				"Remove a relation, taking the same one relation flag as `dep add`.\n\n"+
					"Removing one that does not exist succeeds and changes nothing.\n\n"+
					"Needs proxy.awb.write and the operator's agent.awb_proxy.allow_write.",
				"/v1/awb/dep/rm"),
			awbDepTreeCmd(),
		},
	}.ToCobra()
}

func awbDepVerbCmd(use, short, long, path string) *cobra.Command {
	return boa.CmdT[awbDepParams]{
		Use: use, Short: short, Long: long,
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *awbDepParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(agent.CompleteAskHumanDurations)
			return nil
		},
		RunFunc: func(p *awbDepParams, _ *cobra.Command, _ []string) {
			os.Exit(runAWBDep(p, path, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runAWBDep(p *awbDepParams, path string, stdout, stderr io.Writer) int {
	compact, rc := awbOutputMode(p.JSON, p.Compact, stderr)
	if rc != rcOK {
		return rc
	}
	relType, other, rc := p.relation(stderr)
	if rc != rcOK {
		return rc
	}
	return awbProxyCall(path, map[string]any{
		"id":      strings.TrimSpace(p.ID),
		"type":    relType,
		"other":   other,
		"force":   p.Force,
		"compact": compact,
	}, p.AskHuman, stdout, stderr)
}

func awbDepTreeCmd() *cobra.Command {
	return awbIDCmd("tree", "Print the subtree of children rooted at an issue",
		"Print the decomposition below an issue, to its full depth. It does not show ancestors, and "+
			"it accepts none of the listing filters — a tree with holes in it would misrepresent the "+
			"decomposition.\n\n"+
			"AWB follows children across workspace boundaries, and the workspace gate still applies: a "+
			"child in a workspace you may not reach is dropped together with its own subtree rather "+
			"than returned. The audit row records how many nodes went.\n\n"+
			"Under --compact each node is the ordinary compact issue line prefixed by two spaces per "+
			"level of depth, the root unindented.",
		"/v1/awb/dep/tree")
}
