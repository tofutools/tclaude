package agent

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

// tags.go is `tclaude agent tags {set,add,rm,show}` — the per-agent tag
// set rendered as chips in the dashboard's Description column. Tags are
// short labels keyed on the stable agent-id, so they follow the actor
// across groups and reincarnations. One kind is auto-stamped:
// `tf:<template>` marks which task-force / template deployment spawned an
// agent (JOH-380).
//
// The daemon's tag endpoint is a REPLACE-SET (PUT {"tags": [...]}); this
// CLI composes add/rm on top of it — it GETs the current set, mutates it,
// and PUTs the result. `set` replaces outright.
//
// By default the target is the calling agent itself (requires
// `self.tags`). `--target <selector>` acts on ANOTHER agent — the manager
// pattern (requires `agent.tags`, or being an owner of a group containing
// the target). This mirrors `tclaude agent task`.
func tagsCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:   "tags",
		Short: "Manage an agent's tags (the dashboard Description-column chips)",
		Long: "Set, add, remove, or show an agent's tags — short labels rendered as chips " +
			"in the dashboard's Description column (e.g. the auto-stamped tf:<template> task-force " +
			"marker). By default operates on the calling agent (requires `self.tags`); use " +
			"--target <selector> to act on ANOTHER agent — requires `agent.tags`, or being an owner " +
			"of a group containing the target.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			tagsSetCmd(),
			tagsAddCmd(),
			tagsRmCmd(),
			tagsShowCmd(),
		},
	}.ToCobra()
}

// tagsResp is the shared wire shape of the tag endpoints.
type tagsResp struct {
	ConvID        string   `json:"conv_id"`
	Tags          []string `json:"tags"`
	CallerConv    string   `json:"caller_conv,omitempty"`
	CallerAgentID string   `json:"caller_agent_id,omitempty"`
}

// --- tags set ---

type tagsSetParams struct {
	Tags     []string `pos:"true" help:"The complete tag set to store (replaces any existing tags). Pass zero tags to clear."`
	Target   string   `long:"target" optional:"true" help:"Act on ANOTHER agent instead of self. Selector: title, full conv-id, or 8+-char prefix. Requires agent.tags, or owning a group containing the target."`
	AskHuman string   `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s' or '60'). Capped at 300s. Timeout = deny. Self-target only."`
}

