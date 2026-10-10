package federationcmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/configbundle"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/tofutools/tclaude/pkg/federation/bundletransfer"
)

type offerConfigParams struct {
	Peer         string   `pos:"true" help:"Trusted peer (label, name or instance ID)."`
	File         string   `pos:"true" optional:"true" help:"Existing bundle file; omit to select current configuration."`
	Only         []string `long:"only" optional:"true" help:"Include section or section/name (repeatable)."`
	Skip         []string `long:"skip" optional:"true" help:"Exclude section or section/name (repeatable)."`
	AllowFlagged bool     `long:"allow-flagged" help:"Explicitly allow suspected credentials in free text."`
	JSON         bool     `long:"json" help:"Output JSON."`
}

func offerConfigCmd() *cobra.Command {
	return boa.CmdT[offerConfigParams]{Use: "offer-config", Short: "Offer a config bundle to a trusted peer's operator", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *offerConfigParams, _ *cobra.Command, _ []string) {
		os.Exit(runOfferConfig(p, os.Stdout, os.Stderr))
	}}.ToCobra()
}
func runOfferConfig(p *offerConfigParams, out, stderr io.Writer) int {
	in := map[string]any{"peer": p.Peer, "only": p.Only, "skip": p.Skip, "allow_flagged": p.AllowFlagged}
	if p.File != "" {
		f, err := os.Open(p.File)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		defer f.Close()
		raw, err := io.ReadAll(io.LimitReader(f, bundletransfer.Config.MaxBytes+1))
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		if int64(len(raw)) > bundletransfer.Config.MaxBytes {
			fmt.Fprintln(stderr, "config bundle exceeds 16 MiB")
			return 2
		}
		var b configbundle.Bundle
		if err = json.Unmarshal(raw, &b); err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		in["bundle"] = &b
	}
	var response struct {
		Offer struct {
			Descriptor bundletransfer.Descriptor `json:"offer"`
		} `json:"offer"`
		State   string              `json:"state"`
		Flags   []configbundle.Flag `json:"flags"`
		Omitted []string            `json:"omitted"`
	}
	if rc := offerPost(stderr, "/v1/federation/offer-config", in, &response); rc != 0 {
		return rc
	}
	if p.JSON {
		return printJSON(out, response)
	}
	fmt.Fprintf(out, "%s config offer %s to %s (expires %s). Receiver must explicitly import it.\n", response.State, response.Offer.Descriptor.ID, p.Peer, response.Offer.Descriptor.ExpiresAt.Format("2006-01-02 15:04 MST"))
	for _, f := range response.Flags {
		fmt.Fprintf(stderr, "Flag: %s %s: %s\n", f.Item, f.Field, f.Hint)
	}
	for _, v := range response.Omitted {
		fmt.Fprintf(stderr, "Omitted: %s\n", v)
	}
	return 0
}

type offersParams struct {
	Outgoing bool `long:"outgoing" help:"List sent offers instead of received offers."`
	JSON     bool `long:"json" help:"Output JSON."`
}
type offerIDParams struct {
	ID   string `pos:"true" help:"Offer ID from the inbox or offers listing."`
	Peer string `long:"peer" optional:"true" help:"Select source peer when an offer ID is ambiguous."`
}
type offerImportParams struct {
	Cwd         string `long:"cwd" optional:"true" help:"Remap an agent offer's working directory."`
	Worktree    string `long:"worktree" optional:"true" help:"Remap an agent offer's worktree hint."`
	Group       string `long:"group" optional:"true" help:"Choose a receiving group with agents.receive (default offered group)."`
	Name        string `long:"name" optional:"true" help:"Name the new receiving agent."`
	SkipHistory bool   `long:"skip-history" help:"Import only configuration from an agent offer."`

	ID        string   `pos:"true" help:"Offer ID from the inbox or offers listing."`
	Peer      string   `long:"peer" optional:"true" help:"Select source peer when an offer ID is ambiguous."`
	Only      []string `long:"only" optional:"true" help:"Include section or section/name (repeatable)."`
	Skip      []string `long:"skip" optional:"true" help:"Exclude section or section/name (repeatable)."`
	Set       []string `long:"set" optional:"true" help:"Resolve a placeholder with name=value (repeatable)."`
	KeepPaths bool     `long:"keep-paths" help:"Keep recorded absolute paths; --set overrides individual values."`
	Apply     bool     `long:"apply" help:"Apply selected items and finish the offer."`
	Replace   bool     `long:"replace" help:"Explicitly overwrite conflicting items."`
	JSON      bool     `long:"json" help:"Output JSON."`
}

