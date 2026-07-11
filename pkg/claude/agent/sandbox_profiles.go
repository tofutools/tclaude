package agent

import (
	"bytes"
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
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/common"
)

// sandboxProfileJSON is the stable operator-facing wire shape. Environment
// values are non-secret launch configuration, but the daemon still gates every
// payload read on sandbox-profiles.manage to avoid disclosing accidental
// credentials to ordinary agents.
type sandboxProfileJSON struct {
	Name        string                           `json:"name"`
	Filesystem  []sandboxpolicy.FilesystemGrant  `json:"filesystem"`
	Environment []sandboxpolicy.EnvironmentEntry `json:"environment"`
	CreatedAt   string                           `json:"created_at,omitempty"`
	UpdatedAt   string                           `json:"updated_at,omitempty"`
}

type sandboxProfileAssignmentJSON struct {
	Group string `json:"group,omitempty"`
	Name  string `json:"name"`
}

func sandboxProfilesCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:         "sandbox-profiles",
		Aliases:     []string{"sandbox-profile"},
		Short:       "Manage filesystem and environment sandbox profiles",
		Long:        "Manage the operator-authored sandbox-profile library. Sandbox profiles grant or deny filesystem access and add non-secret environment values without changing a harness's launch posture. Payload reads and all writes require sandbox-profiles.manage.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			sandboxProfilesLsCmd(), sandboxProfilesShowCmd(), sandboxProfilesCreateCmd(),
			sandboxProfilesEditCmd(), sandboxProfilesRmCmd(), sandboxProfilesDefaultCmd(),
			sandboxProfilesGroupCmd(), sandboxProfilesExportCmd(), sandboxProfilesImportCmd(),
			sandboxProfilesDraftCmd(),
		},
	}.ToCobra()
}

type sandboxProfilesLsParams struct {
	JSON bool `long:"json" help:"Emit the stable JSON array instead of a table"`
}