func tagsSetCmd() *cobra.Command {
	return boa.CmdT[tagsSetParams]{
		Use:         "set",
		Short:       "Replace an agent's tag set with the given tags",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *tagsSetParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Target).SetAlternativesFunc(completeConvSelectors)
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *tagsSetParams, _ *cobra.Command, _ []string) {
			os.Exit(runTagsSet(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runTagsSet(p *tagsSetParams, stdout, stderr io.Writer) int {
	return tagsWrite(p.Target, p.AskHuman, cleanTagList(p.Tags), stdout, stderr)
}

// --- tags add ---

type tagsAddParams struct {
	Tags     []string `pos:"true" help:"One or more tags to add (unioned onto the existing set)."`
	Target   string   `long:"target" optional:"true" help:"Act on ANOTHER agent instead of self. Requires agent.tags, or owning a group containing the target."`
	AskHuman string   `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout. Capped at 300s. Timeout = deny. Self-target only."`
}

func tagsAddCmd() *cobra.Command {
	return boa.CmdT[tagsAddParams]{
		Use:         "add",
		Short:       "Add tags to an agent (union with the existing set)",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *tagsAddParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Target).SetAlternativesFunc(completeConvSelectors)
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *tagsAddParams, _ *cobra.Command, _ []string) {
			os.Exit(runTagsAdd(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runTagsAdd(p *tagsAddParams, stdout, stderr io.Writer) int {
	add := cleanTagList(p.Tags)
	if len(add) == 0 {
		fmt.Fprintln(stderr, "Error: at least one tag is required (use `tags set` with no tags to clear).")
		return rcInvalidArg
	}
	cur, rc := tagsRead(p.Target, stderr)
	if rc != rcOK {
		return rc
	}
	return tagsWrite(p.Target, p.AskHuman, unionTags(cur, add), stdout, stderr)
}

// --- tags rm ---

type tagsRmParams struct {
	Tags     []string `pos:"true" help:"One or more tags to remove from the set (tags not present are ignored)."`
	Target   string   `long:"target" optional:"true" help:"Act on ANOTHER agent instead of self. Requires agent.tags, or owning a group containing the target."`
	AskHuman string   `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout. Capped at 300s. Timeout = deny. Self-target only."`
}

func tagsRmCmd() *cobra.Command {
	return boa.CmdT[tagsRmParams]{
		Use:         "rm",
		Short:       "Remove tags from an agent",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *tagsRmParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Target).SetAlternativesFunc(completeConvSelectors)
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *tagsRmParams, _ *cobra.Command, _ []string) {
			os.Exit(runTagsRm(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runTagsRm(p *tagsRmParams, stdout, stderr io.Writer) int {
	rm := cleanTagList(p.Tags)
	if len(rm) == 0 {
		fmt.Fprintln(stderr, "Error: at least one tag is required.")
		return rcInvalidArg
	}
	cur, rc := tagsRead(p.Target, stderr)
	if rc != rcOK {
		return rc
	}
	return tagsWrite(p.Target, p.AskHuman, subtractTags(cur, rm), stdout, stderr)
}

// --- tags show ---

type tagsShowParams struct {
	Target string `long:"target" optional:"true" help:"Show ANOTHER agent's tags instead of self. Requires agent.tags, or owning a group containing the target."`
}

func tagsShowCmd() *cobra.Command {
	return boa.CmdT[tagsShowParams]{
		Use:         "show",
		Short:       "Show an agent's tags",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *tagsShowParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Target).SetAlternativesFunc(completeConvSelectors)
			return nil
		},
		RunFunc: func(p *tagsShowParams, _ *cobra.Command, _ []string) {
			os.Exit(runTagsShow(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runTagsShow(p *tagsShowParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var resp tagsResp
	if err := DaemonGet(tagsPath(p.Target), &resp); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if len(resp.Tags) == 0 {
		fmt.Fprintf(stdout, "%s: no tags set\n", short(resp.ConvID))
		return rcOK
	}
	fmt.Fprintf(stdout, "%s: %s\n", short(resp.ConvID), strings.Join(resp.Tags, ", "))
	return rcOK
}

// --- shared helpers ---

// tagsPath returns the read/write endpoint for the given target (self
// when empty).
func tagsPath(target string) string {
	if t := strings.TrimSpace(target); t != "" {
		return "/v1/agent/" + url.PathEscape(t) + "/tags"
	}
	return "/v1/whoami/tags"
}

// tagsRead GETs the target agent's current tag set — the read half of the
// add/rm read-modify-write. On failure it prints the error and returns a
// non-OK rc so the caller returns.
func tagsRead(target string, stderr io.Writer) ([]string, int) {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return nil, rc
	}
	var resp tagsResp
	if err := DaemonGet(tagsPath(target), &resp); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return nil, MapDaemonErrorToRC(err)
	}
	return resp.Tags, rcOK
}

// tagsWrite PUTs the given (already-composed) tag set to the target and
// renders the result. Shared by set/add/rm. --ask-human is self-only
// (cross-agent calls need a real slug grant or group ownership).
func tagsWrite(target, askHuman string, tags []string, stdout, stderr io.Writer) int {
	target = strings.TrimSpace(target)
	if target != "" && askHuman != "" {
		fmt.Fprintln(stderr, "Error: --ask-human is only supported when targeting self; cross-agent calls require an explicit slug grant or group ownership.")
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	ask, err := ParseAskHuman(askHuman)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcInvalidArg
	}
	if ask > 0 {
		fmt.Fprintf(stdout, "Waiting up to %s for human approval...\n", ask)
	}
	// tags is non-nil so it always marshals as [] (a clear), never null.
	if tags == nil {
		tags = []string{}
	}
	var resp tagsResp
	if err := DaemonRequest(http.MethodPut, tagsPath(target), map[string]any{"tags": tags}, &resp, DaemonOpts{AskHuman: ask}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	printTagsResult(stdout, &resp)
	return rcOK
}

func printTagsResult(stdout io.Writer, resp *tagsResp) {
	by := ""
	if resp.CallerConv != "" {
		by = " (by " + shortAgentID(resp.CallerAgentID, resp.CallerConv) + ")"
	}
	if len(resp.Tags) == 0 {
		fmt.Fprintf(stdout, "Cleared tags for %s%s\n", short(resp.ConvID), by)
		return
	}
	fmt.Fprintf(stdout, "Tags for %s: %s%s\n", short(resp.ConvID), strings.Join(resp.Tags, ", "), by)
}

// cleanTagList trims each tag and drops the empties, preserving order.
// Validation (charset, length, count) is the daemon's job; this only
// keeps a stray "" from an over-split argv out of the request.
func cleanTagList(in []string) []string {
	out := make([]string, 0, len(in))
	for _, t := range in {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// unionTags returns the sorted union of cur and add.
func unionTags(cur, add []string) []string {
	set := map[string]bool{}
	for _, t := range cur {
		set[t] = true
	}
	for _, t := range add {
		set[t] = true
	}
	return sortedKeys(set)
}

// subtractTags returns cur with every tag in rm removed, sorted.
func subtractTags(cur, rm []string) []string {
	drop := map[string]bool{}
	for _, t := range rm {
		drop[t] = true
	}
	out := make([]string, 0, len(cur))
	for _, t := range cur {
		if !drop[t] {
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
