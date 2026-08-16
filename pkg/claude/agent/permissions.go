package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
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
	Overrides map[string]map[string]string          `json:"overrides"`
	Scopes    map[string]map[string]permissionScope `json:"scopes"`
	// GroupGrants is group name → slugs that group grants to every member
	// — a standing source the daemon's resolver consults directly, so it
	// belongs in the roster next to the defaults. Empty against a daemon
	// that predates the field.
	GroupGrants map[string][]string                   `json:"group_grants"`
	GroupScopes map[string]map[string]permissionScope `json:"group_scopes"`
	// AgentIDs projects the stable agent_id behind each conv key (conv-id
	// → agent_id), so the roster can lead with the rotation-immune id while
	// the maps above stay conv-keyed (JOH-325). Absent for a conv with no
	// actor behind it; the renderer falls back to the conv prefix then.
	AgentIDs map[string]string `json:"agent_ids"`
	// Titles projects the display name behind each conv key. The daemon
	// supplies it so this CLI never reads ~/.tclaude/data to decorate the
	// roster — a sandboxed agent is denied that directory (TCL-611).
	Titles map[string]string `json:"titles"`
}

// permissionsEffectiveResp mirrors the daemon's resolved answer to
// GET /v1/permissions?target=<selector>. Selector resolution and the
// effective/owner-implied calculation both happen daemon-side.
type permissionsEffectiveResp struct {
	// Resolved is the contract discriminator the daemon always sets on a
	// targeted answer. It is what distinguishes a real effective view from
	// a pre-TCL-611 daemon's reply — that build ignores `?target` and
	// returns the ordinary roster with HTTP 200, which decodes here as
	// all-zero and would otherwise render as "this agent holds nothing".
	Resolved     bool     `json:"resolved"`
	Target       string   `json:"target"`
	TargetKey    string   `json:"target_key"`
	AgentID      string   `json:"agent_id"`
	Title        string   `json:"title"`
	Effective    []string `json:"effective"`
	Source       string   `json:"source"`
	OwnerImplied []string `json:"owner_implied"`
	// Provenance maps each effective slug to the daemon resolver's source
	// for it — "sudo", "override", "group", "default", or "owner:<scope>"
	// for the structural owner bypass. Absent from an older daemon's
	// reply, in which case rows render unannotated apart from the
	// OwnerImplied projection.
	Provenance map[string]string `json:"provenance,omitempty"`
	// OwnedGroups names the active groups this target owns — what an
	// owner-conferred slug is scoped to. Empty for a non-owner.
	OwnedGroups []string `json:"owned_groups,omitempty"`
}

// ambiguousCandidate is one entry of the daemon's typed `ambiguous`
// envelope — enough to disambiguate without a local conv lookup.
type ambiguousCandidate struct {
	AgentID string `json:"agent_id"`
	ConvID  string `json:"conv_id"`
	Title   string `json:"title"`
}

type permSlugEntry struct {
	Slug        string `json:"slug"`
	Description string `json:"description"`
	// OwnerImplied mirrors agentd.PermSlug.OwnerImplied: group ownership
	// contributes this ordinary permission slug.
	OwnerImplied bool     `json:"owner_implied,omitempty"`
	ScopeDims    []string `json:"scope_dims,omitempty"`
}

type permissionScope map[string][]string

