package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/common/table"
	"github.com/tofutools/tclaude/pkg/common"
)

// linkEntry mirrors the daemon's linkJSON payload.
type linkEntry struct {
	ID        int64  `json:"id"`
	From      string `json:"from"`
	To        string `json:"to"`
	Mode      string `json:"mode"`
	CreatedAt string `json:"created_at"`
	ByConv    string `json:"by_conv,omitempty"`
}

// --- groups link (parent) ---

func groupsLinkCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:   "link",
		Short: "Manage inter-group communication links (directed edges between groups)",
		Long: "An inter-group link enables `tclaude agent message` from one " +
			"group's members (or owners) to another group's members, without " +
			"requiring co-membership or owner bridging. Links are directional. " +
			"Mutating subcommands need permission slugs `groups.link.add` / " +
			"`groups.link.rm` — an owner of the FROM group passes without the " +
			"slug. Read subcommands are open.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			groupsLinkLsCmd(),
			groupsLinkAddCmd(),
			groupsLinkSetModeCmd(),
			groupsLinkRmCmd(),
		},
	}.ToCobra()
}

// --- groups links (overview across all groups) ---

type groupsLinksAllParams struct {
	JSON bool `long:"json" help:"Output JSON"`
}

func groupsLinksAllCmd() *cobra.Command {
	return boa.CmdT[groupsLinksAllParams]{
		Use:         "links",
		Short:       "List every inter-group link across all groups",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *groupsLinksAllParams, _ *cobra.Command, _ []string) {
			os.Exit(runGroupsLinksAll(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runGroupsLinksAll(p *groupsLinksAllParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var links []linkEntry
	if err := DaemonGet("/v1/links", &links); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	return renderLinks(links, p.JSON, stdout)
}

// --- groups link ls ---

type groupsLinkLsParams struct {
	Group string `pos:"true" help:"Group name (or numeric id)"`
	Dir   string `long:"dir" optional:"true" help:"out | in | both (default both)"`
	JSON  bool   `long:"json" help:"Output JSON"`
}

func groupsLinkLsCmd() *cobra.Command {
	return boa.CmdT[groupsLinkLsParams]{
		Use:         "ls",
		Short:       "List inter-group links touching the given group",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *groupsLinkLsParams, _ *cobra.Command, _ []string) {
			os.Exit(runGroupsLinkLs(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runGroupsLinkLs(p *groupsLinkLsParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	dir := strings.TrimSpace(p.Dir)
	if dir == "" {
		dir = "both"
	}
	switch dir {
	case "out", "in", "both":
	default:
		fmt.Fprintf(stderr, "Error: --dir must be one of: out, in, both\n")
		return rcInvalidArg
	}
	path := "/v1/groups/" + url.PathEscape(p.Group) + "/links?dir=" + dir
	var links []linkEntry
	if err := DaemonGet(path, &links); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	return renderLinks(links, p.JSON, stdout)
}

func renderLinks(links []linkEntry, asJSON bool, stdout io.Writer) int {
	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(links); err != nil {
			return rcIOFailure
		}
		return rcOK
	}
	if len(links) == 0 {
		fmt.Fprintln(stdout, "(no links)")
		return rcOK
	}
	tbl := table.New(
		table.Column{Header: "ID", Width: 6, Align: table.AlignRight},
		table.Column{Header: "FROM", MinWidth: 8, Weight: 0.8, Truncate: true},
		table.Column{Header: "TO", MinWidth: 8, Weight: 0.8, Truncate: true},
		table.Column{Header: "MODE", Width: 18},
		table.Column{Header: "CREATED", Width: 20},
	)
	tbl.SetTerminalWidth(table.GetTerminalWidth())
	for _, l := range links {
		tbl.AddRow(table.Row{Cells: []string{
			fmt.Sprintf("%d", l.ID),
			l.From, l.To, l.Mode, l.CreatedAt,
		}})
	}
	fmt.Fprintln(stdout, tbl.Render())
	return rcOK
}

// --- groups link add ---

type groupsLinkAddParams struct {
	From     string `pos:"true" help:"Source group (members of this group can message into 'to')"`
	To       string `pos:"true" help:"Destination group"`
	Mode     string `long:"mode" optional:"true" help:"members->members (default) | owners->members"`
	Bidir    bool   `long:"bidir" help:"Also create the reverse link (TO → FROM)"`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout."`
	JSON     bool   `long:"json" help:"Output JSON"`
}

func groupsLinkAddCmd() *cobra.Command {
	return boa.CmdT[groupsLinkAddParams]{
		Use:         "add",
		Short:       "Add an inter-group communication link",
		Long:        "Members of FROM may message members of TO (subject to mode). Requires `groups.link.add` for agents not owning the FROM group.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *groupsLinkAddParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *groupsLinkAddParams, _ *cobra.Command, _ []string) {
			os.Exit(runGroupsLinkAdd(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runGroupsLinkAdd(p *groupsLinkAddParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	body := map[string]any{"to": p.To}
	if mode := strings.TrimSpace(p.Mode); mode != "" {
		body["mode"] = mode
	}
	if p.Bidir {
		body["bidir"] = true
	}
	var resp map[string]any
	dur, err := ParseAskHuman(p.AskHuman)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcInvalidArg
	}
	opts := DaemonOpts{AskHuman: dur}
	path := "/v1/groups/" + url.PathEscape(p.From) + "/links"
	if err := DaemonRequest("POST", path, body, &resp, opts); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if p.JSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(resp); err != nil {
			return rcIOFailure
		}
		return rcOK
	}
	fmt.Fprintf(stdout, "linked %v → %v (link id %v)\n", resp["from"], resp["to"], resp["id"])
	if rev, ok := resp["reverse_id"]; ok {
		fmt.Fprintf(stdout, "  + reverse link: %v\n", rev)
	}
	// Partial-failure: forward succeeded, reverse failed (and wasn't
	// a benign already-exists). Surface a non-zero rc so automation
	// can distinguish this from a clean success.
	if revErr, ok := resp["reverse_error"]; ok {
		fmt.Fprintf(stderr, "  (reverse link failed: %v)\n", revErr)
		return rcIOFailure
	}
	return rcOK
}

// --- groups link set-mode ---

type groupsLinkSetModeParams struct {
	Group    string `pos:"true" help:"Group the link is scoped to (FROM or TO side)"`
	ID       string `pos:"true" help:"Link id (numeric, from 'groups link ls')"`
	Mode     string `pos:"true" help:"New mode: members->members | owners->members"`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout."`
}

func groupsLinkSetModeCmd() *cobra.Command {
	return boa.CmdT[groupsLinkSetModeParams]{
		Use:         "set-mode",
		Short:       "Change the mode of an existing link",
		Long:        "Only the link's mode is mutable; from/to are immutable (re-pointing an edge is delete + re-add). Requires `groups.link.add` for agents not owning the FROM group.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *groupsLinkSetModeParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *groupsLinkSetModeParams, _ *cobra.Command, _ []string) {
			os.Exit(runGroupsLinkSetMode(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runGroupsLinkSetMode(p *groupsLinkSetModeParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	id, err := strconv.ParseInt(strings.TrimSpace(p.ID), 10, 64)
	if err != nil {
		fmt.Fprintf(stderr, "Error: link id must be numeric, got %q\n", p.ID)
		return rcInvalidArg
	}
	dur, err := ParseAskHuman(p.AskHuman)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcInvalidArg
	}
	opts := DaemonOpts{AskHuman: dur}
	path := "/v1/groups/" + url.PathEscape(p.Group) + "/links/" + strconv.FormatInt(id, 10)
	body := map[string]any{"mode": p.Mode}
	var resp map[string]any
	if err := DaemonRequest("PATCH", path, body, &resp, opts); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if changed, _ := resp["changed"].(bool); changed {
		fmt.Fprintf(stdout, "link %d mode set to %s\n", id, p.Mode)
	} else {
		fmt.Fprintf(stdout, "link %d already has mode %s; no change\n", id, p.Mode)
	}
	return rcOK
}

// --- groups link rm ---

type groupsLinkRmParams struct {
	Group    string `pos:"true" help:"Group the link is scoped to (FROM or TO side)"`
	ID       string `pos:"true" help:"Link id (numeric, from 'groups link ls')"`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout."`
}

func groupsLinkRmCmd() *cobra.Command {
	return boa.CmdT[groupsLinkRmParams]{
		Use:         "rm",
		Short:       "Remove an inter-group communication link",
		Long:        "Pass the group the link is scoped under (either endpoint) and the link's numeric id. Requires `groups.link.rm` for agents not owning the FROM group.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *groupsLinkRmParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *groupsLinkRmParams, _ *cobra.Command, _ []string) {
			os.Exit(runGroupsLinkRm(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runGroupsLinkRm(p *groupsLinkRmParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	id, err := strconv.ParseInt(strings.TrimSpace(p.ID), 10, 64)
	if err != nil {
		fmt.Fprintf(stderr, "Error: link id must be numeric, got %q\n", p.ID)
		return rcInvalidArg
	}
	dur, err := ParseAskHuman(p.AskHuman)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcInvalidArg
	}
	opts := DaemonOpts{AskHuman: dur}
	path := "/v1/groups/" + url.PathEscape(p.Group) + "/links/" + strconv.FormatInt(id, 10)
	if err := DaemonRequest("DELETE", path, nil, nil, opts); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	fmt.Fprintf(stdout, "removed link %d\n", id)
	return rcOK
}

// --- groups why-can-i-message ---

type whyCanMessageParams struct {
	Target string `pos:"true" help:"Target conv-id or display title"`
	From   string `long:"from" optional:"true" help:"Override the sender; defaults to caller's conv-id"`
	JSON   bool   `long:"json" help:"Output JSON"`
}

func groupsWhyCanMessageCmd() *cobra.Command {
	return boa.CmdT[whyCanMessageParams]{
		Use:         "why-can-i-message",
		Short:       "Explain which group / link authorises (or fails to authorise) a send to this target",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *whyCanMessageParams, _ *cobra.Command, _ []string) {
			os.Exit(runWhyCanMessage(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

type canMessageResp struct {
	Allowed  bool   `json:"allowed"`
	Reason   string `json:"reason,omitempty"`
	ViaGroup string `json:"via_group,omitempty"`
	LinkID   int64  `json:"link_id,omitempty"`
	Message  string `json:"message,omitempty"`
}

func runWhyCanMessage(p *whyCanMessageParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	q := url.Values{}
	q.Set("to", p.Target)
	if from := strings.TrimSpace(p.From); from != "" {
		q.Set("from", from)
	}
	var resp canMessageResp
	if err := DaemonGet("/v1/can-message?"+q.Encode(), &resp); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if p.JSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(resp); err != nil {
			return rcIOFailure
		}
		return rcOK
	}
	if !resp.Allowed {
		fmt.Fprintln(stdout, "allowed: no")
		if resp.Message != "" {
			fmt.Fprintf(stdout, "reason:  %s\n", resp.Message)
		}
		return rcOK
	}
	fmt.Fprintln(stdout, "allowed: yes")
	fmt.Fprintf(stdout, "reason:  %s\n", resp.Reason)
	fmt.Fprintf(stdout, "via:     %s\n", resp.ViaGroup)
	if resp.LinkID != 0 {
		fmt.Fprintf(stdout, "link:    #%d\n", resp.LinkID)
	}
	return rcOK
}
