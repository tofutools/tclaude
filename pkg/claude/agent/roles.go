package agent

import (
	"encoding/json"
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

// `tclaude agent roles` — the role library (JOH-240).
//
// A role is a named, reusable bundle of defaults a template roster agent can
// reference: a canonical role-brief (folded into the agent's startup context),
// a default launch shape (harness/model/effort/sandbox/approval or a spawn
// profile), and a default permission set. The CLI twin of the dashboard's
// Roles editor — a thin client over the daemon's /v1/roles endpoints.
//
// Verbs: ls, show, create, edit, rm. Writes are gated daemon-side on
// roles.manage (effectively human-only); reads (ls / show) are open.

// roleJSON mirrors the daemon's wire shape (see agentd/roles.go). create /
// edit accept this JSON via --file; show --json emits it.
type roleJSON struct {
	Name         string   `json:"name"`
	Descr        string   `json:"descr,omitempty"`
	Brief        string   `json:"brief,omitempty"`
	SpawnProfile string   `json:"spawn_profile,omitempty"`
	Harness      string   `json:"harness,omitempty"`
	Model        string   `json:"model,omitempty"`
	Effort       string   `json:"effort,omitempty"`
	Sandbox      string   `json:"sandbox,omitempty"`
	Approval     string   `json:"approval,omitempty"`
	Permissions  []string `json:"permissions"`
	CreatedAt    string   `json:"created_at,omitempty"`
	UpdatedAt    string   `json:"updated_at,omitempty"`
}

func rolesCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:   "roles",
		Short: "Manage the role library (named, reusable agent-role defaults)",
		Long: "List, inspect, create, edit and delete roles. A role is a named bundle of defaults a template roster " +
			"agent can reference: a canonical role-brief (prepended to that agent's startup context), a default " +
			"launch shape (harness/model/effort/sandbox/approval or a spawn profile), and a default permission set. " +
			"A referencing agent's own fields override the role's; the role fills what the agent leaves blank. The " +
			"CLI twin of the dashboard's Roles editor.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			rolesLsCmd(),
			rolesShowCmd(),
			rolesCreateCmd(),
			rolesEditCmd(),
			rolesRmCmd(),
		},
	}.ToCobra()
}

// ---- ls ----

func rolesLsCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:         "ls",
		Short:       "List roles in the library",
		Long:        "Returns every role with its default launch shape, permission count and one-line description.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(_ *struct{}, _ *cobra.Command, _ []string) {
			os.Exit(runRolesLs(os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runRolesLs(stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var roles []roleJSON
	if err := DaemonRequest(http.MethodGet, "/v1/roles", nil, &roles, DaemonOpts{}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if len(roles) == 0 {
		fmt.Fprintln(stdout, "(no roles)")
		return rcOK
	}
	fmt.Fprintf(stdout, "%-16s  %-10s  %-7s  %s\n", "NAME", "LAUNCH", "PERMS", "DESCR")
	fmt.Fprintln(stdout, strings.Repeat("─", 80))
	for _, rl := range roles {
		launch := roleLaunchSummary(rl)
		fmt.Fprintf(stdout, "%-16s  %-10s  %-7d  %s\n",
			rl.Name, truncate(launch, 10), len(rl.Permissions), truncate(rl.Descr, 40))
	}
	return rcOK
}

// roleLaunchSummary renders a compact one-cell view of a role's default launch
// shape for the ls table: the spawn-profile name if set, else the model /
// harness, else "—".
func roleLaunchSummary(rl roleJSON) string {
	switch {
	case rl.SpawnProfile != "":
		return "@" + rl.SpawnProfile
	case rl.Model != "":
		return rl.Model
	case rl.Harness != "":
		return rl.Harness
	default:
		return "—"
	}
}

// ---- show ----

type rolesShowParams struct {
	Name string `pos:"true" help:"Role name (from 'tclaude agent roles ls')."`
	JSON bool   `long:"json" optional:"true" help:"Emit the raw role JSON instead of the human-readable view. The JSON round-trips: edit it and feed it back via 'roles edit <name> --file -'."`
}

func rolesShowCmd() *cobra.Command {
	return boa.CmdT[rolesShowParams]{
		Use:         "show <name>",
		Short:       "Show one role in detail",
		Long:        "Prints a role's brief, default launch shape and default permissions. With --json, emits the raw wire JSON — the same shape 'create' / 'edit' accept via --file.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *rolesShowParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Name).SetAlternativesFunc(completeRoleNames)
			return nil
		},
		RunFunc: func(p *rolesShowParams, _ *cobra.Command, _ []string) {
			os.Exit(runRolesShow(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runRolesShow(p *rolesShowParams, stdout, stderr io.Writer) int {
	name := strings.TrimSpace(p.Name)
	if name == "" {
		fmt.Fprintln(stderr, "Error: a role name is required")
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var rl roleJSON
	if err := DaemonRequest(http.MethodGet, "/v1/roles/"+url.PathEscape(name), nil, &rl, DaemonOpts{}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if p.JSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rl); err != nil {
			fmt.Fprintf(stderr, "Error: encoding role JSON: %v\n", err)
			return rcIOFailure
		}
		return rcOK
	}
	printRoleHuman(stdout, rl)
	return rcOK
}

// printRoleHuman renders a role's human-readable detail view — name, descr, the
// set launch fields (stable order), the permission slugs, and the brief. The
// transparency floor for a terminal-driven operator (JOH-351): the same
// descr / launch / grants / brief the dashboard role-inspect panel surfaces.
// Pure (writer in) so it is unit-tested without a daemon.
func printRoleHuman(w io.Writer, rl roleJSON) {
	fmt.Fprintf(w, "Role: %s\n", rl.Name)
	if rl.Descr != "" {
		fmt.Fprintf(w, "  descr:   %s\n", rl.Descr)
	}
	launch := []string{}
	if rl.SpawnProfile != "" {
		launch = append(launch, "profile="+rl.SpawnProfile)
	}
	// Render launch fields in a stable order.
	for _, kv := range []struct{ k, v string }{
		{"harness", rl.Harness}, {"model", rl.Model}, {"effort", rl.Effort},
		{"sandbox", rl.Sandbox}, {"approval", rl.Approval},
	} {
		if kv.v != "" {
			launch = append(launch, kv.k+"="+kv.v)
		}
	}
	if len(launch) > 0 {
		fmt.Fprintf(w, "  launch:  %s\n", strings.Join(launch, " · "))
	}
	if len(rl.Permissions) > 0 {
		fmt.Fprintf(w, "  perms:   %s\n", strings.Join(rl.Permissions, ", "))
	}
	if brief := strings.TrimSpace(rl.Brief); brief != "" {
		fmt.Fprintln(w, "  brief:")
		for _, line := range strings.Split(rl.Brief, "\n") {
			fmt.Fprintf(w, "    %s\n", line)
		}
	}
}

// ---- create / edit ----

type rolesCreateParams struct {
	File string `long:"file" short:"f" help:"Path to a role JSON file ('-' reads stdin). The JSON shape matches 'roles show <name> --json'."`
}

func rolesCreateCmd() *cobra.Command {
	return boa.CmdT[rolesCreateParams]{
		Use:   "create --file <path>",
		Short: "Create a role from a JSON file",
		Long: "Reads a role definition as JSON from --file (or --file - for stdin) and creates it. The JSON shape is " +
			"what 'roles show <name> --json' emits: {name, descr, brief, spawn_profile, harness, model, effort, " +
			"sandbox, approval, permissions}. A role carries a multi-line brief, so it is supplied as a file rather " +
			"than via flags.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *rolesCreateParams, _ *cobra.Command, _ []string) {
			os.Exit(runRolesCreate(p, os.Stdin, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runRolesCreate(p *rolesCreateParams, stdin io.Reader, stdout, stderr io.Writer) int {
	rl, rc := loadRoleFile(p.File, stdin, stderr)
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
	if err := DaemonRequest(http.MethodPost, "/v1/roles", rl, &resp, DaemonOpts{}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	fmt.Fprintf(stdout, "Created role %q (#%d)\n", resp.Name, resp.ID)
	return rcOK
}

type rolesEditParams struct {
	Name string `pos:"true" help:"Name of the role to replace (from 'tclaude agent roles ls')."`
	File string `long:"file" short:"f" help:"Path to a role JSON file ('-' reads stdin) holding the FULL new state."`
}

func rolesEditCmd() *cobra.Command {
	return boa.CmdT[rolesEditParams]{
		Use:   "edit <name> --file <path>",
		Short: "Replace a role from a JSON file",
		Long: "Replaces the named role wholesale with the JSON in --file (or --file - for stdin) — a full replace, not " +
			"a field merge, so post the role's complete desired state. The body's `name` may differ from <name> to " +
			"rename the role. Typical loop: 'roles show X --json > x.json', edit x.json, 'roles edit X --file x.json'.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *rolesEditParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Name).SetAlternativesFunc(completeRoleNames)
			return nil
		},
		RunFunc: func(p *rolesEditParams, _ *cobra.Command, _ []string) {
			os.Exit(runRolesEdit(p, os.Stdin, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runRolesEdit(p *rolesEditParams, stdin io.Reader, stdout, stderr io.Writer) int {
	name := strings.TrimSpace(p.Name)
	if name == "" {
		fmt.Fprintln(stderr, "Error: a role name is required")
		return rcInvalidArg
	}
	rl, rc := loadRoleFile(p.File, stdin, stderr)
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
	if err := DaemonRequest(http.MethodPatch, "/v1/roles/"+url.PathEscape(name), rl, &resp, DaemonOpts{}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if resp.Name != name {
		fmt.Fprintf(stdout, "Updated role %q → renamed to %q\n", name, resp.Name)
	} else {
		fmt.Fprintf(stdout, "Updated role %q\n", resp.Name)
	}
	return rcOK
}

// loadRoleFile reads a role JSON file (or stdin for "-") and unmarshals it.
func loadRoleFile(file string, stdin io.Reader, stderr io.Writer) (*roleJSON, int) {
	if strings.TrimSpace(file) == "" {
		fmt.Fprintln(stderr, "Error: --file is required (path to a role JSON file, or - to read stdin)")
		return nil, rcInvalidArg
	}
	raw, rc := resolveBodyInput("", file, "--file", stdin, stderr)
	if rc != rcOK {
		return nil, rc
	}
	var rl roleJSON
	if err := json.Unmarshal([]byte(raw), &rl); err != nil {
		fmt.Fprintf(stderr, "Error: --file is not valid role JSON: %v\n", err)
		return nil, rcInvalidArg
	}
	return &rl, rcOK
}

// ---- rm ----

type rolesRmParams struct {
	Name string `pos:"true" help:"Role name to delete (from 'tclaude agent roles ls')."`
}

func rolesRmCmd() *cobra.Command {
	return boa.CmdT[rolesRmParams]{
		Use:   "rm <name>",
		Short: "Delete a role",
		Long: "Removes a role from the library. Roles resolve at deploy time, so a delete is REFUSED while any template " +
			"still references the role — the error lists the referencing templates; edit them to drop or repoint the " +
			"reference first. A deleted canonical seed role reappears the next time the daemon opens the database (seeds " +
			"self-heal).",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *rolesRmParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Name).SetAlternativesFunc(completeRoleNames)
			return nil
		},
		RunFunc: func(p *rolesRmParams, _ *cobra.Command, _ []string) {
			os.Exit(runRolesRm(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runRolesRm(p *rolesRmParams, stdout, stderr io.Writer) int {
	name := strings.TrimSpace(p.Name)
	if name == "" {
		fmt.Fprintln(stderr, "Error: a role name is required")
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	if err := DaemonRequest(http.MethodDelete, "/v1/roles/"+url.PathEscape(name), nil, nil, DaemonOpts{}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	fmt.Fprintf(stdout, "Deleted role %q\n", name)
	return rcOK
}