type permissionsMutateResp struct {
	Target    string          `json:"target"`
	TargetKey string          `json:"target_key,omitempty"`
	AgentID   string          `json:"agent_id,omitempty"`
	Title     string          `json:"title,omitempty"`
	Slug      string          `json:"slug"`
	Scope     permissionScope `json:"scope,omitempty"`
	Effective []string        `json:"effective"`
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

// RunPermissionsLs is the exported entry point for `tclaude agent
// permissions ls`, so flow tests can drive the real CLI rendering against
// the daemon mux. target may be "" (roster view), "default", or a conv
// selector.
func RunPermissionsLs(target string, jsonOut bool, stdout, stderr io.Writer) int {
	return runPermissionsLs(&permissionsLsParams{Target: target, JSON: jsonOut}, stdout, stderr)
}

func runPermissionsLs(p *permissionsLsParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	if p.Target != "" {
		return renderEffectivePerms(p, stdout, stderr)
	}
	var state permissionsState
	if err := DaemonGet("/v1/permissions", &state); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
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
	renderGroupGrants(state, stdout)
	if len(state.Overrides) == 0 {
		fmt.Fprintln(stdout, "PER-AGENT OVERRIDES: (none)")
		return rcOK
	}
	fmt.Fprintln(stdout, "PER-AGENT OVERRIDES:")
	tbl := table.New(
		table.Column{Header: "ID", Width: 12},
		table.Column{Header: "TITLE", MinWidth: 8, Weight: 0.7, Truncate: true},
		table.Column{Header: "GRANTED", MinWidth: 10, Weight: 1.2},
		table.Column{Header: "DENIED", MinWidth: 8, Weight: 1.0},
	)
	maxGranted, maxDenied := 10, 8
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
				granted = append(granted, scopedPermissionLabel(slug, state.Scopes[k][slug]))
			}
		}
		sort.Strings(granted)
		sort.Strings(denied)
		// Titles arrive already projected by the daemon; a key it could
		// not resolve simply renders blank.
		title := state.Titles[k]
		// Lead with the stable agent_id (rotation-immune); fall back to the
		// conv prefix when the daemon couldn't project one. conv-id stays
		// available via --json (the Overrides map is conv-keyed).
		idShort := shortAgentID(state.AgentIDs[k], k)
		tbl.AddRow(table.Row{Cells: []string{
			idShort, title,
			strings.Join(granted, ", "),
			strings.Join(denied, ", "),
		}})
		maxGranted = max(maxGranted, table.StringWidth(strings.Join(granted, ", ")))
		maxDenied = max(maxDenied, table.StringWidth(strings.Join(denied, ", ")))
	}
	// Permission names and their scopes are security-relevant output. Widen
	// beyond a narrow terminal when necessary rather than hiding the decisive
	// suffix behind an ellipsis. ID (12), minimum title (8), and three two-space
	// paddings account for the non-permission columns.
	tbl.SetTerminalWidth(max(table.GetTerminalWidth(), 12+8+6+maxGranted+maxDenied))
	fmt.Fprintln(stdout, tbl.Render())
	return rcOK
}

// renderGroupGrants prints the group-granted slugs — the standing source
// that every member of a group inherits. It is listed alongside the
// defaults because the daemon's resolver consults it directly: a slug
// here is in force for the group's members even with nothing in DEFAULTS
// or PER-AGENT OVERRIDES. Silent when no group grants anything, so the
// common case stays terse.
func renderGroupGrants(state permissionsState, stdout io.Writer) {
	if len(state.GroupGrants) == 0 {
		return
	}
	names := make([]string, 0, len(state.GroupGrants))
	for name := range state.GroupGrants {
		names = append(names, name)
	}
	sort.Strings(names)
	fmt.Fprintln(stdout, "GROUP GRANTS (held by every member):")
	for _, name := range names {
		slugs := make([]string, 0, len(state.GroupGrants[name]))
		for _, slug := range state.GroupGrants[name] {
			slugs = append(slugs, scopedPermissionLabel(slug, state.GroupScopes[name][slug]))
		}
		sort.Strings(slugs)
		fmt.Fprintf(stdout, "  %s: %s\n", name, strings.Join(slugs, ", "))
	}
}

func scopedPermissionLabel(slug string, scope permissionScope) string {
	if rendered := renderPermissionScope(scope); rendered != "" {
		return slug + " [" + rendered + "]"
	}
	return slug
}

func renderPermissionScope(scope permissionScope) string {
	if len(scope) == 0 {
		return ""
	}
	dims := make([]string, 0, len(scope))
	for dim := range scope {
		dims = append(dims, dim)
	}
	sort.Strings(dims)
	parts := make([]string, 0, len(dims))
	for _, dim := range dims {
		parts = append(parts, dim+"="+strings.Join(scope[dim], ","))
	}
	return strings.Join(parts, " ")
}

