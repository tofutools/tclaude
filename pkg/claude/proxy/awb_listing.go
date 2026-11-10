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
// Listings
// ---------------------------------------------------------------------------

// awbFilterValues are the listing filters as VALUES, apart from either of the
// two params structs that collect them.
//
// It exists because `search` cannot embed the filter params: boa registers an
// embedded struct's fields as flags but its GetParamT cannot resolve one, so a
// command whose InitFuncCtx has to touch a filter — to hide it, or to give it a
// completion vocabulary — must own that field directly. `search` and the other
// three therefore declare the same flags twice, and meet here, so that the ONE
// thing that must not drift — what the daemon is actually asked for — is
// written once. TestAWBFilterFlagsAgree pins the two declarations against each
// other.
type awbFilterValues struct {
	Statuses       []string
	IncludeClosed  bool
	Types          []string
	Priorities     []int
	PriorityMax    *int
	Labels         []string
	Assignees      []string
	Mine           bool
	Unassigned     bool
	Workspaces     []string
	LegacyProjects []string
	Parent         string
	Limit          int
	Sort           string
}

// body renders the filters into the daemon request.
func (v awbFilterValues) body(compact bool) map[string]any {
	body := map[string]any{"compact": compact}
	addIfAny := func(key string, values []string) {
		if trimmed := trimmedNonEmptyStrings(values); len(trimmed) > 0 {
			body[key] = trimmed
		}
	}
	addIfAny("statuses", v.Statuses)
	addIfAny("types", v.Types)
	addIfAny("labels", v.Labels)
	addIfAny("assignees", v.Assignees)
	addIfAny("workspaces", v.Workspaces)
	addIfAny("projects", v.LegacyProjects)
	if len(v.Priorities) > 0 {
		body["priorities"] = v.Priorities
	}
	if v.PriorityMax != nil {
		body["priority_max"] = *v.PriorityMax
	}
	if v.IncludeClosed {
		body["include_closed"] = true
	}
	if v.Mine {
		body["mine"] = true
	}
	if v.Unassigned {
		body["unassigned"] = true
	}
	if s := strings.TrimSpace(v.Parent); s != "" {
		body["parent"] = s
	}
	if s := strings.TrimSpace(v.Sort); s != "" {
		body["sort"] = s
	}
	if v.Limit != 0 {
		body["limit"] = v.Limit
	}
	return body
}

// awbFilterParams are the filters `list`, `ready` and `blocked` share,
// mirroring awb's own FilterFlags one for one. `search` declares the same set
// itself — see awbFilterValues for why.
//
// Which filters a verb accepts is NOT uniform, and the difference is awb's:
// `ready` fixes the status set and the assignee filter for itself, and
// `blocked` fixes the status set. A flag a verb does not accept is hidden from
// it entirely rather than accepted and ignored — see hideRejectedFilters — so
// passing one is the usage error it is in awb.
type awbFilterParams struct {
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout. Capped at 300s. Timeout = deny."`
	JSON     bool   `long:"json" optional:"true" help:"Print the stable JSON representation. This is the DEFAULT; the flag exists so an awb command line copies over unchanged."`
	Compact  bool   `long:"compact" optional:"true" help:"Print awb's one terse line per issue instead. Cheapest output there is, and the one to prefer when you only need to see what is there."`
	// Declared after --ask-human on purpose, at the cost of appearing later in
	// --help: boa's enricher hands out single-letter shorthands in field order,
	// taking a name's first letter whenever it is still free, so --assignee
	// placed above --ask-human would silently take -a away from it.
	Statuses      []string `long:"status" optional:"true" help:"Select this status: backlog, open, in_progress or closed. Repeat for several; a value is also split on commas."`
	IncludeClosed bool     `long:"include-closed" optional:"true" help:"Widen whatever status set is in force to include closed issues."`
	Types         []string `long:"type" optional:"true" help:"Select this type: epic, feature, bug, task or chore. Repeat for several."`
	Priorities    []int    `long:"priority" optional:"true" help:"Select this priority exactly, 0 (highest) to 4 (lowest). Repeat for several."`
	PriorityMax   *int     `long:"priority-max" help:"Select issues at least this urgent, inclusive. Reads as urgency rather than as a number: 1 means P0 and P1."`
	Labels        []string `long:"label" optional:"true" help:"Select this label. Repeat for several; a value is also split on commas, which AWB's label charset cannot contain anyway."`
	Assignees     []string `long:"assignee" optional:"true" help:"Select this assignee. Repeat for several. Mutually exclusive with --mine and --unassigned."`
	Mine          bool     `long:"mine" optional:"true" help:"Shorthand for --assignee <the daemon's AWB account>. Note that this is the OPERATOR's user: agents have no AWB identity of their own."`
	Unassigned    bool     `long:"unassigned" optional:"true" help:"Select unassigned issues."`
	Workspaces    []string `long:"workspace" optional:"true" help:"Select this workspace. Repeat for several. Defaults to every workspace you may reach."`
	Parent        string   `long:"parent" optional:"true" help:"Select the direct children of this issue — not the whole subtree, which is 'dep tree'."`
	Limit         int      `long:"limit" optional:"true" help:"Cap the rows returned (1-500, default 50). awb itself returns every row by default; the proxy bounds it, because the rows land in an agent's context."`
	Sort          string   `long:"sort" optional:"true" help:"Ordering, optionally prefixed with \"-\" for descending: order, workspace, status, assignee, blockers, priority, created, updated or id."`
	// Only `ready` offers it (see awbListingOptions.excludeEpic). Declared last
	// so the shorthand it may take cannot displace an existing flag's.
	ExcludeEpic bool `long:"exclude-epic" optional:"true" help:"Select every type except epic. Shorthand for repeating --type with all the others; mutually exclusive with --type."`
}

