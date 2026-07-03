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
		if len(a.Permissions) > 0 {
			tags = append(tags, "perms="+strings.Join(a.Permissions, ","))
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
			"role, descr, initial_message, is_owner, permissions}]}. A template is structured (nested agents with " +
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