func offersCmd() *cobra.Command {
	return boa.CmdT[offersParams]{Use: "offers", Short: "List, preview, apply or decline bundle offers", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *offersParams, _ *cobra.Command, _ []string) {
		if rc := agent.RequireDaemonOrExit(os.Stderr); rc != 0 {
			os.Exit(rc)
		}
		direction := "in"
		if p.Outgoing {
			direction = "out"
		}
		var rows []struct {
			Offer       bundletransfer.Descriptor `json:"offer"`
			Peer        string                    `json:"peer"`
			Direction   string                    `json:"direction"`
			State       string                    `json:"state"`
			LastError   string                    `json:"last_error,omitempty"`
			ImportAgent string                    `json:"import_agent,omitempty"`
		}
		if err := agent.DaemonGet("/v1/federation/bundle-offers?direction="+direction, &rows); err != nil {
			os.Exit(fail(os.Stderr, err))
		}
		if p.JSON {
			os.Exit(printJSON(os.Stdout, rows))
		}
		for _, r := range rows {
			fmt.Printf("%s  %s  %s  %s  %d bytes  expires %s\n", r.Offer.ID, r.Peer, r.Offer.Type, r.State, r.Offer.Bytes, r.Offer.ExpiresAt.Format("2006-01-02 15:04 MST"))
			if r.ImportAgent != "" {
				fmt.Println("  Imported/reserved agent: " + r.ImportAgent)
			}
			if r.Offer.Group != "" {
				fmt.Println("  Receiving group: " + r.Offer.Group)
			}
			if r.LastError != "" {
				fmt.Println("  " + r.LastError)
			}
		}
		if len(rows) == 0 {
			fmt.Println("no bundle offers")
		}
	}, SubCmds: []*cobra.Command{
		offerContentsCmd(),
		offerDownloadCmd(),
		boa.CmdT[offerImportParams]{Use: "import", Short: "Preview a received config or agent offer; --apply imports it", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *offerImportParams, _ *cobra.Command, _ []string) {
			os.Exit(runOfferImport(p, os.Stdout, os.Stderr))
		}}.ToCobra(),
		boa.CmdT[offerIDParams]{Use: "fetch", Short: "Download and verify a large offer without applying it", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *offerIDParams, _ *cobra.Command, _ []string) {
			var out any
			os.Exit(offerPost(os.Stderr, offerPath(p.ID, p.Peer, "fetch"), nil, &out))
		}}.ToCobra(),
		boa.CmdT[offerIDParams]{Use: "decline", Short: "Decline an offer and delete its local payload", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *offerIDParams, _ *cobra.Command, _ []string) {
			var out any
			os.Exit(offerPost(os.Stderr, offerPath(p.ID, p.Peer, "decline"), nil, &out))
		}}.ToCobra(),
	}}.ToCobra()
}
func offerPath(id, peer, action string) string {
	return "/v1/federation/bundle-offers/" + url.PathEscape(id) + "/" + action + "?peer=" + url.QueryEscape(peer)
}
func runOfferImport(p *offerImportParams, out, stderr io.Writer) int {
	values := map[string]string{}
	for _, arg := range p.Set {
		name, value, ok := strings.Cut(arg, "=")
		if !ok || name == "" || name == "HOME" {
			fmt.Fprintln(stderr, "--set requires name=value; HOME is automatic")
			return 2
		}
		values[name] = value
	}
	in := map[string]any{"only": p.Only, "skip": p.Skip, "values": values, "apply": p.Apply, "replace": p.Replace, "keep_paths": p.KeepPaths, "cwd": p.Cwd, "worktree": p.Worktree, "group": p.Group, "name": p.Name, "skip_history": p.SkipHistory}
	var response map[string]any
	if rc := offerPost(stderr, offerPath(p.ID, p.Peer, "import"), in, &response); rc != 0 {
		return rc
	}
	if p.JSON {
		return printJSON(out, response)
	}
	// The full existing import diff includes security tags and provenance.
	if rc := printJSON(out, response); rc != 0 {
		return rc
	}
	raw, _ := json.Marshal(response["unresolved"])
	var unresolved []configbundle.Placeholder
	_ = json.Unmarshal(raw, &unresolved)
	for _, u := range unresolved {
		fmt.Fprintf(out, "Unresolved %s %s (original: %q): --set %s=value or --keep-paths\n", u.Item, u.Field, u.Original, u.Name)
	}
	if !p.Apply {
		fmt.Fprintln(out, "Preview only. Use --apply to import selected items and finish the offer; conflicts require --replace.")
	}
	return 0
}
func offerPost(stderr io.Writer, path string, in, out any) int {
	if rc := agent.RequireDaemonOrExit(stderr); rc != 0 {
		return rc
	}
	if err := agent.DaemonRequest(http.MethodPost, path, in, out, agent.DaemonOpts{Timeout: 6 * time.Minute}); err != nil {
		var de *agent.DaemonError
		if errors.As(err, &de) {
			var detail map[string]any
			if json.Unmarshal(de.Raw, &detail) == nil {
				_ = printJSON(stderr, detail)
			}
		}
		return fail(stderr, err)
	}
	return 0
}