// values resolves the flags into what the daemon is asked for. --exclude-epic
// is expanded here into the explicit type set, so the daemon and awb see an
// ordinary type filter.
func (p *awbFilterParams) values() awbFilterValues {
	types := p.Types
	if p.ExcludeEpic {
		types = awbNonEpicTypes()
	}
	return awbFilterValues{
		Statuses: p.Statuses, IncludeClosed: p.IncludeClosed, Types: types,
		Priorities: p.Priorities, PriorityMax: p.PriorityMax, Labels: p.Labels,
		Assignees: p.Assignees, Mine: p.Mine, Unassigned: p.Unassigned,
		Workspaces: p.Workspaces, LegacyProjects: p.Workspaces, Parent: p.Parent, Limit: p.Limit, Sort: p.Sort,
	}
}

// awbListingOptions says which filters one listing verb offers, mirroring awb's
// filterOptions.
type awbListingOptions struct {
	status      bool
	assignee    bool
	relevance   bool
	excludeEpic bool
}

// awbSortAlternatives is the ordering vocabulary one verb offers. boa ENFORCES
// an alternatives list rather than merely offering it for completion, so
// `search` — the one verb that can order by relevance — needs its own.
func awbSortAlternatives(relevance bool) []string {
	sorts := []string{
		"order", "-order", "workspace", "-workspace", "status", "-status", "assignee", "-assignee", "blockers", "-blockers", "priority", "-priority", "created", "-created", "updated", "-updated", "id", "-id",
	}
	if relevance {
		sorts = append(sorts, "relevance", "-relevance")
	}
	return sorts
}

var (
	awbStatusAlternatives = []string{"backlog", "open", "in_progress", "closed"}
	awbTypeAlternatives   = []string{"epic", "feature", "bug", "task", "chore"}
)

// awbNonEpicTypes is every issue type but epic: what --exclude-epic selects.
func awbNonEpicTypes() []string {
	types := make([]string, 0, len(awbTypeAlternatives)-1)
	for _, t := range awbTypeAlternatives {
		if t != "epic" {
			types = append(types, t)
		}
	}
	return types
}

// checkFilterCombination refuses flag combinations that contradict each other.
func (p *awbFilterParams) checkFilterCombination(stderr io.Writer) int {
	if p.ExcludeEpic && len(trimmedNonEmptyStrings(p.Types)) > 0 {
		fmt.Fprintln(stderr, "Error: --exclude-epic and --type are mutually exclusive.")
		return rcInvalidArg
	}
	return rcOK
}

