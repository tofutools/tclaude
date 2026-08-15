package agent

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/common"
)

type triggerViewJSON struct {
	*db.TriggerRule
	Group   string             `json:"group,omitempty"`
	Firings []db.TriggerFiring `json:"firings,omitempty"`
}

func triggersCmd() *cobra.Command {
	return boa.CmdT[struct{}]{Use: "triggers", Short: "Inspect tclaude-level trigger rules", ParamEnrich: common.DefaultParamEnricher(), SubCmds: []*cobra.Command{triggersLsCmd(), triggersShowCmd(), triggersExplainCmd()}}.ToCobra()
}

func triggersLsCmd() *cobra.Command {
	return boa.CmdT[struct{}]{Use: "ls", Short: "List trigger rules", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(_ *struct{}, _ *cobra.Command, _ []string) { os.Exit(runTriggersLs(os.Stdout, os.Stderr)) }}.ToCobra()
}
func runTriggersLs(stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	rules, err := fetchTriggerList()
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcIOFailure
	}
	if len(rules) == 0 {
		fmt.Fprintln(stdout, "(no triggers)")
		return rcOK
	}
	fmt.Fprintf(stdout, "%-4s  %-20s  %-7s  %-12s  %-18s  %-8s  %s\n", "ID", "NAME", "ENABLED", "SOURCE", "SCOPE", "ACTIONS", "LAST")
	fmt.Fprintln(stdout, strings.Repeat("─", 100))
	for _, view := range rules {
		r := view.TriggerRule
		last := "(never)"
		if len(view.Firings) > 0 {
			last = view.Firings[0].Outcome
		}
		fmt.Fprintf(stdout, "%-4d  %-20s  %-7v  %-12s  %-18s  %-8d  %s\n", r.ID, truncate(r.Name, 20), r.Enabled, r.Source, triggerScopeLabel(r, view.Group), len(r.Actions), last)
	}
	return rcOK
}

type triggersShowParams struct {
	Selector string `pos:"true" help:"Trigger name or numeric id."`
}

func triggersShowCmd() *cobra.Command {
	return boa.CmdT[triggersShowParams]{Use: "show", Short: "Show one trigger and its recent firing ledger", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *triggersShowParams, _ *cobra.Command, _ []string) {
		os.Exit(runTriggersShow(os.Stdout, os.Stderr, p.Selector))
	}}.ToCobra()
}
func runTriggersShow(stdout, stderr io.Writer, selector string) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	view, rc := resolveTrigger(stderr, selector)
	if rc != rcOK {
		return rc
	}
	r := view.TriggerRule
	fmt.Fprintf(stdout, "Trigger:   %s (id %d, revision %d, row version %d)\n", r.Name, r.ID, r.Revision, r.RowVersion)
	owner := "operator"
	if !r.OperatorAuthored {
		owner = r.OwnerAgent
	}
	fmt.Fprintf(stdout, "Owner:     %s\nEnabled:   %v\nScope:     %s\nSource:    %s\nDrafts:    %s\nDebounce:  %s\nCooldown:  %s\n", owner, r.Enabled, triggerScopeLabel(r, view.Group), r.Source, r.DraftFilter, durationLabel(r.DebounceSeconds), durationLabel(r.CooldownSeconds))
	fmt.Fprintln(stdout, "Actions:")
	for i, a := range r.Actions {
		switch a.Type {
		case db.TriggerActionSpawn:
			fmt.Fprintf(stdout, "  %d. spawn profile=%q roles=%v max_live=%d deadline=%s\n", i, a.Spawn.Profile, a.Spawn.RoleRefs, a.Spawn.MaxLiveWorkers, durationLabel(a.Spawn.WorkerDeadlineSeconds))
		case db.TriggerActionMessage:
			fmt.Fprintf(stdout, "  %d. message target=%s subject=%q\n", i, a.Message.Target, a.Message.SubjectTemplate)
		}
	}
	fmt.Fprintln(stdout, "Recent firings:")
	if len(view.Firings) == 0 {
		fmt.Fprintln(stdout, "  (none)")
	}
	for _, f := range view.Firings {
		fmt.Fprintf(stdout, "  %s  %-20s  %s\n", f.StartedAt.Format("2006-01-02 15:04:05"), f.Outcome, f.EventRef)
		for _, a := range f.Actions {
			fmt.Fprintf(stdout, "      %d %-8s %-20s %s\n", a.ActionIndex, a.ActionType, a.Outcome, a.Detail)
		}
	}
	return rcOK
}

