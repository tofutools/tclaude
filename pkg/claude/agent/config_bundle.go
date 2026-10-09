package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/common/configbundle"
	"github.com/tofutools/tclaude/pkg/common"
)

type configExportParams struct {
	File         string   `long:"file" short:"f" optional:"true" help:"Output file (default stdout)."`
	Only         []string `long:"only" optional:"true" help:"Include section or section/name (repeatable)."`
	Skip         []string `long:"skip" optional:"true" help:"Exclude section or section/name (repeatable)."`
	AllowFlagged bool     `long:"allow-flagged" help:"Explicitly allow suspected credentials in free text. Inspect the file before sharing."`
}
type configImportParams struct {
	KeepPaths bool     `long:"keep-paths" help:"Keep original exported absolute paths; --set overrides individual values."`
	File      string   `long:"file" short:"f" help:"Bundle file; '-' reads stdin."`
	Only      []string `long:"only" optional:"true" help:"Include section or section/name (repeatable)."`
	Skip      []string `long:"skip" optional:"true" help:"Exclude section or section/name (repeatable)."`
	Set       []string `long:"set" optional:"true" help:"Resolve a placeholder with name=value (repeatable)."`
	Apply     bool     `long:"apply" help:"Apply the previewed changes."`
	Replace   bool     `long:"replace" help:"Explicitly overwrite conflicting items."`
	JSON      bool     `long:"json" help:"Print machine-readable preview with before/after values."`
}