// renderEffectivePerms asks the daemon for the resolved effective view of
// one target and prints it. Selector resolution, the effective/owner-
// implied calculation and the display metadata all come back over the
// wire: this path must never read ~/.tclaude/data, which a sandboxed
// agent is (correctly) denied (TCL-611).
func renderEffectivePerms(p *permissionsLsParams, stdout, stderr io.Writer) int {
	var resp permissionsEffectiveResp
	err := DaemonGet("/v1/permissions?target="+url.QueryEscape(p.Target), &resp)
	if de, ok := err.(*DaemonError); ok && de.Code == "ambiguous" {
		printDaemonAmbiguous(stderr, p.Target, de)
		return rcAmbiguous
	}
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	// Refuse anything that isn't a genuine targeted answer BEFORE rendering
	// — including in --json mode, where a zero-valued struct would emit a
	// false "no permissions" result a script could act on.
	if !resp.Resolved || resp.Target == "" || resp.Source == "" {
		fmt.Fprintf(stderr,
			"Error: agentd answered without a resolved permission view for %q. "+
				"The running daemon predates this CLI build; restart tclaude agentd so it picks up the current binary.\n",
			p.Target)
		return rcIOFailure
	}
	if p.JSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(resp); err != nil {
			return rcIOFailure
		}
		return rcOK
	}
	if resp.TargetKey == "" {
		// The "default" sentinel: a slug list, not an agent.
		fmt.Fprintf(stdout, "%s — effective permissions:\n", resp.Target)
	} else {
		fmt.Fprintf(stdout, "%s (%s) — effective permissions [%s]:\n",
			shortAgentID(resp.AgentID, resp.TargetKey), resp.Title, resp.Source)
	}
	if len(resp.Effective) == 0 {
		fmt.Fprintln(stdout, "  (none)")
		return rcOK
	}
	ownerSet := map[string]bool{}
	for _, s := range resp.OwnerImplied {
		ownerSet[s] = true
	}
	for _, s := range resp.Effective {
		if note := permSourceNote(resp.Provenance[s], ownerSet[s], resp.OwnedGroups); note != "" {
			fmt.Fprintf(stdout, "  %s  %s\n", s, note)
		} else {
			fmt.Fprintf(stdout, "  %s\n", s)
		}
	}
	return rcOK
}

// permSourceNote renders the daemon's per-slug provenance as the short
// parenthetical the listing appends. A slug held by the plain config
// default gets no note — that is the unremarkable case, and annotating
// every row would bury the ones that matter.
//
// Owner rows carry their SCOPE, because "(via ownership)" alone reads as
// fleet-wide authority the gate does not grant: a group- or member-scoped
// slug reaches only the groups this agent owns, and is refused everywhere
// else. ownerImplied keeps the historical unscoped wording working
// against a daemon that predates the provenance map.
func permSourceNote(source string, ownerImplied bool, ownedGroups []string) string {
	base, scope := splitPermissionProvenance(source)
	var note string
	switch base {
	case "sudo":
		note = "via sudo elevation"
	case "override":
		note = "via agent grant"
	case "group":
		note = "via group"
	case "default":
		if scope == "" {
			return ""
		}
		note = "via default"
	}
	if note != "" {
		if scope != "" {
			note += "; scope: " + scope
		}
		return "(" + note + ")"
	}
	if ownerScope, ok := strings.CutPrefix(base, "owner:"); ok {
		return ownerScopeNote(ownerScope, ownedGroups)
	}
	if base == "owner" || ownerImplied {
		if scope != "" {
			return "(via ownership; scope: " + scope + ")"
		}
		return "(via ownership)"
	}
	return ""
}

func splitPermissionProvenance(source string) (base, scope string) {
	i := strings.Index(source, " [")
	if i < 0 {
		return source, ""
	}
	return source[:i], strings.TrimSpace(source[i+1:])
}

// ownerScopeNote phrases an owner-conferred slug's reach. With no owned
// groups to name (an older daemon, or a lookup that failed) it falls back
// to the unscoped wording rather than claiming a scope it cannot show.
func ownerScopeNote(scope string, ownedGroups []string) string {
	if len(ownedGroups) == 0 {
		return "(via ownership)"
	}
	where := strings.Join(ownedGroups, ", ")
	switch scope {
	case "group":
		return fmt.Sprintf("(via ownership of: %s)", where)
	case "member":
		return fmt.Sprintf("(via ownership of: %s — their members only)", where)
	}
	// "any", and any scope a NEWER daemon invented that this build does
	// not know: fall back to the unscoped wording. Guessing the narrower
	// group phrasing would misreport an unknown scope, which is the
	// failure this whole change exists to prevent.
	return "(via ownership)"
}

// printDaemonAmbiguous renders the daemon's typed ambiguity envelope.
// Candidates come from the response, so this stays readable for a caller
// that cannot resolve conv-ids locally. Falls back to the bare daemon
// message if the envelope carries no candidates.
func printDaemonAmbiguous(out io.Writer, selector string, de *DaemonError) {
	var body struct {
		Candidates []ambiguousCandidate `json:"candidates"`
	}
	_ = json.Unmarshal(de.Raw, &body)
	if len(body.Candidates) == 0 {
		fmt.Fprintf(out, "Error: %s\n", de.Error())
		return
	}
	fmt.Fprintf(out, "Error: selector %q matches %d conversations:\n", selector, len(body.Candidates))
	for _, c := range body.Candidates {
		fmt.Fprintf(out, "  %s  %s\n", shortAgentID(c.AgentID, c.ConvID), c.Title)
	}
	fmt.Fprintf(out, "Disambiguate by ID prefix.\n")
}