// hideRejectedFilters takes the flags a verb does not accept off its command
// entirely.
//
// Hidden rather than accepted-and-ignored, which is what awb does and for the
// same reason: `ready --mine` is a question `ready` cannot answer, and a flag
// that parses but changes nothing is worse than one that does not parse. The
// daemon refuses them independently regardless, because a check made in this
// process is a check the caller could have skipped.
func hideRejectedFilters(ctx *boa.HookContext, f *awbFilterParams, opts awbListingOptions) {
	boa.GetParamT(ctx, &f.AskHuman).SetAlternativesFunc(agent.CompleteAskHumanDurations)
	boa.GetParamT(ctx, &f.Types).SetAlternatives(awbTypeAlternatives)
	boa.GetParamT(ctx, &f.Sort).SetAlternatives(awbSortAlternatives(opts.relevance))
	if opts.status {
		boa.GetParamT(ctx, &f.Statuses).SetAlternatives(awbStatusAlternatives)
	} else {
		boa.GetParamT(ctx, &f.Statuses).SetIgnored(true)
		boa.GetParamT(ctx, &f.IncludeClosed).SetIgnored(true)
	}
	if !opts.excludeEpic {
		boa.GetParamT(ctx, &f.ExcludeEpic).SetIgnored(true)
	}
	if !opts.assignee {
		boa.GetParamT(ctx, &f.Assignees).SetIgnored(true)
		boa.GetParamT(ctx, &f.Mine).SetIgnored(true)
		boa.GetParamT(ctx, &f.Unassigned).SetIgnored(true)
	}
}