// ConfigCmd exposes operator setup transfer independently of federation.
func ConfigCmd() *cobra.Command {
	return boa.CmdT[struct{}]{Use: "config", Short: "Export and import portable setup bundles", ParamEnrich: common.DefaultParamEnricher(), SubCmds: []*cobra.Command{
		boa.CmdT[configExportParams]{Use: "export", Short: "Export a portable config bundle", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *configExportParams, _ *cobra.Command, _ []string) {
			os.Exit(runConfigExport(p, os.Stdout, os.Stderr))
		}}.ToCobra(),
		boa.CmdT[configImportParams]{Use: "import", Short: "Preview and selectively apply a config bundle", Long: "Defaults to preview, including before/after values and security tags. Use --apply to write; conflicts require --replace. Sections: roles, sandbox-profiles, profiles, templates, process-templates, default-permissions, config. Agents need config.import, which permits changing agent authority.", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *configImportParams, _ *cobra.Command, _ []string) {
			os.Exit(runConfigImport(p, os.Stdin, os.Stdout, os.Stderr))
		}}.ToCobra(),
	}}.ToCobra()
}
func runConfigExport(p *configExportParams, out, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	q := url.Values{}
	for _, v := range p.Only {
		q.Add("only", v)
	}
	for _, v := range p.Skip {
		q.Add("skip", v)
	}
	if p.AllowFlagged {
		q.Set("allow_flagged", "true")
	}
	raw, _, err := DaemonGetRaw("/v1/config-bundle/export?" + q.Encode())
	if err != nil {
		printConfigBundleError(stderr, err)
		return MapDaemonErrorToRC(err)
	}
	var b configbundle.Bundle
	if err := json.Unmarshal(raw, &b); err != nil {
		fmt.Fprintln(stderr, err)
		return rcIOFailure
	}
	for _, f := range b.Flags {
		fmt.Fprintf(stderr, "Flag: %s %s: %s\n", f.Item, f.Field, f.Hint)
	}
	for _, field := range b.Omitted {
		fmt.Fprintf(stderr, "Omitted: %s\n", field)
	}
	raw, err = json.MarshalIndent(b, "", "  ")
	if err != nil {
		fmt.Fprintln(stderr, err)
		return rcIOFailure
	}
	raw = append(raw, '\n')
	if p.File != "" {
		if err := os.WriteFile(p.File, raw, 0600); err != nil {
			fmt.Fprintln(stderr, err)
			return rcIOFailure
		}
	} else if _, err := out.Write(raw); err != nil {
		fmt.Fprintln(stderr, err)
		return rcIOFailure
	}
	return rcOK
}
func runConfigImport(p *configImportParams, stdin io.Reader, out, stderr io.Writer) int {
	var raw []byte
	var err error
	if p.File == "-" {
		raw, err = io.ReadAll(io.LimitReader(stdin, 16<<20+1))
	} else {
		raw, err = os.ReadFile(p.File)
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return rcInvalidArg
	}
	if len(raw) > 16<<20 {
		fmt.Fprintln(stderr, "bundle exceeds 16 MiB")
		return rcInvalidArg
	}
	var b configbundle.Bundle
	if err := json.Unmarshal(raw, &b); err != nil {
		fmt.Fprintln(stderr, err)
		return rcInvalidArg
	}
	if err := b.Validate(); err != nil {
		fmt.Fprintln(stderr, err)
		return rcInvalidArg
	}
	values := map[string]string{}
	for _, arg := range p.Set {
		name, value, ok := strings.Cut(arg, "=")
		if !ok || name == "" || name == "HOME" {
			fmt.Fprintln(stderr, "--set requires name=value; HOME is automatic")
			return rcInvalidArg
		}
		values[name] = value
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	in := map[string]any{"bundle": b, "only": p.Only, "skip": p.Skip, "values": values, "apply": p.Apply, "replace": p.Replace, "keep_paths": p.KeepPaths}
	var response struct {
		Changes []struct {
			Item     string          `json:"item"`
			Action   string          `json:"action"`
			Security bool            `json:"security"`
			Before   json.RawMessage `json:"before"`
			After    json.RawMessage `json:"after"`
		} `json:"changes"`
		Unresolved      []configbundle.Placeholder `json:"unresolved"`
		Applied         []string                   `json:"applied"`
		Warnings        []string                   `json:"warnings"`
		SecurityChanges int                        `json:"security_changes"`
	}
	if err := DaemonRequest(http.MethodPost, "/v1/config-bundle/import", in, &response, DaemonOpts{}); err != nil {
		printConfigBundleError(stderr, err)
		return MapDaemonErrorToRC(err)
	}
	if p.JSON {
		raw, _ := json.MarshalIndent(response, "", "  ")
		fmt.Fprintln(out, string(raw))
		return rcOK
	}
	for _, c := range response.Changes {
		tag := ""
		if c.Security {
			tag = " [security]"
		}
		fmt.Fprintf(out, "%s %s%s\n", c.Action, c.Item, tag)
		if c.Action != "unchanged" {
			fmt.Fprintf(out, "  before: %s\n  after:  %s\n", c.Before, c.After)
		}
	}
	fmt.Fprintf(out, "Security changes: %d (permissions, sandbox policy, or launch authority).\n", response.SecurityChanges)
	for _, u := range response.Unresolved {
		fmt.Fprintf(out, "Unresolved %s %s (original: %q): --set %s=value or --keep-paths\n", u.Item, u.Field, u.Original, u.Name)
	}
	for _, warning := range response.Warnings {
		fmt.Fprintf(out, "Warning: %s\n", warning)
	}
	if p.Apply {
		fmt.Fprintf(out, "Applied %d items.\n", len(response.Applied))
	} else {
		fmt.Fprintln(out, "Preview only. Use --apply to write and --replace to overwrite conflicts.")
	}
	return rcOK
}

func printConfigBundleError(out io.Writer, err error) {
	fmt.Fprintln(out, err)
	var de *DaemonError
	if errors.As(err, &de) {
		var detail struct {
			Flags   []configbundle.Flag `json:"flags"`
			Preview struct {
				Unresolved []configbundle.Placeholder `json:"unresolved"`
				Applied    []string                   `json:"applied"`
			} `json:"preview"`
		}
		if json.Unmarshal(de.Raw, &detail) == nil {
			for _, f := range detail.Flags {
				fmt.Fprintf(out, "Flag: %s %s: %s\n", f.Item, f.Field, f.Hint)
			}
			for _, u := range detail.Preview.Unresolved {
				fmt.Fprintf(out, "Unresolved %s %s (original: %q): --set %s=value or --keep-paths\n", u.Item, u.Field, u.Original, u.Name)
			}
			for _, item := range detail.Preview.Applied {
				fmt.Fprintf(out, "Already applied: %s\n", item)
			}
		}
	}
}