// --- permissions grant ---

type permissionsGrantParams struct {
	Target   string   `pos:"true" help:"'default' or a conv selector (UUID, prefix, or current title)"`
	Slug     string   `pos:"true" help:"Permission slug to grant (see 'tclaude agent permissions slugs')"`
	Scope    []string `long:"scope" optional:"true" help:"Limit the grant with dim=v1,v2. Repeat for multiple dimensions."`
	AskHuman string   `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s' or '60'). Capped at 300s. Timeout = deny."`
}

func parsePermissionScopeFlags(flags []string) (permissionScope, error) {
	if len(flags) == 0 {
		return nil, nil
	}
	scope := permissionScope{}
	seen := map[string]map[string]bool{}
	for _, flag := range flags {
		dim, values, ok := strings.Cut(flag, "=")
		dim = strings.TrimSpace(dim)
		if !ok || dim == "" || strings.TrimSpace(values) == "" {
			return nil, fmt.Errorf("invalid --scope %q (want dim=v1,v2)", flag)
		}
		if seen[dim] == nil {
			seen[dim] = map[string]bool{}
		}
		for _, raw := range strings.Split(values, ",") {
			value := strings.TrimSpace(raw)
			if value == "" {
				return nil, fmt.Errorf("invalid --scope %q: matcher must not be empty", flag)
			}
			if !seen[dim][value] {
				scope[dim] = append(scope[dim], value)
				seen[dim][value] = true
			}
		}
		sort.Strings(scope[dim])
	}
	return scope, nil
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
			os.Exit(runPermissionsMutate("/v1/permissions/grant", "Granted", p.Target, p.Slug, p.Scope, p.AskHuman, os.Stdout, os.Stderr))
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
			os.Exit(runPermissionsMutate("/v1/permissions/deny", "Denied", p.Target, p.Slug, nil, p.AskHuman, os.Stdout, os.Stderr))
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
			os.Exit(runPermissionsMutate("/v1/permissions/revoke", "Revoked", p.Target, p.Slug, nil, p.AskHuman, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

// runPermissionsMutate is shared between grant and revoke; the only
// difference is the path and the verb used in success output.
func runPermissionsMutate(path, verb, target, slug string, scopeFlags []string, askHumanRaw string, stdout, stderr io.Writer) int {
	if target == "" {
		fmt.Fprintln(stderr, "Error: target is required ('default' or a conv selector)")
		return rcInvalidArg
	}
	if slug == "" {
		fmt.Fprintln(stderr, "Error: slug is required")
		return rcInvalidArg
	}
	scope, err := parsePermissionScopeFlags(scopeFlags)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
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
	body := map[string]any{
		"target": target,
		"slug":   slug,
	}
	if len(scope) != 0 {
		body["scope"] = scope
	}
	if err := DaemonRequest(http.MethodPost, path, body, &resp, DaemonOpts{AskHuman: ask}); err != nil {
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

// ownerScopeCell renders the ordinary grant shape ownership contributes.
func ownerScopeCell(s permSlugEntry) string {
	if !s.OwnerImplied {
		return ""
	}
	if slices.Contains(s.ScopeDims, "group") {
		return "✔ owned groups"
	}
	return "✔ global"
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
		table.Column{Header: "OWNER GRANT", Width: 16},
		table.Column{Header: "SCOPES", MinWidth: 8, Weight: 0.4, Truncate: true},
		table.Column{Header: "DESCRIPTION", MinWidth: 20, Weight: 1.5, Truncate: true},
	)
	tbl.SetTerminalWidth(table.GetTerminalWidth())
	for _, s := range slugs {
		tbl.AddRow(table.Row{Cells: []string{s.Slug, ownerScopeCell(s), strings.Join(s.ScopeDims, ", "), s.Description}})
	}
	fmt.Fprintln(stdout, tbl.Render())
	fmt.Fprintln(stdout, "\nOWNER GRANT = group ownership contributes this permission automatically.")
	fmt.Fprintln(stdout, "  ✔ owned groups — one grant scoped to each group you own")
	fmt.Fprintln(stdout, "  ✔ global       — unscoped when you own any active group")
	fmt.Fprintln(stdout, "A per-agent deny for the same slug suppresses the owner grant.")
	return rcOK
}
