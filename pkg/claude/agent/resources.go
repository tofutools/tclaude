package agent

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

// `tclaude agent resources` — a group's resources (its boards, the process
// templates pinned to it, its live pages) and their locks. Listing needs
// groups.resources.read in the group, which membership confers; locking and
// unlocking need groups.resources.lock in every group the resource belongs
// to, which only the operator grants.

func resourcesCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:   "resources",
		Short: "List a group's resources and lock or unlock them",
		Long: "A group's resources are its boards, the process templates pinned to it and its live pages. " +
			"Its members read and change them by default (groups.resources.read / groups.resources.write, conferred by membership).\n\n" +
			"A locked resource stays readable, but no agent may change it, except one holding groups.resources.lock for it. " +
			"`lock` and `unlock` need groups.resources.lock in every group the resource belongs to; the operator grants it, " +
			"and also locks and unlocks from the dashboard.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds:     []*cobra.Command{resourcesLsCmd(), resourcesLockCmd(true), resourcesLockCmd(false)},
	}.ToCobra()
}

type resourcesLsParams struct {
	Group    string `pos:"true" help:"The group whose resources to list"`
	JSON     bool   `long:"json" help:"Print the daemon's JSON"`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s'). Capped at 300s. Timeout = deny."`
}

func resourcesLsCmd() *cobra.Command {
	return boa.CmdT[resourcesLsParams]{
		Use:         "ls",
		Short:       "List a group's processes, boards and pages, with their locks",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *resourcesLsParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *resourcesLsParams, _ *cobra.Command, _ []string) {
			os.Exit(runResourcesLs(p, cliStdout, cliStderr))
		},
	}.ToCobra()
}

type groupResourcesResp struct {
	Group     string `json:"group"`
	Processes []struct {
		TemplateID   string `json:"templateId"`
		Name         string `json:"name"`
		AgentsLocked bool   `json:"agentsLocked"`
	} `json:"processes"`
	Boards []struct {
		ID           string `json:"id"`
		Title        string `json:"title"`
		AgentsLocked bool   `json:"agentsLocked"`
	} `json:"boards"`
	Pages []struct {
		ID           string `json:"id"`
		Name         string `json:"name"`
		Owner        string `json:"owner"`
		State        string `json:"state"`
		AgentsLocked bool   `json:"agentsLocked"`
	} `json:"pages"`
	CanLock bool `json:"canLock"`
}

func runResourcesLs(p *resourcesLsParams, stdout, stderr io.Writer) int {
	ask, err := ParseAskHuman(p.AskHuman)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcInvalidArg
	}
	var resp groupResourcesResp
	path := "/v1/groups/" + url.PathEscape(strings.TrimSpace(p.Group)) + "/resources"
	if err := DaemonRequest(http.MethodGet, path, nil, &resp, DaemonOpts{AskHuman: ask}); err != nil {
		return boardErr(stderr, err)
	}
	if p.JSON {
		return writeBoardJSON(stdout, resp)
	}
	lock := func(locked bool) string {
		if locked {
			return "locked"
		}
		return "-"
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "KIND\tID\tNAME\tLOCKED")
	for _, r := range resp.Processes {
		fmt.Fprintf(tw, "process\t%s\t%s\t%s\n", r.TemplateID, r.Name, lock(r.AgentsLocked))
	}
	for _, r := range resp.Boards {
		fmt.Fprintf(tw, "board\t%s\t%s\t%s\n", r.ID, r.Title, lock(r.AgentsLocked))
	}
	for _, r := range resp.Pages {
		name := r.Owner + "/" + r.Name
		if r.State != "" && r.State != "open" {
			name += " (" + r.State + ")"
		}
		fmt.Fprintf(tw, "page\t%s\t%s\t%s\n", r.ID, name, lock(r.AgentsLocked))
	}
	_ = tw.Flush()
	if len(resp.Processes)+len(resp.Boards)+len(resp.Pages) == 0 {
		fmt.Fprintf(stdout, "Group %s has no resources.\n", resp.Group)
	}
	if resp.CanLock {
		fmt.Fprintln(stdout, "You hold groups.resources.lock here: `tclaude agent resources lock|unlock <kind> <id>`.")
	}
	return rcOK
}

type resourcesLockParams struct {
	Kind     string `pos:"true" help:"process, board or page"`
	ID       string `pos:"true" help:"The template id, board id or page id (as resources ls lists it)"`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s'). Capped at 300s. Timeout = deny."`
}

func resourcesLockCmd(lock bool) *cobra.Command {
	use, short := "lock", "Lock a resource against agent changes (needs groups.resources.lock)"
	if !lock {
		use, short = "unlock", "Unlock a resource (needs groups.resources.lock)"
	}
	return boa.CmdT[resourcesLockParams]{
		Use:         use,
		Short:       short,
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *resourcesLockParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Kind).SetAlternatives([]string{"process", "board", "page"})
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *resourcesLockParams, _ *cobra.Command, _ []string) {
			os.Exit(runResourcesLock(p, lock, cliStdout, cliStderr))
		},
	}.ToCobra()
}

func runResourcesLock(p *resourcesLockParams, lock bool, stdout, stderr io.Writer) int {
	ask, err := ParseAskHuman(p.AskHuman)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcInvalidArg
	}
	kind := strings.TrimSpace(p.Kind)
	switch kind {
	case "process", "board", "page":
	default:
		fmt.Fprintln(stderr, "Error: kind must be process, board or page")
		return rcInvalidArg
	}
	body := map[string]any{"kind": kind, "id": strings.TrimSpace(p.ID), "locked": lock}
	if err := DaemonRequest(http.MethodPost, "/v1/resource-locks", body, nil, DaemonOpts{AskHuman: ask}); err != nil {
		return boardErr(stderr, err)
	}
	if lock {
		fmt.Fprintf(stdout, "Locked %s %s: agents can still read it, but only holders of groups.resources.lock may change it.\n", kind, p.ID)
	} else {
		fmt.Fprintf(stdout, "Unlocked %s %s.\n", kind, p.ID)
	}
	return rcOK
}
