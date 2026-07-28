package agent

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

// groupAttachmentCmd is `tclaude agent groups attachment {set,clear,show}` —
// the persistent http(s) reference carried by a group.
func groupAttachmentCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:   "attachment",
		Short: "Manage a group's persistent reference link",
		Long: "Set, clear, or show the persistent http(s) reference attached to a group — " +
			"for example a Linear project, GitHub board, or design document. The dashboard " +
			"renders it as a compact pin beside the group name. Writes require `groups.attachment` " +
			"or ownership of the group.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			groupAttachmentSetCmd(),
			groupAttachmentClearCmd(),
			groupAttachmentShowCmd(),
		},
	}.ToCobra()
}

type groupAttachmentSetParams struct {
	Group    string `pos:"true" help:"Group to configure"`
	URL      string `pos:"true" help:"Reference URL (http(s) only)"`
	Label    string `long:"label" short:"l" optional:"true" help:"Optional display label overriding the auto-derived one (Linear issue key, GitHub number, or hostname)"`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s'). Capped at 300s. Timeout = deny."`
}

func groupAttachmentSetCmd() *cobra.Command {
	return boa.CmdT[groupAttachmentSetParams]{
		Use:         "set",
		Short:       "Set a group's persistent reference link",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *groupAttachmentSetParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Group).SetAlternativesFunc(completeGroupNames)
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *groupAttachmentSetParams, _ *cobra.Command, _ []string) {
			os.Exit(runGroupAttachmentSet(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runGroupAttachmentSet(p *groupAttachmentSetParams, stdout, stderr io.Writer) int {
	group := strings.TrimSpace(p.Group)
	refURL := strings.TrimSpace(p.URL)
	if group == "" || refURL == "" {
		fmt.Fprintln(stderr, "Error: group name and reference URL are required (use `groups attachment clear` to remove one).")
		return rcInvalidArg
	}
	return groupAttachmentWrite(group, p.AskHuman,
		map[string]any{"url": refURL, "label": strings.TrimSpace(p.Label)}, stdout, stderr)
}

type groupAttachmentClearParams struct {
	Group    string `pos:"true" help:"Group to configure"`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s'). Capped at 300s. Timeout = deny."`
}

func groupAttachmentClearCmd() *cobra.Command {
	return boa.CmdT[groupAttachmentClearParams]{
		Use:         "clear",
		Short:       "Clear a group's persistent reference link",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *groupAttachmentClearParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Group).SetAlternativesFunc(completeGroupNames)
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *groupAttachmentClearParams, _ *cobra.Command, _ []string) {
			os.Exit(runGroupAttachmentClear(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runGroupAttachmentClear(p *groupAttachmentClearParams, stdout, stderr io.Writer) int {
	group := strings.TrimSpace(p.Group)
	if group == "" {
		fmt.Fprintln(stderr, "Error: group name is required.")
		return rcInvalidArg
	}
	return groupAttachmentWrite(group, p.AskHuman, map[string]any{"clear": true}, stdout, stderr)
}

func groupAttachmentWrite(group, askHuman string, body map[string]any, stdout, stderr io.Writer) int {
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
	var resp groupAttachmentResp
	path := "/v1/groups/" + url.PathEscape(group) + "/attachment"
	if err := DaemonRequest(http.MethodPost, path, body, &resp, DaemonOpts{AskHuman: ask}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if resp.Cleared {
		fmt.Fprintf(stdout, "%s: attachment cleared\n", resp.Group)
	} else {
		fmt.Fprintf(stdout, "%s: attachment set to %s (%s)\n", resp.Group, resp.URL, resp.Label)
	}
	return rcOK
}

type groupAttachmentShowParams struct {
	Group string `pos:"true" help:"Group to inspect"`
}

func groupAttachmentShowCmd() *cobra.Command {
	return boa.CmdT[groupAttachmentShowParams]{
		Use:         "show",
		Short:       "Show a group's persistent reference link",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *groupAttachmentShowParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Group).SetAlternativesFunc(completeGroupNames)
			return nil
		},
		RunFunc: func(p *groupAttachmentShowParams, _ *cobra.Command, _ []string) {
			os.Exit(runGroupAttachmentShow(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runGroupAttachmentShow(p *groupAttachmentShowParams, stdout, stderr io.Writer) int {
	group := strings.TrimSpace(p.Group)
	if group == "" {
		fmt.Fprintln(stderr, "Error: group name is required.")
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var resp groupAttachmentResp
	if err := DaemonGet("/v1/groups/"+url.PathEscape(group)+"/attachment", &resp); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if resp.URL == "" {
		fmt.Fprintf(stdout, "%s: no attachment set\n", resp.Group)
	} else {
		fmt.Fprintf(stdout, "%s: %s  (%s)\n", resp.Group, resp.URL, resp.Label)
	}
	return rcOK
}

type groupAttachmentResp struct {
	Group         string `json:"group"`
	URL           string `json:"attachment_url,omitempty"`
	Label         string `json:"attachment_label,omitempty"`
	LabelOverride string `json:"attachment_label_override,omitempty"`
	Cleared       bool   `json:"cleared,omitempty"`
}
