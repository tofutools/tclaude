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
	// AgentIDs projects the stable agent_id behind each conv key (conv-id
	// → agent_id), so the roster can lead with the rotation-immune id while
	// the maps above stay conv-keyed (JOH-325). Absent for a conv with no
	// actor behind it; the renderer falls back to the conv prefix then.
	AgentIDs map[string]string `json:"agent_ids"`
}

type permSlugEntry struct {
	Slug        string `json:"slug"`
	Description string `json:"description"`
	// OwnerImplied mirrors agentd.PermSlug.OwnerImplied: group ownership
	// confers this slug structurally (the owner-bypass), so an owner holds
	// it for owned groups / their members without an explicit grant.
	OwnerImplied bool `json:"owner_implied,omitempty"`
}

type permissionsMutateResp struct {
	Target    string   `json:"target"`
	TargetKey string   `json:"target_key,omitempty"`
	AgentID   string   `json:"agent_id,omitempty"`
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
		table.Column{Header: "ID", Width: 12},
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
		// Lead with the stable agent_id (rotation-immune); fall back to the
		// conv prefix when the daemon couldn't project one. conv-id stays
		// available via --json (the Overrides map is conv-keyed).
		idShort := shortAgentID(state.AgentIDs[k], k)
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
	ownerImplied, isOwner := ownerImpliedSlugsFor(res.ConvID)
	effective, ownerAdded, source := effectivePermsFor(state, res.ConvID, ownerImplied, isOwner)
	sort.Strings(effective)
	if p.JSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		sort.Strings(ownerAdded)
		if err := enc.Encode(map[string]any{
			"target":        p.Target,
			"target_key":    res.ConvID,
			"agent_id":      res.AgentID,
			"title":         title,
			"effective":     effective,
			"source":        source,
			"owner_implied": ownerAdded,
		}); err != nil {
			return rcIOFailure
		}
		return rcOK
	}
	fmt.Fprintf(stdout, "%s (%s) — effective permissions [%s]:\n", shortAgentID(res.AgentID, res.ConvID), title, source)
	if len(effective) == 0 {
		fmt.Fprintln(stdout, "  (none)")
		return rcOK
	}
	ownerSet := map[string]bool{}
	for _, s := range ownerAdded {
		ownerSet[s] = true
	}
	for _, s := range effective {
		if ownerSet[s] {
			fmt.Fprintf(stdout, "  %s  (via ownership)\n", s)
		} else {
			fmt.Fprintf(stdout, "  %s\n", s)
		}
	}
	return rcOK
}

// ownerImpliedSlugsFor reports the owner-conferred slug set and whether
// convID owns at least one group. Group ownership is read straight from
// the shared SQLite (db.ListGroupsOwnedBy); the owner-conferred slug set
// comes from the daemon's registry (the daemon is the source of truth, and
// the agent package can't import agentd). On any error it degrades to
// (nil, false) — owner perms simply go un-annotated rather than failing
// the listing.
func ownerImpliedSlugsFor(convID string) ([]string, bool) {
	owned, err := db.ListGroupsOwnedBy(convID)
	if err != nil || len(owned) == 0 {
		return nil, false
	}
	var slugs []permSlugEntry
	if err := DaemonGet("/v1/permissions/slugs", &slugs); err != nil {
		return nil, true
	}
	var out []string
	for _, s := range slugs {
		if s.OwnerImplied {
			out = append(out, s.Slug)
		}
	}
	return out, true
}

// effectivePermsFor returns the slug list the daemon would consult for
// this agent. Per-conv overrides live in SQLite keyed by full conv-id:
// a grant override ADDS a slug on top of the global defaults, a deny
// override SUBTRACTS one. Group ownership ADDS the owner-conferred slugs
// (ownerImplied, folded in only when isOwner) — the structural owner-
// bypass, which a deny still suppresses. So the effective set is
// ((defaults ∪ grants ∪ owner-implied) − denies).
//
// ownerAdded reports the subset contributed SOLELY by ownership (not
// already held via defaults/grants and not denied), so the caller can
// annotate those rows "(via ownership)".
//
// The returned label names the matched sources ("defaults",
// "defaults+grants:<conv>", "+owner", with " −denies" appended when any
// deny override applies).
func effectivePermsFor(state permissionsState, convID string, ownerImplied []string, isOwner bool) (effective, ownerAdded []string, source string) {
	effective = append([]string{}, state.Defaults...)
	source = "defaults"
	if grants, ok := state.Grants[convID]; ok && len(grants) > 0 {
		effective = mergeUnique(effective, grants)
		source = "defaults+grants:" + convID
	}
	if isOwner && len(ownerImplied) > 0 {
		held := map[string]bool{}
		for _, s := range effective {
			held[s] = true
		}
		for _, s := range ownerImplied {
			if !held[s] {
				ownerAdded = append(ownerAdded, s)
			}
		}
		if len(ownerAdded) > 0 {
			effective = mergeUnique(effective, ownerImplied)
			source += "+owner"
		}
	}
	denied := map[string]bool{}
	for slug, effect := range state.Overrides[convID] {
		if effect == "deny" {
			denied[slug] = true
		}
	}
	if len(denied) > 0 {
		effective = dropDenied(effective, denied)
		ownerAdded = dropDenied(ownerAdded, denied)
		source += " −denies"
	}
	return effective, ownerAdded, source
}

// dropDenied returns slugs with every denied entry removed, preserving
// order. Shared by the effective set and its owner-conferred projection so
// a deny override suppresses a slug in both — deny is authoritative over
// the owner-bypass, mirroring the daemon's precedence.
func dropDenied(slugs []string, denied map[string]bool) []string {
	kept := make([]string, 0, len(slugs))
	for _, s := range slugs {
		if !denied[s] {
			kept = append(kept, s)
		}
	}
	return kept
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
		// Lead the resolved identity with the stable agent_id (conv-id is
		// the live snapshot behind it); fall back to the conv prefix when
		// the daemon couldn't project an agent_id.
		who := shortAgentID(resp.AgentID, resp.TargetKey)
		if resp.Title != "" {
			label = fmt.Sprintf("%s (%s, %s)", resp.Target, who, resp.Title)
		} else {
			label = fmt.Sprintf("%s (%s)", resp.Target, who)
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
		table.Column{Header: "OWNER", Width: 5},
		table.Column{Header: "DESCRIPTION", MinWidth: 20, Weight: 1.5, Truncate: true},
	)
	tbl.SetTerminalWidth(table.GetTerminalWidth())
	for _, s := range slugs {
		owner := ""
		if s.OwnerImplied {
			owner = "✔"
		}
		tbl.AddRow(table.Row{Cells: []string{s.Slug, owner, s.Description}})
	}
	fmt.Fprintln(stdout, tbl.Render())
	fmt.Fprintln(stdout, "\nOWNER ✔ = group ownership confers this slug for owned groups / their members, without an explicit grant (a per-agent deny still suppresses it).")
	return rcOK
}
