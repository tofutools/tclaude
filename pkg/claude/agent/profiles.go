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

// `tclaude agent profiles` — the spawn-profile library, read side (JOH-210).
//
// A spawn profile is a named, reusable bundle of (most of) the spawn-agent
// dialog's fields — launch shape (harness/model/effort/sandbox/approval + the
// auto-review/trust-dir/remote-control toggles), identity (name/role/descr/
// initial message), and birth-time access (owner + permission overrides). The
// dashboard's spawn modal pre-fills from a profile client-side; this is the CLI
// twin for the read/introspect half, plus the source of the names you pass to
// `tclaude agent spawn --profile <name>`.
//
// Verbs here are read-only: `ls` and `show`. Mutations (create/edit/delete)
// stay in the dashboard's profile editor — they are gated on profiles.manage
// (effectively human-only), and a profile carries multi-line/permission state
// best edited in the visual form. Reads are open, mirroring `roles ls/show`.
//
// profileJSON mirrors the daemon's wire shape (agentd.spawnProfileJSON) field
// for field, so `show --json` round-trips exactly what the dashboard editor
// posts. Every field is optional: a blank text field / absent toggle is unset.
type profileJSON struct {
	Name string `json:"name"`

	// Launch fields.
	Harness    string `json:"harness,omitempty"`
	Model      string `json:"model,omitempty"`
	Effort     string `json:"effort,omitempty"`
	Sandbox    string `json:"sandbox,omitempty"`
	Approval   string `json:"approval,omitempty"`
	AutoReview *bool  `json:"auto_review,omitempty"`
	TrustDir   *bool  `json:"trust_dir,omitempty"`
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
		Short: "Inspect the spawn-profile library (reusable spawn-dialog field bundles)",
		Long: "List and inspect spawn profiles. A spawn profile is a named, reusable bundle of the spawn-agent " +
			"dialog's fields — a launch shape (harness/model/effort/sandbox/approval + toggles), identity " +
			"(name/role/descr/initial message) and birth-time access (owner + permission overrides). Pass a profile " +
			"name to `tclaude agent spawn --profile <name>` to pre-fill those fields; explicit spawn flags override " +
			"the profile, and the profile overrides the group / harness defaults.\n\n" +
			"This command is read-only (ls / show). Create, edit and delete live in the dashboard's profile editor " +
			"(gated on profiles.manage). Reads are open, like `roles ls/show`.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			profilesLsCmd(),
			profilesShowCmd(),
		},
	}.ToCobra()
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
		{"sandbox", p.Sandbox}, {"approval", p.Approval},
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

	// Birth-time access.
	if p.IsOwner != nil && *p.IsOwner {
		fmt.Fprintln(w, "  owner:   yes")
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
