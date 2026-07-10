package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

// `tclaude agent profiles` — manage the spawn-profile library (JOH-210).
//
// A spawn profile is a named, reusable bundle of (most of) the spawn-agent
// dialog's fields — launch shape (harness/model/effort/sandbox/approval + the
// auto-review/trust-dir/remote-control toggles), identity (name/role/descr/
// initial message), and birth-time access (owner + permission overrides). The
// dashboard's spawn modal pre-fills from a profile client-side; this is the CLI
// twin of its Profiles editor, plus the source of the names you pass to
// `tclaude agent spawn --profile <name>`.
//
// Verbs: `ls`, `show`, `create`, `edit`, `rm`, `default` — the CLI twin of the dashboard's
// profile editor, mirroring `roles ls/show/create/edit/rm`. Reads (ls/show) are
// open; writes (create/edit/rm) rewrite shared spawn config and so are gated
// daemon-side on profiles.manage (effectively human-only — an agent without the
// slug gets a clean permission error). A profile carries multi-line/permission
// state, so like a role it is supplied as a JSON file (--file) rather than via
// a flag-per-field; the JSON round-trips `show --json`.
//
// profileJSON mirrors the daemon's wire shape (agentd.spawnProfileJSON) field
// for field, so `show --json` round-trips exactly what the dashboard editor
// posts. Every field is optional: a blank text field / absent toggle is unset.
type profileJSON struct {
	Name string `json:"name"`

	// Launch fields.
	Harness  string `json:"harness,omitempty"`
	Model    string `json:"model,omitempty"`
	Effort   string `json:"effort,omitempty"`
	Sandbox  string `json:"sandbox,omitempty"`
	Approval string `json:"approval,omitempty"`
	// AskUserQuestionTimeout is the profile's Claude Code AskUserQuestion
	// idle-timeout default (inherit|never|60s|5m|10m; "" = unset), delivered
	// per-spawn via `--settings`. Claude-Code-only.
	AskUserQuestionTimeout string `json:"ask_user_question_timeout,omitempty"`
	AutoReview             *bool  `json:"auto_review,omitempty"`
	TrustDir               *bool  `json:"trust_dir,omitempty"`
	// RemoteControl is the profile's "start with Remote Access on" default
	// (tri-state). NOTE: `tclaude agent spawn --profile` does NOT inherit this —
	// the CLI can't see the group's remote-control policy, which must win, so use
	// the explicit --remote-control flag. It is still surfaced here for parity.
	RemoteControl *bool `json:"remote_control,omitempty"`

	// Identity / enrollment fields.
	AgentName      string `json:"agent_name,omitempty"`
	Role           string `json:"role,omitempty"`
	Descr          string `json:"descr,omitempty"`
	InitialMessage string `json:"initial_message,omitempty"`

	// Dialog toggles.
	SyncWorktree               *bool `json:"sync_worktree,omitempty"`
	AutoFocus                  *bool `json:"auto_focus,omitempty"`
	IncludeGroupDefaultContext *bool `json:"include_group_default_context,omitempty"`

	// Birth-time access controls.
	IsOwner             *bool             `json:"is_owner,omitempty"`
	PermissionOverrides map[string]string `json:"permission_overrides,omitempty"`

	CreatedAt string `json:"created_at,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

func profilesCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:   "profiles",
		Short: "Manage the spawn-profile library (reusable spawn-dialog field bundles)",
		Long: "List, inspect, create, edit and delete spawn profiles. A spawn profile is a named, reusable bundle of " +
			"the spawn-agent dialog's fields — a launch shape (harness/model/effort/sandbox/approval + toggles), " +
			"identity (name/role/descr/initial message) and birth-time access (owner + permission overrides). Pass a " +
			"profile name to `tclaude agent spawn --profile <name>` to pre-fill those fields; explicit spawn flags " +
			"override the profile, and the profile overrides the group / global / harness defaults.\n\n" +
			"The CLI twin of the dashboard's Profiles editor. Reads (ls / show) are open; writes (create / edit / rm) " +
			"are gated daemon-side on profiles.manage (effectively human-only). Mirrors `roles`.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			profilesLsCmd(),
			profilesShowCmd(),
			profilesDefaultCmd(),
			profilesCreateCmd(),
			profilesEditCmd(),
			profilesRmCmd(),
		},
	}.ToCobra()
}

// ---- global default ----

func profilesDefaultCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:         "default",
		Short:       "Show, set or clear the global default spawn profile",
		Long:        "Manage the global default profile used after a group's own default. The dashboard's global profile picker edits this same server-persisted value.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			profilesDefaultShowCmd(),
			profilesDefaultSetCmd(),
			profilesDefaultClearCmd(),
		},
	}.ToCobra()
}

func profilesDefaultShowCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:         "show",
		Short:       "Show the global default spawn profile",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(_ *struct{}, _ *cobra.Command, _ []string) {
			os.Exit(runProfilesDefaultShow(os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

type profilesDefaultSetParams struct {
	Name string `pos:"true" help:"Profile name (from 'tclaude agent profiles ls')."`
}

func profilesDefaultSetCmd() *cobra.Command {
	return boa.CmdT[profilesDefaultSetParams]{
		Use:         "set <name>",
		Short:       "Set the global default spawn profile",
		Long:        "Set the profile whose fields fill spawn parameters left blank by explicit flags, an explicit --profile and the target group's default profile. Gated on profiles.manage.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *profilesDefaultSetParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Name).SetAlternativesFunc(completeSpawnProfileNames)
			return nil
		},
		RunFunc: func(p *profilesDefaultSetParams, _ *cobra.Command, _ []string) {
			os.Exit(runProfilesDefaultSet(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func profilesDefaultClearCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:         "clear",
		Short:       "Clear the global default spawn profile",
		Long:        "Clear the global spawn-profile fallback. Gated on profiles.manage.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(_ *struct{}, _ *cobra.Command, _ []string) {
			os.Exit(runProfilesDefaultClear(os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

type profilesDefaultResponse struct {
	Name string `json:"name"`
}

func runProfilesDefaultShow(stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var resp profilesDefaultResponse
	if err := DaemonRequest(http.MethodGet, "/v1/spawn-profile-default", nil, &resp, DaemonOpts{}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if resp.Name == "" {
		fmt.Fprintln(stdout, "(no global default spawn profile)")
	} else {
		fmt.Fprintln(stdout, resp.Name)
	}
	return rcOK
}

func runProfilesDefaultSet(p *profilesDefaultSetParams, stdout, stderr io.Writer) int {
	name := strings.TrimSpace(p.Name)
	if name == "" {
		fmt.Fprintln(stderr, "Error: a profile name is required")
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var resp profilesDefaultResponse
	if err := DaemonRequest(http.MethodPut, "/v1/spawn-profile-default", map[string]string{"name": name}, &resp, DaemonOpts{}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	fmt.Fprintf(stdout, "Global default profile set to %s\n", resp.Name)
	return rcOK
}

func runProfilesDefaultClear(stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	if err := DaemonRequest(http.MethodDelete, "/v1/spawn-profile-default", nil, nil, DaemonOpts{}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	fmt.Fprintln(stdout, "Global default profile cleared")
	return rcOK
}

// ---- ls ----

func profilesLsCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:         "ls",
		Short:       "List spawn profiles in the library",
		Long:        "Returns every spawn profile with its launch shape (harness/model/effort) and one-line description.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(_ *struct{}, _ *cobra.Command, _ []string) {
			os.Exit(runProfilesLs(os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runProfilesLs(stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var profiles []profileJSON
	if err := DaemonRequest(http.MethodGet, "/v1/spawn-profiles", nil, &profiles, DaemonOpts{}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if len(profiles) == 0 {
		fmt.Fprintln(stdout, "(no spawn profiles)")
		return rcOK
	}
	sort.Slice(profiles, func(i, j int) bool { return profiles[i].Name < profiles[j].Name })
	fmt.Fprintf(stdout, "%-16s  %-8s  %-12s  %-7s  %s\n", "NAME", "HARNESS", "MODEL", "EFFORT", "DESCR")
	fmt.Fprintln(stdout, strings.Repeat("─", 80))
	for _, p := range profiles {
		fmt.Fprintf(stdout, "%-16s  %-8s  %-12s  %-7s  %s\n",
			truncate(p.Name, 16), truncate(dash(p.Harness), 8), truncate(dash(p.Model), 12),
			truncate(dash(p.Effort), 7), truncate(p.Descr, 30))
	}
	return rcOK
}

// dash renders "—" for an unset (blank) field in the ls table.
func dash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

// ---- show ----

type profilesShowParams struct {
	Name string `pos:"true" help:"Profile name (from 'tclaude agent profiles ls')."`
	JSON bool   `long:"json" optional:"true" help:"Emit the raw profile JSON instead of the human-readable view (the same shape the dashboard editor posts)."`
}

func profilesShowCmd() *cobra.Command {
	return boa.CmdT[profilesShowParams]{
		Use:         "show <name>",
		Short:       "Show one spawn profile in detail",
		Long:        "Prints a spawn profile's launch shape, identity defaults and birth-time access. With --json, emits the raw wire JSON.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *profilesShowParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Name).SetAlternativesFunc(completeSpawnProfileNames)
			return nil
		},
		RunFunc: func(p *profilesShowParams, _ *cobra.Command, _ []string) {
			os.Exit(runProfilesShow(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runProfilesShow(p *profilesShowParams, stdout, stderr io.Writer) int {
	name := strings.TrimSpace(p.Name)
	if name == "" {
		fmt.Fprintln(stderr, "Error: a profile name is required")
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	prof, rc := fetchSpawnProfile(name, stderr)
	if rc != rcOK {
		return rc
	}
	if p.JSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(prof); err != nil {
			fmt.Fprintf(stderr, "Error: encoding profile JSON: %v\n", err)
			return rcIOFailure
		}
		return rcOK
	}
	printProfileHuman(stdout, *prof)
	return rcOK
}

// fetchSpawnProfile GETs one spawn profile by name (reads are open on the
// daemon). Returns a clear error on a missing profile so `spawn --profile foo`
// and `profiles show foo` fail fast with the same message. Assumes the caller
// already ran RequireDaemonOrExit.
func fetchSpawnProfile(name string, stderr io.Writer) (*profileJSON, int) {
	var prof profileJSON
	if err := DaemonRequest(http.MethodGet, "/v1/spawn-profiles/"+url.PathEscape(name), nil, &prof, DaemonOpts{}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return nil, MapDaemonErrorToRC(err)
	}
	return &prof, rcOK
}

// ---- create / edit ----

type profilesCreateParams struct {
	File string `long:"file" short:"f" help:"Path to a profile JSON file ('-' reads stdin). The JSON shape matches 'profiles show <name> --json'."`
}

func profilesCreateCmd() *cobra.Command {
	return boa.CmdT[profilesCreateParams]{
		Use:   "create --file <path>",
		Short: "Create a spawn profile from a JSON file",
		Long: "Reads a spawn-profile definition as JSON from --file (or --file - for stdin) and creates it. The JSON " +
			"shape is what 'profiles show <name> --json' emits: {name, harness, model, effort, sandbox, approval, " +
			"agent_name, role, descr, initial_message, is_owner, permission_overrides, …}. A profile carries multi-line " +
			"and permission state, so it is supplied as a file rather than via flags. Gated on profiles.manage.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *profilesCreateParams, _ *cobra.Command, _ []string) {
			os.Exit(runProfilesCreate(p, os.Stdin, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runProfilesCreate(p *profilesCreateParams, stdin io.Reader, stdout, stderr io.Writer) int {
	prof, rc := loadProfileFile(p.File, stdin, stderr)
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
	if err := DaemonRequest(http.MethodPost, "/v1/spawn-profiles", prof, &resp, DaemonOpts{}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	fmt.Fprintf(stdout, "Created profile %q (#%d)\n", resp.Name, resp.ID)
	return rcOK
}

type profilesEditParams struct {
	Name string `pos:"true" help:"Name of the profile to replace (from 'tclaude agent profiles ls')."`
	File string `long:"file" short:"f" help:"Path to a profile JSON file ('-' reads stdin) holding the FULL new state."`
}

func profilesEditCmd() *cobra.Command {
	return boa.CmdT[profilesEditParams]{
		Use:   "edit <name> --file <path>",
		Short: "Replace a spawn profile from a JSON file",
		Long: "Replaces the named profile wholesale with the JSON in --file (or --file - for stdin) — a full replace, " +
			"not a field merge, so post the profile's complete desired state. The body's `name` may differ from <name> " +
			"to rename the profile. Typical loop: 'profiles show X --json > x.json', edit x.json, 'profiles edit X " +
			"--file x.json'. Gated on profiles.manage.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *profilesEditParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Name).SetAlternativesFunc(completeSpawnProfileNames)
			return nil
		},
		RunFunc: func(p *profilesEditParams, _ *cobra.Command, _ []string) {
			os.Exit(runProfilesEdit(p, os.Stdin, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runProfilesEdit(p *profilesEditParams, stdin io.Reader, stdout, stderr io.Writer) int {
	name := strings.TrimSpace(p.Name)
	if name == "" {
		fmt.Fprintln(stderr, "Error: a profile name is required")
		return rcInvalidArg
	}
	prof, rc := loadProfileFile(p.File, stdin, stderr)
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
	if err := DaemonRequest(http.MethodPatch, "/v1/spawn-profiles/"+url.PathEscape(name), prof, &resp, DaemonOpts{}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if resp.Name != name {
		fmt.Fprintf(stdout, "Updated profile %q → renamed to %q\n", name, resp.Name)
	} else {
		fmt.Fprintf(stdout, "Updated profile %q\n", resp.Name)
	}
	return rcOK
}

// loadProfileFile reads a profile JSON file (or stdin for "-") and unmarshals it.
func loadProfileFile(file string, stdin io.Reader, stderr io.Writer) (*profileJSON, int) {
	if strings.TrimSpace(file) == "" {
		fmt.Fprintln(stderr, "Error: --file is required (path to a profile JSON file, or - to read stdin)")
		return nil, rcInvalidArg
	}
	raw, rc := resolveBodyInput("", file, "--file", stdin, stderr)
	if rc != rcOK {
		return nil, rc
	}
	var prof profileJSON
	if err := json.Unmarshal([]byte(raw), &prof); err != nil {
		fmt.Fprintf(stderr, "Error: --file is not valid profile JSON: %v\n", err)
		return nil, rcInvalidArg
	}
	return &prof, rcOK
}

// ---- rm ----

type profilesRmParams struct {
	Name string `pos:"true" help:"Profile name to delete (from 'tclaude agent profiles ls')."`
}

func profilesRmCmd() *cobra.Command {
	return boa.CmdT[profilesRmParams]{
		Use:   "rm <name>",
		Short: "Delete a spawn profile",
		Long: "Removes a spawn profile from the library. A group or the dashboard that names it as a default falls back " +
			"to blank spawn fields until re-pointed; agents already spawned are untouched. Gated on profiles.manage.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *profilesRmParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Name).SetAlternativesFunc(completeSpawnProfileNames)
			return nil
		},
		RunFunc: func(p *profilesRmParams, _ *cobra.Command, _ []string) {
			os.Exit(runProfilesRm(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runProfilesRm(p *profilesRmParams, stdout, stderr io.Writer) int {
	name := strings.TrimSpace(p.Name)
	if name == "" {
		fmt.Fprintln(stderr, "Error: a profile name is required")
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	if err := DaemonRequest(http.MethodDelete, "/v1/spawn-profiles/"+url.PathEscape(name), nil, nil, DaemonOpts{}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	fmt.Fprintf(stdout, "Deleted profile %q\n", name)
	return rcOK
}

// printProfileHuman renders a spawn profile's human-readable detail view — the
// terminal-operator twin of the dashboard's profile-inspect panel. Pure (writer
// in) so it is unit-tested without a daemon. Only set fields are shown.
func printProfileHuman(w io.Writer, p profileJSON) {
	fmt.Fprintf(w, "Profile: %s\n", p.Name)
	if p.Descr != "" {
		fmt.Fprintf(w, "  descr:   %s\n", p.Descr)
	}

	// Launch fields in a stable order.
	launch := []string{}
	for _, kv := range []struct{ k, v string }{
		{"harness", p.Harness}, {"model", p.Model}, {"effort", p.Effort},
		{"sandbox", p.Sandbox}, {"ask_user_question_timeout", p.AskUserQuestionTimeout},
		{"approval", p.Approval},
	} {
		if kv.v != "" {
			launch = append(launch, kv.k+"="+kv.v)
		}
	}
	launch = append(launch, boolFlags([]struct {
		k string
		v *bool
	}{
		{"auto_review", p.AutoReview}, {"trust_dir", p.TrustDir}, {"remote_control", p.RemoteControl},
	})...)
	if len(launch) > 0 {
		fmt.Fprintf(w, "  launch:  %s\n", strings.Join(launch, " · "))
	}

	// Identity defaults.
	ident := []string{}
	if p.AgentName != "" {
		ident = append(ident, "name="+p.AgentName)
	}
	if p.Role != "" {
		ident = append(ident, "role="+p.Role)
	}
	if len(ident) > 0 {
		fmt.Fprintf(w, "  agent:   %s\n", strings.Join(ident, " · "))
	}

	// Dialog toggles.
	toggles := boolFlags([]struct {
		k string
		v *bool
	}{
		{"auto_focus", p.AutoFocus}, {"sync_worktree", p.SyncWorktree},
		{"include_group_context", p.IncludeGroupDefaultContext},
	})
	if len(toggles) > 0 {
		fmt.Fprintf(w, "  toggles: %s\n", strings.Join(toggles, " · "))
	}

	// Birth-time access. IsOwner is tri-state: render yes/no for any set value
	// (an explicit false is a real, saved value distinct from unset) so the human
	// view matches --json and the boolFlags toggles above.
	if p.IsOwner != nil {
		state := "no"
		if *p.IsOwner {
			state = "yes"
		}
		fmt.Fprintf(w, "  owner:   %s\n", state)
	}
	if len(p.PermissionOverrides) > 0 {
		keys := make([]string, 0, len(p.PermissionOverrides))
		for k := range p.PermissionOverrides {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, k+"="+p.PermissionOverrides[k])
		}
		fmt.Fprintf(w, "  perms:   %s\n", strings.Join(parts, ", "))
	}

	if msg := strings.TrimSpace(p.InitialMessage); msg != "" {
		fmt.Fprintln(w, "  initial message:")
		for _, line := range strings.Split(p.InitialMessage, "\n") {
			fmt.Fprintf(w, "    %s\n", line)
		}
	}
}

// boolFlags renders a set of tri-state toggles as "k=on"/"k=off", skipping
// unset (nil) ones — for the profile detail view.
func boolFlags(flags []struct {
	k string
	v *bool
}) []string {
	out := []string{}
	for _, f := range flags {
		if f.v == nil {
			continue
		}
		state := "off"
		if *f.v {
			state = "on"
		}
		out = append(out, f.k+"="+state)
	}
	return out
}
