package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/table"
	"github.com/tofutools/tclaude/pkg/common"
)

// permissionsCmd is `tclaude agent permissions …`. Read-only subcommands
// (`ls`, `slugs`) are open. Mutating subcommands (`grant`, `revoke`) are
// gated server-side on `permissions.grant` / `permissions.revoke` —
// humans always pass; agents need the slug granted in
// agent.default_permissions or agent.permission_overrides in
// ~/.tclaude/config.json. By default no agent holds these, so the
// commands are effectively human-only.
//
// `default` is a magic target that means "modify the defaults list"
// rather than a per-conv override. Anywhere `<target>` appears below,
// you can pass `default` or a conv selector (UUID, prefix, or current
// title).
func permissionsCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:   "permissions",
		Short: "Inspect and manage agent permission slugs",
		Long: "List, grant, deny, and revoke agent permission slugs without hand-editing ~/.tclaude/config.json. " +
			"`ls` and `slugs` are open to anyone; `grant`, `deny`, and `revoke` are gated on permissions.grant / permissions.revoke. " +
			"Use the magic target `default` to modify the global defaults list, or pass a conv selector (UUID/prefix/title) to set per-conv overrides. " +
			"`grant` adds a slug, `deny` blocks an otherwise-default slug for one agent, and `revoke` clears either back to the inherited default.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			permissionsLsCmd(),
			permissionsGrantCmd(),
			permissionsDenyCmd(),
			permissionsRevokeCmd(),
			permissionsSlugsCmd(),
		},
	}.ToCobra()
}

// --- shared types ---

type permissionsState struct {
	Defaults []string            `json:"defaults"`
	Grants   map[string][]string `json:"grants"`
	// Overrides is the full tri-state per-conv view — conv-id → slug →
	// "grant" | "deny". Grants is its grant-only projection.
	Overrides map[string]map[string]string `json:"overrides"`
}

type permSlugEntry struct {
	Slug        string `json:"slug"`
	Description string `json:"description"`
}

type permissionsMutateResp struct {
	Target    string   `json:"target"`
	TargetKey string   `json:"target_key,omitempty"`
	Title     string   `json:"title,omitempty"`
	Slug      string   `json:"slug"`
	Effective []string `json:"effective"`
}

// --- permissions ls ---

type permissionsLsParams struct {
	Target string `pos:"true" optional:"true" help:"Show effective permissions for one target. 'default' shows the defaults list; otherwise a conv selector (UUID, prefix, or title)."`
	JSON   bool   `long:"json" help:"Output JSON"`
}