func awbListingCmd(use, short, long, path string, opts awbListingOptions) *cobra.Command {
	return boa.CmdT[awbFilterParams]{
		Use:         use,
		Short:       short,
		Long:        long,
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *awbFilterParams, _ *cobra.Command) error {
			hideRejectedFilters(ctx, p, opts)
			return nil
		},
		RunFunc: func(p *awbFilterParams, _ *cobra.Command, _ []string) {
			compact, rc := awbOutputMode(p.JSON, p.Compact, os.Stderr)
			if rc != rcOK {
				os.Exit(rc)
			}
			if rc := p.checkFilterCombination(os.Stderr); rc != rcOK {
				os.Exit(rc)
			}
			os.Exit(awbProxyCall(path, p.values().body(compact), p.AskHuman, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func awbListCmd() *cobra.Command {
	return awbListingCmd("list", "List issues",
		"List issues across every workspace you may reach, or some of them with --workspace.\n\n"+
			"By default closed ones are hidden; --include-closed widens whatever status set is in "+
			"force.",
		"/v1/awb/issue/list", awbListingOptions{status: true, assignee: true})
}

func awbReadyCmd() *cobra.Command {
	return awbListingCmd("ready", "List ready issues, highest priority first",
		"An issue is ready when it is open and not blocked.\n\n"+
			"ready lists only unassigned issues, because \"what should nobody-in-particular pick up "+
			"next\" is the question it exists to answer. It therefore takes no assignee filter and no "+
			"status filter: which issues you hold is `list --mine`.\n\n"+
			"--exclude-epic leaves epics out, since an epic is usually decomposed rather than "+
			"picked up directly.\n\n"+
			"This is the primary entry point. Start here.",
		"/v1/awb/issue/ready", awbListingOptions{excludeEpic: true})
}

func awbBlockedCmd() *cobra.Command {
	return awbListingCmd("blocked", "List issues that are not closed and are blocked",
		"List blocked issues, each with the ids of the issues blocking it.\n\n"+
			"Under --compact every line carries its blockers as blocked-by:<id> tokens, which is the "+
			"one thing this listing exists to show.",
		"/v1/awb/issue/blocked", awbListingOptions{assignee: true})
}

// awbSearchParams repeats awbFilterParams' flags rather than embedding them.
// See awbFilterValues for why that is not a slip.
type awbSearchParams struct {
	Terms    []string `pos:"true" help:"Literal search terms. An issue matches when its title and description together contain all of them."`
	AskHuman string   `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout. Capped at 300s. Timeout = deny."`
	JSON     bool     `long:"json" optional:"true" help:"Print the stable JSON representation. This is the DEFAULT; the flag exists so an awb command line copies over unchanged."`
	Compact  bool     `long:"compact" optional:"true" help:"Print awb's one terse line per issue instead. Cheapest output there is, and the one to prefer when you only need to see what is there."`

	Statuses      []string `long:"status" optional:"true" help:"Select this status: backlog, open, in_progress or closed. Repeat for several; a value is also split on commas."`
	IncludeClosed bool     `long:"include-closed" optional:"true" help:"Widen whatever status set is in force to include closed issues."`
	Types         []string `long:"type" optional:"true" help:"Select this type: epic, feature, bug, task or chore. Repeat for several."`
	Priorities    []int    `long:"priority" optional:"true" help:"Select this priority exactly, 0 (highest) to 4 (lowest). Repeat for several."`
	PriorityMax   *int     `long:"priority-max" help:"Select issues at least this urgent, inclusive. Reads as urgency rather than as a number: 1 means P0 and P1."`
	Labels        []string `long:"label" optional:"true" help:"Select this label. Repeat for several; a value is also split on commas, which AWB's label charset cannot contain anyway."`
	Assignees     []string `long:"assignee" optional:"true" help:"Select this assignee. Repeat for several. Mutually exclusive with --mine and --unassigned."`
	Mine          bool     `long:"mine" optional:"true" help:"Shorthand for --assignee <the daemon's AWB account>. Note that this is the OPERATOR's user: agents have no AWB identity of their own."`
	Unassigned    bool     `long:"unassigned" optional:"true" help:"Select unassigned issues."`
	Workspaces    []string `long:"workspace" optional:"true" help:"Select this workspace. Repeat for several. Defaults to every workspace you may reach."`
	Parent        string   `long:"parent" optional:"true" help:"Select the direct children of this issue — not the whole subtree, which is 'dep tree'."`
	Limit         int      `long:"limit" optional:"true" help:"Cap the rows returned (1-500, default 50). awb itself returns every row by default; the proxy bounds it, because the rows land in an agent's context."`
	Sort          string   `long:"sort" optional:"true" help:"Ordering, optionally prefixed with \"-\" for descending: relevance, order, workspace, status, assignee, blockers, priority, created, updated or id. Defaults to relevance."`
}

func (p *awbSearchParams) values() awbFilterValues {
	return awbFilterValues{
		Statuses: p.Statuses, IncludeClosed: p.IncludeClosed, Types: p.Types,
		Priorities: p.Priorities, PriorityMax: p.PriorityMax, Labels: p.Labels,
		Assignees: p.Assignees, Mine: p.Mine, Unassigned: p.Unassigned,
		Workspaces: p.Workspaces, LegacyProjects: p.Workspaces, Parent: p.Parent, Limit: p.Limit, Sort: p.Sort,
	}
}

func awbSearchCmd() *cobra.Command {
	return boa.CmdT[awbSearchParams]{
		Use:   "search",
		Short: "Full text search over title and description",
		Long: "Search titles and descriptions. Each argument is a literal term: no operator, wildcard " +
			"or column prefix is passed through, so no input can produce a query syntax error.\n\n" +
			"Matching is by whole token and is case- and diacritic-insensitive, with no stemming and " +
			"no prefix matching: \"parser\" finds \"Parser\" and \"parser,\" but neither \"pars\" nor " +
			"\"parsers\" finds \"parser\". Widen the terms rather than the syntax.\n\n" +
			"The workspace gate applies, so a search cannot reach outside it.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *awbSearchParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(agent.CompleteAskHumanDurations)
			boa.GetParamT(ctx, &p.Statuses).SetAlternatives(awbStatusAlternatives)
			boa.GetParamT(ctx, &p.Types).SetAlternatives(awbTypeAlternatives)
			boa.GetParamT(ctx, &p.Sort).SetAlternatives(awbSortAlternatives(true))
			return nil
		},
		RunFunc: func(p *awbSearchParams, _ *cobra.Command, _ []string) {
			compact, rc := awbOutputMode(p.JSON, p.Compact, os.Stderr)
			if rc != rcOK {
				os.Exit(rc)
			}
			body := p.values().body(compact)
			terms := trimmedNonEmptyStrings(p.Terms)
			if len(terms) == 0 {
				fmt.Fprintln(os.Stderr, "Error: search needs at least one term.")
				os.Exit(rcInvalidArg)
			}
			body["terms"] = terms
			os.Exit(awbProxyCall("/v1/awb/issue/search", body, p.AskHuman, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}