type triggersExplainParams struct {
	URL    string `long:"pr-url" help:"PR URL to simulate."`
	Number int    `long:"pr-number" optional:"true"`
	Branch string `long:"pr-branch" optional:"true"`
	Author string `long:"author-agent" help:"Stable author agent id."`
	Group  string `long:"group" optional:"true" help:"Group name or numeric id at open time."`
	Draft  bool   `long:"draft" optional:"true"`
}

func triggersExplainCmd() *cobra.Command {
	return boa.CmdT[triggersExplainParams]{Use: "explain", Short: "Dry-run pr.opened against every trigger", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *triggersExplainParams, _ *cobra.Command, _ []string) {
		os.Exit(runTriggersExplain(os.Stdout, os.Stderr, p))
	}}.ToCobra()
}
func runTriggersExplain(stdout, stderr io.Writer, p *triggersExplainParams) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var resp struct {
		Results []struct {
			RuleID   int64  `json:"rule_id"`
			RuleName string `json:"rule_name"`
			Fire     bool   `json:"fire"`
			Outcome  string `json:"outcome"`
			Detail   string `json:"detail"`
		} `json:"results"`
	}
	body := map[string]any{"pr_url": strings.TrimSpace(p.URL), "pr_number": p.Number, "pr_branch": p.Branch, "author_agent": strings.TrimSpace(p.Author), "group": strings.TrimSpace(p.Group), "draft": p.Draft}
	if err := DaemonRequest(http.MethodPost, "/v1/triggers/explain", body, &resp, DaemonOpts{}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	fmt.Fprintln(stdout, "Dry run: no actions execute and nothing is recorded.")
	for _, result := range resp.Results {
		mark := "—"
		if result.Fire {
			mark = "→"
		}
		fmt.Fprintf(stdout, "%s %-20s %-22s %s\n", mark, result.RuleName, result.Outcome, result.Detail)
	}
	return rcOK
}

func resolveTrigger(stderr io.Writer, selector string) (*triggerViewJSON, int) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		fmt.Fprintln(stderr, "Error: trigger name or id required")
		return nil, rcInvalidArg
	}
	rules, err := fetchTriggerList()
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return nil, rcIOFailure
	}
	var id int64
	id, _ = strconv.ParseInt(selector, 10, 64)
	var found *triggerViewJSON
	for i := range rules {
		if rules[i].Name == selector || (id > 0 && rules[i].ID == id) {
			found = &rules[i]
			break
		}
	}
	if found == nil {
		fmt.Fprintf(stderr, "Error: no trigger %q\n", selector)
		return nil, rcNotFound
	}
	var view triggerViewJSON
	if err := DaemonRequest(http.MethodGet, "/v1/triggers/"+strconv.FormatInt(found.ID, 10), nil, &view, DaemonOpts{}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return nil, MapDaemonErrorToRC(err)
	}
	return &view, rcOK
}

func fetchTriggerList() ([]triggerViewJSON, error) {
	var resp struct {
		Triggers []triggerViewJSON `json:"triggers"`
	}
	err := DaemonRequest(http.MethodGet, "/v1/triggers", nil, &resp, DaemonOpts{})
	return resp.Triggers, err
}
func triggerScopeLabel(r *db.TriggerRule, group string) string {
	if r.ScopeKind == db.TriggerScopeGlobal {
		return "global"
	}
	if group != "" {
		return "group:" + group
	}
	return fmt.Sprintf("group:#%d", r.GroupID)
}
func durationLabel(seconds int64) string {
	if seconds <= 0 {
		return "off"
	}
	return (time.Duration(seconds) * time.Second).String()
}