func permissionsLsCmd() *cobra.Command {
	return boa.CmdT[permissionsLsParams]{
		Use:         "ls",
		Short:       "List defaults and overrides (or effective perms for one target)",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *permissionsLsParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Target).SetAlternativesFunc(completePermissionTargets)
			return nil
		},
		RunFunc: func(p *permissionsLsParams, _ *cobra.Command, _ []string) {
			os.Exit(runPermissionsLs(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runPermissionsLs(p *permissionsLsParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var state permissionsState
	if err := DaemonGet("/v1/permissions", &state); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if p.Target != "" {
		return renderEffectivePerms(p, state, stdout, stderr)
	}
	if p.JSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(state); err != nil {
			return rcIOFailure
		}
		return rcOK
	}
	return renderPermissionsState(state, stdout)
}

func renderPermissionsState(state permissionsState, stdout io.Writer) int {
	defs := append([]string{}, state.Defaults...)
	sort.Strings(defs)
	if len(defs) == 0 {
		fmt.Fprintln(stdout, "DEFAULTS: (none)")
	} else {
		fmt.Fprintln(stdout, "DEFAULTS:")
		for _, s := range defs {
			fmt.Fprintf(stdout, "  %s\n", s)
		}
	}
	if len(state.Overrides) == 0 {
		fmt.Fprintln(stdout, "PER-AGENT OVERRIDES: (none)")
		return rcOK
	}
	fmt.Fprintln(stdout, "PER-AGENT OVERRIDES:")
	tbl := table.New(
		table.Column{Header: "ID", Width: 8},
		table.Column{Header: "TITLE", MinWidth: 8, Weight: 0.7, Truncate: true},
		table.Column{Header: "GRANTED", MinWidth: 10, Weight: 1.2, Truncate: true},
		table.Column{Header: "DENIED", MinWidth: 8, Weight: 1.0, Truncate: true},
	)
	tbl.SetTerminalWidth(table.GetTerminalWidth())
	keys := make([]string, 0, len(state.Overrides))
	for k := range state.Overrides {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		var granted, denied []string
		for slug, effect := range state.Overrides[k] {
			if effect == "deny" {
				denied = append(denied, slug)
			} else {
				granted = append(granted, slug)
			}
		}
		sort.Strings(granted)
		sort.Strings(denied)
		// Try to surface a friendly title for keys that look like full
		// conv-ids. Prefixes and arbitrary strings are passed through.
		title := grantKeyTitle(k)
		idShort := k
		if len(k) > 8 {
			idShort = k[:8]
		}
		tbl.AddRow(table.Row{Cells: []string{
			idShort, title,
			strings.Join(granted, ", "),
			strings.Join(denied, ", "),
		}})
	}
	fmt.Fprintln(stdout, tbl.Render())
	return rcOK
}

// grantKeyTitle returns the display title for a conv-id grant key. The
// daemon stores grants under full conv-ids, but in practice the human
// also gets prefixes back as scaffolding — accept both. Returns "" when
// nothing's known so render falls through gracefully.
func grantKeyTitle(key string) string {
	if len(key) < 8 {
		return ""
	}
	if row, err := db.GetConvIndex(key); err == nil && row != nil {
		return DisplayTitle(row)
	}
	if row, err := db.FindConvIndexByPrefix(key); err == nil && row != nil {
		return DisplayTitle(row)
	}
	return ""
}

func renderEffectivePerms(p *permissionsLsParams, state permissionsState, stdout, stderr io.Writer) int {
	if p.Target == "default" {
		defs := append([]string{}, state.Defaults...)
		sort.Strings(defs)
		if p.JSON {
			enc := json.NewEncoder(stdout)
			enc.SetIndent("", "  ")
			if err := enc.Encode(map[string]any{"target": "default", "effective": defs}); err != nil {
				return rcIOFailure
			}
			return rcOK
		}
		fmt.Fprintln(stdout, "default — effective permissions:")
		if len(defs) == 0 {
			fmt.Fprintln(stdout, "  (none)")
			return rcOK
		}
		for _, s := range defs {
			fmt.Fprintf(stdout, "  %s\n", s)
		}
		return rcOK
	}
	res, matches, err := resolveSelector(p.Target)
	if err != nil {
		if matches != nil {
			printAmbiguous(stderr, p.Target, matches)
			return rcAmbiguous
		}
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcNotFound
	}
	title := ""
	if res.Row != nil {
		title = DisplayTitle(res.Row)
	}
	effective, source := effectivePermsFor(state, res.ConvID, title)
	sort.Strings(effective)
	if p.JSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(map[string]any{
			"target":     p.Target,
			"target_key": res.ConvID,
			"title":      title,
			"effective":  effective,
			"source":     source,
		}); err != nil {
			return rcIOFailure
		}
		return rcOK
	}
	fmt.Fprintf(stdout, "%s (%s) — effective permissions [%s]:\n", short(res.ConvID), title, source)
	if len(effective) == 0 {
		fmt.Fprintln(stdout, "  (none)")
		return rcOK
	}
	for _, s := range effective {
		fmt.Fprintf(stdout, "  %s\n", s)
	}
	return rcOK
}

// effectivePermsFor returns the slug list the daemon would consult for
// this agent. Per-conv overrides live in SQLite keyed by full conv-id:
// a grant override ADDS a slug on top of the global defaults, a deny
// override SUBTRACTS one. So the effective set is
// (defaults ∪ grants) − denies.
//
// The returned label names the matched sources ("defaults",
// "defaults+grants:<conv>", with " −denies" appended when any deny
// override applies).
func effectivePermsFor(state permissionsState, convID, _ string) ([]string, string) {
	effective := append([]string{}, state.Defaults...)
	source := "defaults"
	if grants, ok := state.Grants[convID]; ok && len(grants) > 0 {
		effective = mergeUnique(effective, grants)
		source = "defaults+grants:" + convID
	}
	denied := map[string]bool{}
	for slug, effect := range state.Overrides[convID] {
		if effect == "deny" {
			denied[slug] = true
		}
	}
	if len(denied) > 0 {
		kept := make([]string, 0, len(effective))
		for _, s := range effective {
			if !denied[s] {
				kept = append(kept, s)
			}
		}
		effective = kept
		source += " −denies"
	}
	return effective, source
}

func mergeUnique(a, b []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, v := range a {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	for _, v := range b {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// --- permissions grant ---

type permissionsGrantParams struct {
	Target   string `pos:"true" help:"'default' or a conv selector (UUID, prefix, or current title)"`
	Slug     string `pos:"true" help:"Permission slug to grant (see 'tclaude agent permissions slugs')"`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s' or '60'). Capped at 300s. Timeout = deny."`
}

func permissionsGrantCmd() *cobra.Command {
	return boa.CmdT[permissionsGrantParams]{
		Use:         "grant",
		Short:       "Grant a permission slug to defaults or a specific agent",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *permissionsGrantParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Target).SetAlternativesFunc(completePermissionTargets)
			boa.GetParamT(ctx, &p.Slug).SetAlternativesFunc(completePermissionSlugs)
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *permissionsGrantParams, _ *cobra.Command, _ []string) {
			os.Exit(runPermissionsMutate("/v1/permissions/grant", "Granted", p.Target, p.Slug, p.AskHuman, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

// --- permissions deny ---

type permissionsDenyParams struct {
	Target   string `pos:"true" help:"A conv selector (UUID, prefix, or current title). Unlike grant/revoke, 'default' is not accepted — deny is a per-conv override."`
	Slug     string `pos:"true" help:"Permission slug to deny (see 'tclaude agent permissions slugs')"`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s' or '60'). Capped at 300s. Timeout = deny."`
}

func permissionsDenyCmd() *cobra.Command {
	return boa.CmdT[permissionsDenyParams]{
		Use:   "deny",
		Short: "Deny a permission slug for a specific agent (blocks an otherwise-default slug)",
		Long: "Write a per-conv DENY override: the agent will not hold the slug even if it is in the global defaults list. " +
			"Use 'revoke' to clear the deny back to the inherited default. The 'default' target is not valid here — " +
			"to remove a slug for every agent, revoke it from the defaults list instead.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *permissionsDenyParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Target).SetAlternativesFunc(completePermissionTargets)
			boa.GetParamT(ctx, &p.Slug).SetAlternativesFunc(completePermissionSlugs)
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *permissionsDenyParams, _ *cobra.Command, _ []string) {
			os.Exit(runPermissionsMutate("/v1/permissions/deny", "Denied", p.Target, p.Slug, p.AskHuman, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

// --- permissions revoke ---

type permissionsRevokeParams struct {
	Target   string `pos:"true" help:"'default' or a conv selector (UUID, prefix, or current title)"`
	Slug     string `pos:"true" help:"Permission slug to revoke (clears a grant or deny back to the inherited default)"`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s' or '60'). Capped at 300s. Timeout = deny."`
}

func permissionsRevokeCmd() *cobra.Command {
	return boa.CmdT[permissionsRevokeParams]{
		Use:         "revoke",
		Short:       "Revoke a permission slug from defaults or a specific agent",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *permissionsRevokeParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Target).SetAlternativesFunc(completePermissionTargets)
			boa.GetParamT(ctx, &p.Slug).SetAlternativesFunc(completePermissionSlugs)
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *permissionsRevokeParams, _ *cobra.Command, _ []string) {
			os.Exit(runPermissionsMutate("/v1/permissions/revoke", "Revoked", p.Target, p.Slug, p.AskHuman, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

// runPermissionsMutate is shared between grant and revoke; the only
// difference is the path and the verb used in success output.
func runPermissionsMutate(path, verb, target, slug, askHumanRaw string, stdout, stderr io.Writer) int {
	if target == "" {
		fmt.Fprintln(stderr, "Error: target is required ('default' or a conv selector)")
		return rcInvalidArg
	}
	if slug == "" {
		fmt.Fprintln(stderr, "Error: slug is required")
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	ask, err := ParseAskHuman(askHumanRaw)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcInvalidArg
	}
	if ask > 0 {
		fmt.Fprintf(stdout, "Waiting up to %s for human approval...\n", ask)
	}
	var resp permissionsMutateResp
	if err := DaemonRequest(http.MethodPost, path, map[string]string{
		"target": target,
		"slug":   slug,
	}, &resp, DaemonOpts{AskHuman: ask}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	label := resp.Target
	if resp.TargetKey != "" && resp.TargetKey != resp.Target {
		short := resp.TargetKey
		if len(short) > 8 {
			short = short[:8]
		}
		if resp.Title != "" {
			label = fmt.Sprintf("%s (%s, %s)", resp.Target, short, resp.Title)
		} else {
			label = fmt.Sprintf("%s (%s)", resp.Target, short)
		}
	}
	sort.Strings(resp.Effective)
	if len(resp.Effective) == 0 {
		fmt.Fprintf(stdout, "%s %q on %s. Effective: (none)\n", verb, slug, label)
	} else {
		fmt.Fprintf(stdout, "%s %q on %s. Effective: %s\n",
			verb, slug, label, strings.Join(resp.Effective, ", "))
	}
	return rcOK
}

// --- permissions slugs ---

type permissionsSlugsParams struct {
	JSON bool `long:"json" help:"Output JSON"`
}

func permissionsSlugsCmd() *cobra.Command {
	return boa.CmdT[permissionsSlugsParams]{
		Use:         "slugs",
		Short:       "List known permission slugs and their descriptions",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *permissionsSlugsParams, _ *cobra.Command, _ []string) {
			os.Exit(runPermissionsSlugs(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runPermissionsSlugs(p *permissionsSlugsParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var slugs []permSlugEntry
	if err := DaemonGet("/v1/permissions/slugs", &slugs); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if p.JSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(slugs); err != nil {
			return rcIOFailure
		}
		return rcOK
	}
	if len(slugs) == 0 {
		fmt.Fprintln(stdout, "(no slugs registered)")
		return rcOK
	}
	tbl := table.New(
		table.Column{Header: "SLUG", MinWidth: 12, Weight: 0.5, Truncate: true},
		table.Column{Header: "DESCRIPTION", MinWidth: 20, Weight: 1.5, Truncate: true},
	)
	tbl.SetTerminalWidth(table.GetTerminalWidth())
	for _, s := range slugs {
		tbl.AddRow(table.Row{Cells: []string{s.Slug, s.Description}})
	}
	fmt.Fprintln(stdout, tbl.Render())
	return rcOK
}