type offerContentsParams struct {
	ID       string `pos:"true" help:"Received offer ID (fetch it first)."`
	Peer     string `long:"peer" optional:"true" help:"Source peer."`
	Path     string `long:"path" optional:"true" help:"Entry path from the contents listing."`
	Offset   int64  `long:"offset" help:"Byte offset within the entry."`
	MaxBytes int64  `long:"max-bytes" default:"262144" help:"Bounded bytes to read, at most 1048576."`
}

func offerContentsCmd() *cobra.Command {
	return boa.CmdT[offerContentsParams]{Use: "contents", Short: "Inspect a verified offer's entries or bounded text", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *offerContentsParams, _ *cobra.Command, _ []string) {
		os.Exit(runOfferContents(p, os.Stdout, os.Stderr))
	}}.ToCobra()
}
func runOfferContents(p *offerContentsParams, out, stderr io.Writer) int {
	if rc := agent.RequireDaemonOrExit(stderr); rc != 0 {
		return rc
	}
	path := offerPath(p.ID, p.Peer, "contents")
	if p.Path != "" {
		path += "&path=" + url.QueryEscape(p.Path) + fmt.Sprintf("&offset=%d&max_bytes=%d", p.Offset, p.MaxBytes)
	}
	var response any
	if err := agent.DaemonGet(path, &response); err != nil {
		return fail(stderr, err)
	}
	return printJSON(out, response)
}

type offerDownloadParams struct {
	ID   string `pos:"true" help:"Received offer ID (fetch it first)."`
	File string `pos:"true" help:"New output file; existing files are refused."`
	Peer string `long:"peer" optional:"true" help:"Source peer."`
}

func offerDownloadCmd() *cobra.Command {
	return boa.CmdT[offerDownloadParams]{Use: "download", Short: "Save a verified offer without importing it", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *offerDownloadParams, _ *cobra.Command, _ []string) { os.Exit(runOfferDownload(p, os.Stderr)) }}.ToCobra()
}
func runOfferDownload(p *offerDownloadParams, stderr io.Writer) int {
	if rc := agent.RequireDaemonOrExit(stderr); rc != 0 {
		return rc
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	body, err := agent.DaemonStreamGet(ctx, offerPath(p.ID, p.Peer, "download"))
	if err != nil {
		return fail(stderr, err)
	}
	defer body.Close()
	f, err := os.OpenFile(p.File, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fail(stderr, err)
	}
	n, copyErr := io.Copy(f, io.LimitReader(body, bundletransfer.Agent.MaxBytes+1))
	closeErr := f.Close()
	if copyErr == nil {
		copyErr = closeErr
	}
	if copyErr == nil && n > bundletransfer.Agent.MaxBytes {
		copyErr = errors.New("bundle exceeds 256 MiB")
	}
	if copyErr != nil {
		_ = os.Remove(p.File)
		return fail(stderr, copyErr)
	}
	return 0
}
