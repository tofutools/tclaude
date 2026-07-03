package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

// `tclaude agent templates` — group templates.
//
// A template is a reusable blueprint for a working group: a name, a
// shared default context, and an ordered list of agent specs.
// Instantiating one creates a fresh group and spawns its whole agent
// team. This is the CLI twin of the dashboard's Templates tab — a thin
// client over the daemon's /v1/templates endpoints.
//
// Verbs: ls, show, create, edit, rm, instantiate, from-group.
// Permissions are enforced daemon-side: templates.manage for create /
// edit / rm / from-group, templates.instantiate for instantiate; both
// are effectively human-only by default. Reads (ls / show) are open.

func templatesCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:         "templates",
		Short:       "Manage group templates (reusable team blueprints)",
		Long: "List, inspect, create, edit and delete group templates, instantiate a working group from one, " +
			"or snapshot an existing group into a new template. A template is a blueprint — a name, a shared " +
			"context, and an ordered list of agent specs (role, descr, task brief, owner flag, permission slugs); " +
			"instantiating it creates a fresh group and spawns its whole agent team. The CLI twin of the " +
			"dashboard's Templates tab.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			templatesLsCmd(),
			templatesShowCmd(),
			templatesCreateCmd(),
			templatesEditCmd(),
			templatesRmCmd(),
			templatesInstantiateCmd(),
			templatesFromGroupCmd(),
			templatesExportCmd(),
			templatesImportCmd(),
		},
	}.ToCobra()
}

// templateAgentJSON / templateJSON mirror the daemon's wire shape (see
// agentd/templates.go). create / edit accept this JSON via --file;
// show --json emits it.
type templateAgentJSON struct {
	Name           string   `json:"name"`
	Role           string   `json:"role,omitempty"`
	Descr          string   `json:"descr,omitempty"`
	InitialMessage string   `json:"initial_message,omitempty"`
	IsOwner        bool     `json:"is_owner,omitempty"`
	Permissions    []string `json:"permissions"`

	// RoleRef references a role in the role library (JOH-240): the agent
	// inherits that role's defaults beneath its own overrides. Empty = none.
	RoleRef string `json:"role_ref,omitempty"`

	// Per-role launch profile (JOH-239): a spawn-profile reference by name plus
	// inline launch overrides (harness/model/effort/sandbox/approval) that win
	// over it. All optional — absent = inherit the group default at instantiate.
	SpawnProfile string `json:"spawn_profile,omitempty"`
	Harness      string `json:"harness,omitempty"`
	Model        string `json:"model,omitempty"`
	Effort       string `json:"effort,omitempty"`
	Sandbox      string `json:"sandbox,omitempty"`
	Approval     string `json:"approval,omitempty"`
}

// workPatternEntryJSON mirrors the daemon's wire shape for one
// work-pattern step: an ordered, routed briefing message delivered
// after the whole roster has spawned. send_to is a template-agent name
// or "all"; value may carry {{task}}.
type workPatternEntryJSON struct {
	SendTo string `json:"send_to"`
	Value  string `json:"value"`
}

type templateJSON struct {
	Name           string                 `json:"name"`
	Descr          string                 `json:"descr,omitempty"`
	DefaultContext string                 `json:"default_context,omitempty"`
	Agents         []templateAgentJSON    `json:"agents"`
	WorkPattern    []workPatternEntryJSON `json:"work_pattern,omitempty"`
	CreatedAt      string                 `json:"created_at,omitempty"`
	UpdatedAt      string                 `json:"updated_at,omitempty"`
}

// templateExportEnvelope mirrors the daemon's portable export shape
// (see agentd/templates.go): a small versioned wrapper around the inner
// template JSON. `export` writes it; `import` sends it back.
type templateExportEnvelope struct {
	Format        string       `json:"format"`
	FormatVersion int          `json:"format_version"`
	ExportedAt    string       `json:"exported_at,omitempty"`
	Template      templateJSON `json:"template"`
	// Roles embeds the referenced role definitions (JOH-240) so the export
	// round-trips through the CLI; import re-creates any missing on the target.
	Roles []roleJSON `json:"roles,omitempty"`
}