func sandboxProfilesLsCmd() *cobra.Command {
	return boa.CmdT[sandboxProfilesLsParams]{
		Use: "ls", Aliases: []string{"list"}, Short: "List sandbox profiles", ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *sandboxProfilesLsParams, _ *cobra.Command, _ []string) {
			os.Exit(runSandboxProfilesLs(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runSandboxProfilesLs(p *sandboxProfilesLsParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var profiles []sandboxProfileJSON
	if err := DaemonRequest(http.MethodGet, "/v1/sandbox-profiles", nil, &profiles, DaemonOpts{}); err != nil {
		return printSandboxProfileDaemonError(stderr, err)
	}
	sort.Slice(profiles, func(i, j int) bool { return profiles[i].Name < profiles[j].Name })
	if p.JSON {
		return writeSandboxProfileJSON(stdout, stderr, profiles)
	}
	if len(profiles) == 0 {
		fmt.Fprintln(stdout, "(no sandbox profiles)")
		return rcOK
	}
	fmt.Fprintf(stdout, "%-24s  %10s  %11s\n", "NAME", "FILESYSTEM", "ENVIRONMENT")
	fmt.Fprintln(stdout, strings.Repeat("─", 51))
	for _, profile := range profiles {
		fmt.Fprintf(stdout, "%-24s  %10d  %11d\n", truncate(profile.Name, 24), len(profile.Filesystem), len(profile.Environment))
	}
	return rcOK
}

type sandboxProfilesShowParams struct {
	Name string `pos:"true" help:"Sandbox profile name"`
	JSON bool   `long:"json" help:"Emit the stable profile JSON instead of the human view"`
}

func sandboxProfilesShowCmd() *cobra.Command {
	return boa.CmdT[sandboxProfilesShowParams]{
		Use: "show <name>", Short: "Show one sandbox profile", ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *sandboxProfilesShowParams, _ *cobra.Command, _ []string) {
			os.Exit(runSandboxProfilesShow(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runSandboxProfilesShow(p *sandboxProfilesShowParams, stdout, stderr io.Writer) int {
	name, rc := requireSandboxProfileName(p.Name, stderr)
	if rc != rcOK {
		return rc
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var profile sandboxProfileJSON
	if err := DaemonRequest(http.MethodGet, "/v1/sandbox-profiles/"+url.PathEscape(name), nil, &profile, DaemonOpts{}); err != nil {
		return printSandboxProfileDaemonError(stderr, err)
	}
	if p.JSON {
		return writeSandboxProfileJSON(stdout, stderr, profile)
	}
	printSandboxProfileHuman(stdout, profile)
	return rcOK
}

func printSandboxProfileHuman(w io.Writer, profile sandboxProfileJSON) {
	fmt.Fprintf(w, "Sandbox profile: %s\n", profile.Name)
	if len(profile.Filesystem) == 0 {
		fmt.Fprintln(w, "  filesystem: (none)")
	} else {
		fmt.Fprintln(w, "  filesystem:")
		for _, grant := range profile.Filesystem {
			fmt.Fprintf(w, "    %-5s %s\n", grant.Access, grant.Path)
		}
	}
	if len(profile.Environment) == 0 {
		fmt.Fprintln(w, "  environment: (none)")
	} else {
		fmt.Fprintln(w, "  environment:")
		for _, entry := range profile.Environment {
			fmt.Fprintf(w, "    %s=%s\n", entry.Name, entry.Value)
		}
	}
	if profile.CreatedAt != "" {
		fmt.Fprintf(w, "  created: %s\n", profile.CreatedAt)
	}
	if profile.UpdatedAt != "" {
		fmt.Fprintf(w, "  updated: %s\n", profile.UpdatedAt)
	}
}

type sandboxProfilesFileParams struct {
	File string `long:"file" short:"f" help:"Profile JSON path ('-' reads stdin); use the shape emitted by show --json"`
}

type sandboxProfilesDraftParams struct {
	Token string `long:"token" help:"Opaque dashboard handoff token"`
	File  string `long:"file" short:"f" help:"Profile JSON path ('-' reads stdin)"`
}

func sandboxProfilesDraftCmd() *cobra.Command {
	return boa.CmdT[sandboxProfilesDraftParams]{
		Use:         "draft --token <token> --file <path>",
		Short:       "Submit a validated draft to the human dashboard without saving it",
		Long:        "Submit a sandbox-profile proposal for human preview. This command never creates, edits, assigns, or applies a profile; the human must explicitly save it in the dashboard.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *sandboxProfilesDraftParams, _ *cobra.Command, _ []string) {
			os.Exit(runSandboxProfilesDraft(p, os.Stdin, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runSandboxProfilesDraft(p *sandboxProfilesDraftParams, stdin io.Reader, stdout, stderr io.Writer) int {
	token := strings.TrimSpace(p.Token)
	if token == "" {
		fmt.Fprintln(stderr, "Error: --token is required")
		return rcInvalidArg
	}
	profile, rc := loadSandboxProfileFile(p.File, stdin, stderr)
	if rc != rcOK {
		return rc
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	body := struct {
		Profile sandboxProfileJSON `json:"profile"`
	}{Profile: *profile}
	var resp struct {
		Message string `json:"message"`
	}
	if err := DaemonRequest(http.MethodPost, "/v1/sandbox-profile-drafts/"+url.PathEscape(token), body, &resp, DaemonOpts{}); err != nil {
		return printSandboxProfileDaemonError(stderr, err)
	}
	fmt.Fprintln(stdout, "Draft validated and sent to the dashboard. It has not been saved; the human must preview and explicitly save it.")
	return rcOK
}

func sandboxProfilesCreateCmd() *cobra.Command {
	return boa.CmdT[sandboxProfilesFileParams]{Use: "create --file <path>", Short: "Create a sandbox profile from JSON", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *sandboxProfilesFileParams, _ *cobra.Command, _ []string) {
		os.Exit(runSandboxProfilesCreate(p, os.Stdin, os.Stdout, os.Stderr))
	}}.ToCobra()
}
func runSandboxProfilesCreate(p *sandboxProfilesFileParams, stdin io.Reader, stdout, stderr io.Writer) int {
	profile, rc := loadSandboxProfileFile(p.File, stdin, stderr)
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
	if err := DaemonRequest(http.MethodPost, "/v1/sandbox-profiles", profile, &resp, DaemonOpts{}); err != nil {
		return printSandboxProfileDaemonError(stderr, err)
	}
	fmt.Fprintf(stdout, "Created sandbox profile %q (#%d)\n", resp.Name, resp.ID)
	return rcOK
}

type sandboxProfilesEditParams struct {
	Name string `pos:"true" help:"Sandbox profile to replace"`
	File string `long:"file" short:"f" help:"Full replacement profile JSON path ('-' reads stdin)"`
}

func sandboxProfilesEditCmd() *cobra.Command {
	return boa.CmdT[sandboxProfilesEditParams]{Use: "edit <name> --file <path>", Short: "Replace or rename a sandbox profile from JSON", Long: "Replace the complete profile. The JSON name may differ from <name> to rename it; stable assignment references follow the rename.", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *sandboxProfilesEditParams, _ *cobra.Command, _ []string) {
		os.Exit(runSandboxProfilesEdit(p, os.Stdin, os.Stdout, os.Stderr))
	}}.ToCobra()
}
func runSandboxProfilesEdit(p *sandboxProfilesEditParams, stdin io.Reader, stdout, stderr io.Writer) int {
	name, rc := requireSandboxProfileName(p.Name, stderr)
	if rc != rcOK {
		return rc
	}
	profile, rc := loadSandboxProfileFile(p.File, stdin, stderr)
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
	if err := DaemonRequest(http.MethodPatch, "/v1/sandbox-profiles/"+url.PathEscape(name), profile, &resp, DaemonOpts{}); err != nil {
		return printSandboxProfileDaemonError(stderr, err)
	}
	if resp.Name == name {
		fmt.Fprintf(stdout, "Updated sandbox profile %q\n", name)
	} else {
		fmt.Fprintf(stdout, "Updated sandbox profile %q → renamed to %q\n", name, resp.Name)
	}
	return rcOK
}

type sandboxProfilesRmParams struct {
	Name string `pos:"true" help:"Sandbox profile to delete"`
}

func sandboxProfilesRmCmd() *cobra.Command {
	return boa.CmdT[sandboxProfilesRmParams]{Use: "rm <name>", Aliases: []string{"remove", "delete"}, Short: "Delete a sandbox profile", Long: "Delete a sandbox profile. Global and group assignments to its stable ID are cleared atomically.", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *sandboxProfilesRmParams, _ *cobra.Command, _ []string) {
		os.Exit(runSandboxProfilesRm(p, os.Stdout, os.Stderr))
	}}.ToCobra()
}
func runSandboxProfilesRm(p *sandboxProfilesRmParams, stdout, stderr io.Writer) int {
	name, rc := requireSandboxProfileName(p.Name, stderr)
	if rc != rcOK {
		return rc
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	if err := DaemonRequest(http.MethodDelete, "/v1/sandbox-profiles/"+url.PathEscape(name), nil, nil, DaemonOpts{}); err != nil {
		return printSandboxProfileDaemonError(stderr, err)
	}
	fmt.Fprintf(stdout, "Deleted sandbox profile %q\n", name)
	return rcOK
}

func sandboxProfilesDefaultCmd() *cobra.Command {
	return boa.CmdT[struct{}]{Use: "default", Short: "Show, set or clear the global sandbox-profile default", ParamEnrich: common.DefaultParamEnricher(), SubCmds: []*cobra.Command{sandboxProfilesDefaultShowCmd(), sandboxProfilesDefaultSetCmd(), sandboxProfilesDefaultClearCmd()}}.ToCobra()
}

type sandboxProfilesJSONParams struct {
	JSON bool `long:"json" help:"Emit stable JSON"`
}

func sandboxProfilesDefaultShowCmd() *cobra.Command {
	return boa.CmdT[sandboxProfilesJSONParams]{Use: "show", Short: "Show the global sandbox-profile default", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *sandboxProfilesJSONParams, _ *cobra.Command, _ []string) {
		os.Exit(runSandboxProfilesDefaultShow(p, os.Stdout, os.Stderr))
	}}.ToCobra()
}
func runSandboxProfilesDefaultShow(p *sandboxProfilesJSONParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var resp sandboxProfileAssignmentJSON
	if err := DaemonRequest(http.MethodGet, "/v1/sandbox-profile-default", nil, &resp, DaemonOpts{}); err != nil {
		return printSandboxProfileDaemonError(stderr, err)
	}
	if p.JSON {
		return writeSandboxProfileJSON(stdout, stderr, resp)
	}
	if resp.Name == "" {
		fmt.Fprintln(stdout, "(no global default sandbox profile)")
	} else {
		fmt.Fprintln(stdout, resp.Name)
	}
	return rcOK
}

type sandboxProfilesNameParams struct {
	Name string `pos:"true" help:"Sandbox profile name"`
}

func sandboxProfilesDefaultSetCmd() *cobra.Command {
	return boa.CmdT[sandboxProfilesNameParams]{Use: "set <name>", Short: "Set the global sandbox-profile default", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *sandboxProfilesNameParams, _ *cobra.Command, _ []string) {
		os.Exit(runSandboxProfilesDefaultSet(p, os.Stdout, os.Stderr))
	}}.ToCobra()
}
func runSandboxProfilesDefaultSet(p *sandboxProfilesNameParams, stdout, stderr io.Writer) int {
	return mutateSandboxProfileAssignment(http.MethodPut, "/v1/sandbox-profile-default", "", p.Name, stdout, stderr)
}
func sandboxProfilesDefaultClearCmd() *cobra.Command {
	return boa.CmdT[struct{}]{Use: "clear", Short: "Clear the global sandbox-profile default", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(_ *struct{}, _ *cobra.Command, _ []string) {
		os.Exit(runSandboxProfilesDefaultClear(os.Stdout, os.Stderr))
	}}.ToCobra()
}
func runSandboxProfilesDefaultClear(stdout, stderr io.Writer) int {
	return mutateSandboxProfileAssignment(http.MethodDelete, "/v1/sandbox-profile-default", "", "", stdout, stderr)
}

func sandboxProfilesGroupCmd() *cobra.Command {
	return boa.CmdT[struct{}]{Use: "group", Short: "Manage a group's sandbox-profile assignment", ParamEnrich: common.DefaultParamEnricher(), SubCmds: []*cobra.Command{sandboxProfilesGroupShowCmd(), sandboxProfilesGroupSetCmd(), sandboxProfilesGroupClearCmd()}}.ToCobra()
}

type sandboxProfilesGroupShowParams struct {
	Group string `pos:"true" help:"Group name"`
	JSON  bool   `long:"json" help:"Emit stable JSON"`
}

func sandboxProfilesGroupShowCmd() *cobra.Command {
	return boa.CmdT[sandboxProfilesGroupShowParams]{Use: "show <group>", Short: "Show a group's sandbox-profile assignment", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *sandboxProfilesGroupShowParams, _ *cobra.Command, _ []string) {
		os.Exit(runSandboxProfilesGroupShow(p, os.Stdout, os.Stderr))
	}}.ToCobra()
}
func runSandboxProfilesGroupShow(p *sandboxProfilesGroupShowParams, stdout, stderr io.Writer) int {
	group := strings.TrimSpace(p.Group)
	if group == "" {
		fmt.Fprintln(stderr, "Error: a group name is required")
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var resp sandboxProfileAssignmentJSON
	if err := DaemonRequest(http.MethodGet, "/v1/groups/"+url.PathEscape(group)+"/sandbox-profile", nil, &resp, DaemonOpts{}); err != nil {
		return printSandboxProfileDaemonError(stderr, err)
	}
	if p.JSON {
		return writeSandboxProfileJSON(stdout, stderr, resp)
	}
	if resp.Name == "" {
		fmt.Fprintf(stdout, "%s: (no sandbox profile)\n", resp.Group)
	} else {
		fmt.Fprintf(stdout, "%s: %s\n", resp.Group, resp.Name)
	}
	return rcOK
}

type sandboxProfilesGroupSetParams struct {
	Group string `pos:"true" help:"Group name"`
	Name  string `pos:"true" help:"Sandbox profile name"`
}

func sandboxProfilesGroupSetCmd() *cobra.Command {
	return boa.CmdT[sandboxProfilesGroupSetParams]{Use: "set <group> <name>", Short: "Assign a sandbox profile to a group", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *sandboxProfilesGroupSetParams, _ *cobra.Command, _ []string) {
		os.Exit(runSandboxProfilesGroupSet(p, os.Stdout, os.Stderr))
	}}.ToCobra()
}
func runSandboxProfilesGroupSet(p *sandboxProfilesGroupSetParams, stdout, stderr io.Writer) int {
	return mutateSandboxProfileAssignment(http.MethodPut, "/v1/groups/"+url.PathEscape(strings.TrimSpace(p.Group))+"/sandbox-profile", p.Group, p.Name, stdout, stderr)
}

type sandboxProfilesGroupClearParams struct {
	Group string `pos:"true" help:"Group name"`
}

func sandboxProfilesGroupClearCmd() *cobra.Command {
	return boa.CmdT[sandboxProfilesGroupClearParams]{Use: "clear <group>", Short: "Clear a group's sandbox-profile assignment", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *sandboxProfilesGroupClearParams, _ *cobra.Command, _ []string) {
		os.Exit(runSandboxProfilesGroupClear(p, os.Stdout, os.Stderr))
	}}.ToCobra()
}
func runSandboxProfilesGroupClear(p *sandboxProfilesGroupClearParams, stdout, stderr io.Writer) int {
	return mutateSandboxProfileAssignment(http.MethodDelete, "/v1/groups/"+url.PathEscape(strings.TrimSpace(p.Group))+"/sandbox-profile", p.Group, "", stdout, stderr)
}

func mutateSandboxProfileAssignment(method, path, group, name string, stdout, stderr io.Writer) int {
	group = strings.TrimSpace(group)
	if strings.Contains(path, "/groups//") || (strings.Contains(path, "/groups/") && group == "") {
		fmt.Fprintln(stderr, "Error: a group name is required")
		return rcInvalidArg
	}
	name = strings.TrimSpace(name)
	if method == http.MethodPut && name == "" {
		fmt.Fprintln(stderr, "Error: a sandbox profile name is required")
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var resp sandboxProfileAssignmentJSON
	var body any
	if method == http.MethodPut {
		body = map[string]string{"name": name}
	}
	if err := DaemonRequest(method, path, body, &resp, DaemonOpts{}); err != nil {
		return printSandboxProfileDaemonError(stderr, err)
	}
	if group == "" {
		if resp.Name == "" {
			fmt.Fprintln(stdout, "Global sandbox profile cleared")
		} else {
			fmt.Fprintf(stdout, "Global sandbox profile set to %s\n", resp.Name)
		}
	} else {
		if resp.Name == "" {
			fmt.Fprintf(stdout, "%s: sandbox profile cleared\n", resp.Group)
		} else {
			fmt.Fprintf(stdout, "%s: sandbox profile set to %s\n", resp.Group, resp.Name)
		}
	}
	return rcOK
}

type sandboxProfilesExportParams struct {
	Names              []string `pos:"true" optional:"true" help:"Profile names; omit to export all"`
	IncludeAssignments bool     `long:"include-assignments" help:"Include global/group assignments that reference exported profiles"`
	File               string   `long:"file" short:"f" optional:"true" help:"Write JSON to this path instead of stdout"`
}

func sandboxProfilesExportCmd() *cobra.Command {
	return boa.CmdT[sandboxProfilesExportParams]{Use: "export [name...]", Short: "Export portable sandbox-profile JSON", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *sandboxProfilesExportParams, _ *cobra.Command, _ []string) {
		os.Exit(runSandboxProfilesExport(p, os.Stdout, os.Stderr))
	}}.ToCobra()
}
func runSandboxProfilesExport(p *sandboxProfilesExportParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	query := url.Values{}
	for _, name := range p.Names {
		if name = strings.TrimSpace(name); name != "" {
			query.Add("name", name)
		}
	}
	if p.IncludeAssignments {
		query.Set("include_assignments", "true")
	}
	path := "/v1/sandbox-profiles/export"
	if q := query.Encode(); q != "" {
		path += "?" + q
	}
	// Keep the envelope opaque so a CLI built from an older release does not
	// discard fields added by a newer daemon. Re-indentation is the only change.
	raw, _, err := DaemonGetRaw(path)
	if err != nil {
		return printSandboxProfileDaemonError(stderr, err)
	}
	var out bytes.Buffer
	if err := json.Indent(&out, raw, "", "  "); err != nil {
		fmt.Fprintf(stderr, "Error: malformed sandbox-profile export JSON from daemon: %v\n", err)
		return rcIOFailure
	}
	out.WriteByte('\n')
	if file := strings.TrimSpace(p.File); file != "" {
		if err := os.WriteFile(file, out.Bytes(), 0o644); err != nil {
			fmt.Fprintf(stderr, "Error: writing %s: %v\n", file, err)
			return rcIOFailure
		}
		return rcOK
	}
	if _, err := stdout.Write(out.Bytes()); err != nil {
		fmt.Fprintf(stderr, "Error: writing export: %v\n", err)
		return rcIOFailure
	}
	return rcOK
}

type sandboxProfilesImportParams struct {
	File             string `long:"file" short:"f" help:"Portable export JSON path ('-' reads stdin)"`
	OnConflict       string `long:"on-conflict" optional:"true" help:"Conflict policy: error, skip, or overwrite"`
	ApplyAssignments bool   `long:"apply-assignments" help:"Apply included global/group assignments"`
	JSON             bool   `long:"json" help:"Emit the stable import-result JSON instead of a summary"`
}

func sandboxProfilesImportCmd() *cobra.Command {
	return boa.CmdT[sandboxProfilesImportParams]{Use: "import --file <path>", Short: "Import portable sandbox-profile JSON", Long: "Import an export bundle transactionally. Assignment application is opt-in because group names are machine-local.", ParamEnrich: common.DefaultParamEnricher(), InitFuncCtx: func(ctx *boa.HookContext, p *sandboxProfilesImportParams, _ *cobra.Command) error {
		boa.GetParamT(ctx, &p.OnConflict).SetAlternatives([]string{"error", "skip", "overwrite"})
		return nil
	}, RunFunc: func(p *sandboxProfilesImportParams, _ *cobra.Command, _ []string) {
		os.Exit(runSandboxProfilesImport(p, os.Stdin, os.Stdout, os.Stderr))
	}}.ToCobra()
}
func runSandboxProfilesImport(p *sandboxProfilesImportParams, stdin io.Reader, stdout, stderr io.Writer) int {
	raw, rc := loadSandboxProfileRawFile(p.File, stdin, stderr)
	if rc != rcOK {
		return rc
	}
	// Decode to a generic object solely to overlay CLI import controls. Unknown
	// envelope fields remain intact when the request is re-encoded.
	var env map[string]any
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		fmt.Fprintf(stderr, "Error: --file is not valid sandbox-profile export JSON: %v\n", err)
		return rcInvalidArg
	}
	if env == nil {
		fmt.Fprintln(stderr, "Error: --file must contain a sandbox-profile export JSON object")
		return rcInvalidArg
	}
	conflict := strings.ToLower(strings.TrimSpace(p.OnConflict))
	if conflict == "" {
		conflict = "error"
	}
	if conflict != "error" && conflict != "skip" && conflict != "overwrite" {
		fmt.Fprintln(stderr, "Error: --on-conflict must be error, skip, or overwrite")
		return rcInvalidArg
	}
	// Import controls belong to the invoking operator, not to the file. Always
	// overwrite any values embedded in a hand-edited bundle so assignments and
	// destructive conflict handling remain explicit CLI choices.
	env["on_conflict"] = conflict
	env["apply_assignments"] = p.ApplyAssignments
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var resp struct {
		Imported []string `json:"imported"`
		Skipped  []string `json:"skipped"`
		Warnings []string `json:"warnings"`
	}
	if err := DaemonRequest(http.MethodPost, "/v1/sandbox-profiles/import", env, &resp, DaemonOpts{}); err != nil {
		return printSandboxProfileDaemonError(stderr, err)
	}
	if p.JSON {
		return writeSandboxProfileJSON(stdout, stderr, resp)
	}
	fmt.Fprintf(stdout, "Imported %d sandbox profile(s)", len(resp.Imported))
	if len(resp.Skipped) > 0 {
		fmt.Fprintf(stdout, "; skipped %d", len(resp.Skipped))
	}
	fmt.Fprintln(stdout)
	for _, warning := range resp.Warnings {
		fmt.Fprintf(stdout, "Warning: %s\n", warning)
	}
	return rcOK
}

func loadSandboxProfileFile(path string, stdin io.Reader, stderr io.Writer) (*sandboxProfileJSON, int) {
	raw, rc := loadSandboxProfileRawFile(path, stdin, stderr)
	if rc != rcOK {
		return nil, rc
	}
	var profile sandboxProfileJSON
	if err := json.Unmarshal([]byte(raw), &profile); err != nil {
		fmt.Fprintf(stderr, "Error: --file is not valid sandbox-profile JSON: %v\n", err)
		return nil, rcInvalidArg
	}
	return &profile, rcOK
}
func loadSandboxProfileRawFile(path string, stdin io.Reader, stderr io.Writer) (string, int) {
	if strings.TrimSpace(path) == "" {
		fmt.Fprintln(stderr, "Error: --file is required (path or - for stdin)")
		return "", rcInvalidArg
	}
	return resolveBodyInput("", path, "--file", stdin, stderr)
}
func requireSandboxProfileName(name string, stderr io.Writer) (string, int) {
	name = strings.TrimSpace(name)
	if name == "" {
		fmt.Fprintln(stderr, "Error: a sandbox profile name is required")
		return "", rcInvalidArg
	}
	return name, rcOK
}
func printSandboxProfileDaemonError(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "Error: %v\n", err)
	return MapDaemonErrorToRC(err)
}
func writeSandboxProfileJSON(w, stderr io.Writer, value any) int {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(value); err != nil {
		fmt.Fprintf(stderr, "Error: encoding sandbox-profile JSON: %v\n", err)
		return rcIOFailure
	}
	return rcOK
}