// ownerNames returns the names of the template's owner agents.
func (t templateJSON) ownerNames() []string {
	out := []string{}
	for _, a := range t.Agents {
		if a.IsOwner {
			out = append(out, a.Name)
		}
	}
	return out
}

// ---- ls ----

func templatesLsCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:         "ls",
		Short:       "List group templates",
		Long:        "Returns every group template, with its agent count and owner(s).",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(_ *struct{}, _ *cobra.Command, _ []string) {
			os.Exit(runTemplatesLs(os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runTemplatesLs(stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var templates []templateJSON
	if err := DaemonRequest(http.MethodGet, "/v1/templates", nil, &templates, DaemonOpts{}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if len(templates) == 0 {
		fmt.Fprintln(stdout, "(no group templates)")
		return rcOK
	}
	fmt.Fprintf(stdout, "%-24s  %-7s  %-16s  %s\n", "NAME", "AGENTS", "OWNER(S)", "DESCR")
	fmt.Fprintln(stdout, strings.Repeat("─", 80))
	for _, t := range templates {
		owners := strings.Join(t.ownerNames(), ",")
		if owners == "" {
			owners = "—"
		}
		fmt.Fprintf(stdout, "%-24s  %-7d  %-16s  %s\n",
			t.Name, len(t.Agents), truncate(owners, 16), truncate(t.Descr, 32))
	}
	return rcOK
}

// ---- show ----

type templatesShowParams struct {
	Name string `pos:"true" help:"Template name (from 'tclaude agent templates ls')."`
	JSON bool   `long:"json" optional:"true" help:"Emit the raw template JSON instead of the human-readable view. The JSON round-trips: edit it and feed it back via 'templates edit <name> --file -'."`
}

func templatesShowCmd() *cobra.Command {
	return boa.CmdT[templatesShowParams]{
		Use:         "show <name>",
		Short:       "Show one group template in detail",
		Long:        "Prints a template's context and per-agent specs. With --json, emits the raw wire JSON — the same shape 'create' / 'edit' accept via --file, so `templates show X --json` is the start of an edit loop.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *templatesShowParams, _ *cobra.Command, _ []string) {
			os.Exit(runTemplatesShow(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runTemplatesShow(p *templatesShowParams, stdout, stderr io.Writer) int {
	name := strings.TrimSpace(p.Name)
	if name == "" {
		fmt.Fprintln(stderr, "Error: a template name is required")
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var t templateJSON
	if err := DaemonRequest(http.MethodGet, "/v1/templates/"+url.PathEscape(name), nil, &t, DaemonOpts{}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if p.JSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(t); err != nil {
			fmt.Fprintf(stderr, "Error: encoding template JSON: %v\n", err)
			return rcIOFailure
		}
		return rcOK
	}
	fmt.Fprintf(stdout, "Template: %s\n", t.Name)
	if t.Descr != "" {
		fmt.Fprintf(stdout, "  descr:   %s\n", t.Descr)
	}
	if ctx := strings.TrimSpace(t.DefaultContext); ctx != "" {
		fmt.Fprintln(stdout, "  context:")
		for _, line := range strings.Split(t.DefaultContext, "\n") {
			fmt.Fprintf(stdout, "    %s\n", line)
		}
	}
	fmt.Fprintf(stdout, "  agents (%d):\n", len(t.Agents))
	for i, a := range t.Agents {
		tags := []string{}
		if a.IsOwner {
			tags = append(tags, "owner")
		}
		if a.Role != "" {
			tags = append(tags, "role="+a.Role)
		}
		if a.RoleRef != "" {
			tags = append(tags, "role_ref="+a.RoleRef)
		}
		if len(a.Permissions) > 0 {
			tags = append(tags, "perms="+strings.Join(a.Permissions, ","))
		}
		// Per-role launch profile (JOH-239): show the profile reference and any
		// inline overrides so an edit loop sees what each role launches with.
		if a.SpawnProfile != "" {
			tags = append(tags, "profile="+a.SpawnProfile)
		}
		if a.Harness != "" {
			tags = append(tags, "harness="+a.Harness)
		}
		if a.Model != "" {
			tags = append(tags, "model="+a.Model)
		}
		if a.Effort != "" {
			tags = append(tags, "effort="+a.Effort)
		}
		if a.Sandbox != "" {
			tags = append(tags, "sandbox="+a.Sandbox)
		}
		if a.Approval != "" {
			tags = append(tags, "approval="+a.Approval)
		}
		suffix := ""
		if len(tags) > 0 {
			suffix = "  [" + strings.Join(tags, " · ") + "]"
		}
		fmt.Fprintf(stdout, "    %d. %s%s\n", i+1, a.Name, suffix)
		if a.Descr != "" {
			fmt.Fprintf(stdout, "       descr: %s\n", a.Descr)
		}
		if brief := strings.TrimSpace(a.InitialMessage); brief != "" {
			for _, line := range strings.Split(a.InitialMessage, "\n") {
				fmt.Fprintf(stdout, "       │ %s\n", line)
			}
		}
	}
	if len(t.WorkPattern) > 0 {
		fmt.Fprintf(stdout, "  work pattern (%d step%s, delivered in order after the roster spawns):\n",
			len(t.WorkPattern), plural(len(t.WorkPattern)))
		for i, e := range t.WorkPattern {
			fmt.Fprintf(stdout, "    %d. → %s\n", i+1, e.SendTo)
			for _, line := range strings.Split(e.Value, "\n") {
				fmt.Fprintf(stdout, "       │ %s\n", line)
			}
		}
	}
	return rcOK
}

// ---- create / edit ----

type templatesCreateParams struct {
	File string `long:"file" short:"f" help:"Path to a template JSON file ('-' reads stdin). The JSON shape matches 'templates show <name> --json'."`
}

func templatesCreateCmd() *cobra.Command {
	return boa.CmdT[templatesCreateParams]{
		Use:   "create --file <path>",
		Short: "Create a group template from a JSON file",
		Long: "Reads a template definition as JSON from --file (or --file - for stdin) and creates it. The JSON " +
			"shape is what 'templates show <name> --json' emits: {name, descr, default_context, agents:[{name, " +
			"role, descr, initial_message, is_owner, permissions, spawn_profile, harness, model, effort, sandbox, " +
			"approval}]}. A template is structured (nested agents with " +
			"multi-line briefs), so it is supplied as a file rather than via flags. Bootstrap one with " +
			"'templates from-group' or by editing another template's --json output.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *templatesCreateParams, _ *cobra.Command, _ []string) {
			os.Exit(runTemplatesCreate(p, os.Stdin, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runTemplatesCreate(p *templatesCreateParams, stdin io.Reader, stdout, stderr io.Writer) int {
	tmpl, rc := loadTemplateFile(p.File, stdin, stderr)
	if rc != rcOK {
		return rc
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var resp struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	if err := DaemonRequest(http.MethodPost, "/v1/templates", tmpl, &resp, DaemonOpts{}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	fmt.Fprintf(stdout, "Created template %q (#%d) with %d agent%s\n",
		resp.Name, resp.ID, len(tmpl.Agents), plural(len(tmpl.Agents)))
	return rcOK
}

type templatesEditParams struct {
	Name string `pos:"true" help:"Name of the template to replace (from 'tclaude agent templates ls')."`
	File string `long:"file" short:"f" help:"Path to a template JSON file ('-' reads stdin) holding the FULL new state."`
}

func templatesEditCmd() *cobra.Command {
	return boa.CmdT[templatesEditParams]{
		Use:   "edit <name> --file <path>",
		Short: "Replace a group template from a JSON file",
		Long: "Replaces the named template wholesale with the JSON in --file (or --file - for stdin) — it is a " +
			"full replace, not a field merge, so post the template's complete desired state. The body's `name` " +
			"may differ from <name> to rename the template. Typical loop: 'templates show X --json > x.json', " +
			"edit x.json, 'templates edit X --file x.json'.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *templatesEditParams, _ *cobra.Command, _ []string) {
			os.Exit(runTemplatesEdit(p, os.Stdin, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runTemplatesEdit(p *templatesEditParams, stdin io.Reader, stdout, stderr io.Writer) int {
	name := strings.TrimSpace(p.Name)
	if name == "" {
		fmt.Fprintln(stderr, "Error: a template name is required")
		return rcInvalidArg
	}
	tmpl, rc := loadTemplateFile(p.File, stdin, stderr)
	if rc != rcOK {
		return rc
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var resp struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	if err := DaemonRequest(http.MethodPatch, "/v1/templates/"+url.PathEscape(name), tmpl, &resp, DaemonOpts{}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if resp.Name != name {
		fmt.Fprintf(stdout, "Updated template %q → renamed to %q\n", name, resp.Name)
	} else {
		fmt.Fprintf(stdout, "Updated template %q\n", resp.Name)
	}
	return rcOK
}

// loadTemplateFile reads a template JSON file (or stdin for "-") and
// unmarshals it. A missing --file, an unreadable path, or malformed
// JSON each surface as a clear error with the matching rc.
func loadTemplateFile(file string, stdin io.Reader, stderr io.Writer) (*templateJSON, int) {
	if strings.TrimSpace(file) == "" {
		fmt.Fprintln(stderr, "Error: --file is required (path to a template JSON file, or - to read stdin)")
		return nil, rcInvalidArg
	}
	raw, rc := resolveBodyInput("", file, "--file", stdin, stderr)
	if rc != rcOK {
		return nil, rc
	}
	var tmpl templateJSON
	if err := json.Unmarshal([]byte(raw), &tmpl); err != nil {
		fmt.Fprintf(stderr, "Error: --file is not valid template JSON: %v\n", err)
		return nil, rcInvalidArg
	}
	return &tmpl, rcOK
}

// ---- rm ----

type templatesRmParams struct {
	Name string `pos:"true" help:"Template name to delete (from 'tclaude agent templates ls')."`
}

func templatesRmCmd() *cobra.Command {
	return boa.CmdT[templatesRmParams]{
		Use:         "rm <name>",
		Short:       "Delete a group template",
		Long:        "Removes a template blueprint. Groups already instantiated from it are left untouched — only the blueprint is deleted.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *templatesRmParams, _ *cobra.Command, _ []string) {
			os.Exit(runTemplatesRm(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runTemplatesRm(p *templatesRmParams, stdout, stderr io.Writer) int {
	name := strings.TrimSpace(p.Name)
	if name == "" {
		fmt.Fprintln(stderr, "Error: a template name is required")
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	if err := DaemonRequest(http.MethodDelete, "/v1/templates/"+url.PathEscape(name), nil, nil, DaemonOpts{}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	fmt.Fprintf(stdout, "Deleted template %q\n", name)
	return rcOK
}

// ---- instantiate ----

type templatesInstantiateParams struct {
	Name     string `pos:"true" help:"Template to instantiate (from 'tclaude agent templates ls')."`
	Group    string `long:"group" help:"Name for the new group. Also the prefix for every spawned agent's name (agent 'PO' → '<group>-PO')."`
	Task     string `long:"task" optional:"true" help:"The task / project for this group — folded into the group context every spawned agent sees. Use --task-file for long or multi-line text."`
	TaskFile string `long:"task-file" optional:"true" help:"Read the task text from this file ('-' reads stdin). Sidesteps shell quoting; best for long, multi-line briefs. Mutually exclusive with --task."`
	Cwd      string `long:"cwd" optional:"true" help:"Working directory the agents spawn in (~ expands). Must exist. Empty inherits the daemon's cwd."`
	Descr    string `long:"descr" optional:"true" help:"One-line description for the new group. Defaults to 'Instantiated from template <name>'."`
}

func templatesInstantiateCmd() *cobra.Command {
	return boa.CmdT[templatesInstantiateParams]{
		Use:   "instantiate <name> --group <group>",
		Short: "Create a group from a template and spawn its team",
		Long: "Creates a fresh group named --group and spawns one agent per template spec, named '<group>-<agent>'. " +
			"The --task text is folded into the group's shared context, so every spawned agent's startup briefing " +
			"carries it; give it inline with --task or, for long / multi-line text, with --task-file. The template's " +
			"owner agent(s) are granted group ownership and each agent its permission slugs. Spawning a whole team " +
			"can take some time; a per-agent failure is reported, not rolled back.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *templatesInstantiateParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Name).SetAlternativesFunc(completeTemplateNames)
			return nil
		},
		RunFunc: func(p *templatesInstantiateParams, _ *cobra.Command, _ []string) {
			os.Exit(runTemplatesInstantiate(p, os.Stdin, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

// instantiateAgentResult / instantiateResponse mirror the daemon's
// instantiate response (see agentd/templates.go).
type instantiateAgentResult struct {
	Name      string   `json:"name"`
	FinalName string   `json:"final_name"`
	ConvID    string   `json:"conv_id"`
	Owner     bool     `json:"owner"`
	Granted   []string `json:"granted"`
	Error     string   `json:"error"`
}

type instantiateResponse struct {
	Group            string                   `json:"group"`
	Template         string                   `json:"template"`
	Agents           []instantiateAgentResult `json:"agents"`
	Spawned          int                      `json:"spawned"`
	Failed           int                      `json:"failed"`
	PatternDelivered int                      `json:"pattern_delivered"`
	PatternErrors    []string                 `json:"pattern_errors"`
}

func runTemplatesInstantiate(p *templatesInstantiateParams, stdin io.Reader, stdout, stderr io.Writer) int {
	name := strings.TrimSpace(p.Name)
	if name == "" {
		fmt.Fprintln(stderr, "Error: a template name is required")
		return rcInvalidArg
	}
	if strings.TrimSpace(p.Group) == "" {
		fmt.Fprintln(stderr, "Error: --group is required (the name for the new group)")
		return rcInvalidArg
	}
	task, rc := resolveBodyInput(p.Task, p.TaskFile, "--task", stdin, stderr)
	if rc != rcOK {
		return rc
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	body := map[string]any{"group_name": strings.TrimSpace(p.Group)}
	if task != "" {
		body["task"] = task
	}
	if c := strings.TrimSpace(p.Cwd); c != "" {
		body["cwd"] = c
	}
	if d := strings.TrimSpace(p.Descr); d != "" {
		body["descr"] = d
	}
	var resp instantiateResponse
	// Instantiation spawns the whole team sequentially — each spawn polls
	// for a conv-id — so it can run well past the default 10s budget.
	if err := DaemonRequest(http.MethodPost, "/v1/templates/"+url.PathEscape(name)+"/instantiate",
		body, &resp, DaemonOpts{Timeout: 5 * time.Minute}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	fmt.Fprintf(stdout, "Instantiated group %q from template %q: %d spawned, %d failed\n",
		resp.Group, resp.Template, resp.Spawned, resp.Failed)
	for _, a := range resp.Agents {
		if a.Error != "" {
			fmt.Fprintf(stdout, "  ✗ %-24s  %s\n", a.FinalName, a.Error)
			continue
		}
		tags := []string{"conv " + short(a.ConvID)}
		if a.Owner {
			tags = append(tags, "owner")
		}
		if len(a.Granted) > 0 {
			tags = append(tags, "granted: "+strings.Join(a.Granted, ","))
		}
		fmt.Fprintf(stdout, "  ✓ %-24s  %s\n", a.FinalName, strings.Join(tags, "  "))
	}
	if resp.PatternDelivered > 0 {
		fmt.Fprintf(stdout, "  work pattern: %d message%s delivered\n",
			resp.PatternDelivered, plural(resp.PatternDelivered))
	}
	for _, e := range resp.PatternErrors {
		fmt.Fprintf(stdout, "  ⚠ work pattern: %s\n", e)
	}
	// A partial (or total) spawn failure is a non-zero exit so scripts
	// notice — the group + any spawned agents still exist for the human
	// to finish or retry by hand.
	if resp.Failed > 0 {
		fmt.Fprintf(stderr, "Error: %d of %d agent(s) failed to spawn — see above\n",
			resp.Failed, resp.Failed+resp.Spawned)
		return rcIOFailure
	}
	return rcOK
}

// ---- from-group ----

type templatesFromGroupParams struct {
	Group        string `pos:"true" help:"Existing group to snapshot."`
	TemplateName string `pos:"true" help:"Name for the new template."`
	Update       bool   `help:"Re-snapshot into an existing template of this name, in place: roster, owner flags, permissions and context are re-traced from the group; curated per-agent task briefs are kept for matching agent names. Creates the template if it doesn't exist."`
}

func templatesFromGroupCmd() *cobra.Command {
	return boa.CmdT[templatesFromGroupParams]{
		Use:   "from-group <group> <template-name>",
		Short: "Snapshot an existing group into a new (or existing, --update) template",
		Long: "Captures a live group's structure — its context plus one agent per member (role, descr, owner flag, " +
			"per-agent permission grants) — into a new template. Per-agent task briefs come through blank: a live " +
			"group has no stored brief per member, so fill them in afterwards with 'templates edit'.\n\n" +
			"With --update, a template that already exists under this name is re-snapshotted IN PLACE from the " +
			"group's current state; agents that round-trip by name (members titled \"<group>-<agent>\", as " +
			"instantiate names them) keep their curated task briefs.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *templatesFromGroupParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Group).SetAlternativesFunc(completeGroupNames)
			return nil
		},
		RunFunc: func(p *templatesFromGroupParams, _ *cobra.Command, _ []string) {
			os.Exit(runTemplatesFromGroup(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runTemplatesFromGroup(p *templatesFromGroupParams, stdout, stderr io.Writer) int {
	group := strings.TrimSpace(p.Group)
	tmplName := strings.TrimSpace(p.TemplateName)
	if group == "" || tmplName == "" {
		fmt.Fprintln(stderr, "Error: both <group> and <template-name> are required")
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	body := map[string]any{"group": group, "template_name": tmplName, "update": p.Update}
	// The update-mode response embeds the template flat and adds a
	// roster-diff report; the create response is the bare template, so
	// the extra fields simply stay zero.
	var t struct {
		templateJSON
		Updated    bool     `json:"updated"`
		BriefsKept []string `json:"briefs_kept"`
		Added      []string `json:"added"`
		Removed    []string `json:"removed"`
	}
	if err := DaemonRequest(http.MethodPost, "/v1/templates/from-group", body, &t, DaemonOpts{}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if t.Updated {
		fmt.Fprintf(stdout, "Updated template %q from group %q — %d agent%s\n",
			t.Name, group, len(t.Agents), plural(len(t.Agents)))
		fmt.Fprintf(stdout, "  briefs kept: %s; added: %s; removed: %s\n",
			orNone(t.BriefsKept), orNone(t.Added), orNone(t.Removed))
		return rcOK
	}
	fmt.Fprintf(stdout, "Created template %q from group %q with %d agent%s\n",
		t.Name, group, len(t.Agents), plural(len(t.Agents)))
	fmt.Fprintln(stdout, "  Per-agent task briefs are blank — fill them in with `tclaude agent templates edit "+t.Name+" --file …`")
	return rcOK
}

// ---- export / import (JOH-341) ----

type templatesExportParams struct {
	Name string `pos:"true" help:"Template to export (from 'tclaude agent templates ls')."`
	File string `long:"file" short:"f" optional:"true" help:"Write the export to this file instead of stdout. By convention '<name>.task-force.json'."`
}

func templatesExportCmd() *cobra.Command {
	return boa.CmdT[templatesExportParams]{
		Use:   "export <name>",
		Short: "Export a template as a portable task-force JSON file",
		Long: "Emits the named template wrapped in a small versioned envelope — a portable blueprint you can share " +
			"with a friend, a coworker, or your own other machine and re-import with 'templates import'. Writes to " +
			"stdout by default, or to --file. The file carries no machine-local identity (no DB ids, no conv links); " +
			"spawn-profile references and permission slugs travel by name and are validated on import.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *templatesExportParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Name).SetAlternativesFunc(completeTemplateNames)
			return nil
		},
		RunFunc: func(p *templatesExportParams, _ *cobra.Command, _ []string) {
			os.Exit(runTemplatesExport(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runTemplatesExport(p *templatesExportParams, stdout, stderr io.Writer) int {
	name := strings.TrimSpace(p.Name)
	if name == "" {
		fmt.Fprintln(stderr, "Error: a template name is required")
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var env templateExportEnvelope
	if err := DaemonRequest(http.MethodGet, "/v1/templates/"+url.PathEscape(name)+"/export", nil, &env, DaemonOpts{}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	buf, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		fmt.Fprintf(stderr, "Error: encoding export JSON: %v\n", err)
		return rcIOFailure
	}
	buf = append(buf, '\n')
	if file := strings.TrimSpace(p.File); file != "" {
		if err := os.WriteFile(file, buf, 0o644); err != nil {
			fmt.Fprintf(stderr, "Error: writing %s: %v\n", file, err)
			return rcIOFailure
		}
		fmt.Fprintf(stderr, "Exported template %q → %s (%d agent%s)\n",
			name, file, len(env.Template.Agents), plural(len(env.Template.Agents)))
		return rcOK
	}
	if _, err := stdout.Write(buf); err != nil {
		fmt.Fprintf(stderr, "Error: writing export: %v\n", err)
		return rcIOFailure
	}
	return rcOK
}

type templatesImportParams struct {
	File   string `long:"file" short:"f" help:"Path to a task-force JSON file ('-' reads stdin) produced by 'templates export'."`
	As     string `long:"as" optional:"true" help:"Import under this name instead of the name in the file (sidesteps a collision, or just renames)."`
	Update bool   `long:"update" optional:"true" help:"Overwrite an existing template of the target name in place. Without it, a name collision is an error."`
}

func templatesImportCmd() *cobra.Command {
	return boa.CmdT[templatesImportParams]{
		Use:   "import --file <path>",
		Short: "Import a template from a portable task-force JSON file",
		Long: "Reads a task-force export (from 'templates export') via --file (or --file - for stdin) and stores it as " +
			"a local template. A name collision is an error unless you pass --update (overwrite in place) or --as " +
			"<new-name> (import under a different name). References that don't exist on this machine degrade rather " +
			"than fail: an unknown spawn-profile reference is dropped (the agent falls back to the group/harness " +
			"default) and unknown permission slugs are dropped — each reported as a warning. An export from a NEWER " +
			"tclaude is rejected with an upgrade message.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *templatesImportParams, _ *cobra.Command, _ []string) {
			os.Exit(runTemplatesImport(p, os.Stdin, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runTemplatesImport(p *templatesImportParams, stdin io.Reader, stdout, stderr io.Writer) int {
	if strings.TrimSpace(p.File) == "" {
		fmt.Fprintln(stderr, "Error: --file is required (path to a task-force JSON file, or - to read stdin)")
		return rcInvalidArg
	}
	raw, rc := resolveBodyInput("", p.File, "--file", stdin, stderr)
	if rc != rcOK {
		return rc
	}
	// Send the file's bytes verbatim so the daemon — the authority on the
	// envelope format and version — does the parsing and version-gating. A
	// local light syntax check gives a friendlier error than a raw 400.
	if !json.Valid([]byte(raw)) {
		fmt.Fprintln(stderr, "Error: --file is not valid JSON")
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	path := "/v1/templates/import"
	q := url.Values{}
	if as := strings.TrimSpace(p.As); as != "" {
		q.Set("as", as)
	}
	if p.Update {
		q.Set("update", "true")
	}
	if enc := q.Encode(); enc != "" {
		path += "?" + enc
	}
	var res struct {
		Imported string   `json:"imported"`
		Updated  bool     `json:"updated"`
		Warnings []string `json:"warnings"`
	}
	if err := DaemonPostRaw(path, "application/json", []byte(raw), &res); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	verb := "Imported"
	if res.Updated {
		verb = "Updated (overwrote)"
	}
	fmt.Fprintf(stdout, "%s template %q\n", verb, res.Imported)
	for _, wmsg := range res.Warnings {
		fmt.Fprintf(stdout, "  ⚠ %s\n", wmsg)
	}
	return rcOK
}

// orNone renders a name list for the from-group change report — "none"
// when empty.
func orNone(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// plural returns "s" unless n == 1 — for "1 agent" / "3 agents".
func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
