package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

// sudoCmd is `tclaude agent sudo …` — time-bounded permission
// elevations modeled on Unix sudo and GCP PAM. An agent calls
// `request <slug>... --duration` to ask the human for a bundle of
// permission slugs to be active for a bounded window. The request
// always pops a human-approval popup; on approve, the slugs join
// the agent's effective permission set (alongside defaults +
// per-conv grants) until the window closes.
//
// `ls` and `revoke` are the management surface — humans can see
// every active grant across the daemon and pull individual ones
// (or all of them) at any time.
func sudoCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:   "sudo",
		Short: "Time-bounded permission elevations (request, list, revoke)",
		Long: "Request a bundle of permission slugs for a bounded duration. The request " +
			"always triggers a human-approval popup; on approve, the slugs join the agent's " +
			"effective permission set until the window expires (or a human revokes early). " +
			"Permanent escalation slugs (permissions.grant / permissions.revoke) are " +
			"blocklisted from sudo by design — the audit trail of a time-bounded grant is " +
			"the whole point of the model.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			sudoRequestCmd(),
			sudoLsCmd(),
			sudoRevokeCmd(),
		},
	}.ToCobra()
}

// --- shared types ---

type sudoGrantJSON struct {
	ID               int64  `json:"id"`
	ConvID           string `json:"conv_id"`
	ConvTitle        string `json:"conv_title,omitempty"`
	Slug             string `json:"slug"`
	GrantedAt        string `json:"granted_at"`
	ExpiresAt        string `json:"expires_at"`
	GrantedBy        string `json:"granted_by"`
	Reason           string `json:"reason,omitempty"`
	RemainingSeconds int64  `json:"remaining_seconds,omitempty"`
}

// --- sudo request ---

type sudoRequestParams struct {
	Slugs    []string `pos:"true" help:"One or more permission slugs to elevate (e.g. groups.spawn member.add)"`
	Duration string   `long:"duration" short:"d" help:"How long the elevation lasts (e.g. 5m, 1h). Capped at 1h. Default: 5m." default:""`
	Reason   string   `long:"reason" short:"r" help:"Optional justification surfaced in the popup + audit trail" default:""`
	JSON     bool     `long:"json" help:"Output JSON"`
}

func sudoRequestCmd() *cobra.Command {
	return boa.CmdT[sudoRequestParams]{
		Use:         "request <slug>...",
		Short:       "Ask the human to elevate one or more slugs for a bounded duration",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *sudoRequestParams, _ *cobra.Command, _ []string) {
			os.Exit(runSudoRequest(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runSudoRequest(p *sudoRequestParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	if len(p.Slugs) == 0 {
		fmt.Fprintln(stderr, "Error: at least one slug is required")
		return rcInvalidArg
	}
	body := map[string]any{
		"slugs":    p.Slugs,
		"duration": p.Duration,
		"reason":   p.Reason,
	}
	var resp struct {
		Grants    []sudoGrantJSON `json:"grants"`
		ExpiresAt string          `json:"expires_at"`
		ConvID    string          `json:"conv_id"`
	}
	if err := DaemonPost("/v1/sudo", body, &resp); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if p.JSON {
		return writeJSONIndentSudo(stdout, resp)
	}
	if len(resp.Grants) == 0 {
		fmt.Fprintln(stderr, "No grants landed.")
		return rcIOFailure
	}
	exp, _ := time.Parse(time.RFC3339Nano, resp.ExpiresAt)
	if exp.IsZero() {
		fmt.Fprintln(stdout, "Sudo approved.")
	} else {
		fmt.Fprintf(stdout, "Sudo approved — expires at %s (%s from now).\n",
			exp.Format(time.RFC3339), time.Until(exp).Round(time.Second))
	}
	for _, g := range resp.Grants {
		if g.ID == 0 {
			fmt.Fprintf(stdout, "  ✗ %-30s %s\n", g.Slug, g.Reason)
			continue
		}
		fmt.Fprintf(stdout, "  ✓ #%-5d %-30s\n", g.ID, g.Slug)
	}
	return rcOK
}

// --- sudo ls ---

type sudoLsParams struct {
	All  bool `long:"all" help:"List active grants across all conversations (human-only)"`
	JSON bool `long:"json" help:"Output JSON"`
}

func sudoLsCmd() *cobra.Command {
	return boa.CmdT[sudoLsParams]{
		Use:         "ls",
		Short:       "List active sudo grants (self by default, --all for everyone)",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *sudoLsParams, _ *cobra.Command, _ []string) {
			os.Exit(runSudoLs(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runSudoLs(p *sudoLsParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	path := "/v1/sudo"
	if p.All {
		path += "?all=1"
	}
	var rows []sudoGrantJSON
	if err := DaemonGet(path, &rows); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if p.JSON {
		return writeJSONIndentSudo(stdout, rows)
	}
	if len(rows) == 0 {
		fmt.Fprintln(stdout, "No active sudo grants.")
		return rcOK
	}
	// Group by conv-id for readable output: one block per agent
	// listing its currently-elevated slugs.
	byConv := map[string][]sudoGrantJSON{}
	convOrder := []string{}
	for _, g := range rows {
		if _, ok := byConv[g.ConvID]; !ok {
			convOrder = append(convOrder, g.ConvID)
		}
		byConv[g.ConvID] = append(byConv[g.ConvID], g)
	}
	sort.Strings(convOrder)
	for _, conv := range convOrder {
		gs := byConv[conv]
		title := gs[0].ConvTitle
		if title == "" {
			title = "(no title)"
		}
		fmt.Fprintf(stdout, "%s  %s\n", short(conv), title)
		for _, g := range gs {
			remain := time.Duration(g.RemainingSeconds) * time.Second
			fmt.Fprintf(stdout, "  #%-5d %-30s %s remaining   %q\n",
				g.ID, g.Slug, remain.Round(time.Second), g.Reason)
		}
	}
	return rcOK
}

// --- sudo revoke ---

type sudoRevokeParams struct {
	ID    int64  `pos:"true" optional:"true" help:"Grant ID to revoke (from sudo ls). Mutually exclusive with --conv / --all."`
	Conv  string `long:"conv" short:"c" help:"Revoke every active grant for one conv (selector: alias / prefix / UUID)"`
	All   bool   `long:"all" help:"Revoke every active grant daemon-wide (use with care)"`
	Force bool   `long:"force" short:"f" help:"Skip the confirmation prompt for --all"`
}

func sudoRevokeCmd() *cobra.Command {
	return boa.CmdT[sudoRevokeParams]{
		Use:         "revoke [<id>] [--conv <selector> | --all]",
		Short:       "Revoke sudo grants (one by id, all for one conv, or every active)",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *sudoRevokeParams, _ *cobra.Command, _ []string) {
			os.Exit(runSudoRevoke(p, os.Stdin, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runSudoRevoke(p *sudoRevokeParams, stdin io.Reader, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	// Exactly one of {id, --conv, --all} must be set.
	modes := 0
	if p.ID != 0 {
		modes++
	}
	if p.Conv != "" {
		modes++
	}
	if p.All {
		modes++
	}
	if modes != 1 {
		fmt.Fprintln(stderr, "Error: pass exactly one of <id>, --conv <selector>, or --all")
		return rcInvalidArg
	}

	switch {
	case p.ID != 0:
		path := "/v1/sudo/" + strconv.FormatInt(p.ID, 10)
		var resp struct {
			Revoked int64 `json:"revoked"`
			ID      int64 `json:"id"`
		}
		if err := DaemonRequest(http.MethodDelete, path, nil, &resp, DaemonOpts{}); err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return MapDaemonErrorToRC(err)
		}
		fmt.Fprintf(stdout, "Revoked grant #%d.\n", resp.ID)
		return rcOK
	case p.Conv != "":
		path := "/v1/sudo?conv=" + url.QueryEscape(p.Conv)
		var resp struct {
			Revoked int64  `json:"revoked"`
			ConvID  string `json:"conv_id"`
		}
		if err := DaemonRequest(http.MethodDelete, path, nil, &resp, DaemonOpts{}); err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return MapDaemonErrorToRC(err)
		}
		fmt.Fprintf(stdout, "Revoked %d grant(s) for %s.\n", resp.Revoked, short(resp.ConvID))
		return rcOK
	case p.All:
		// Confirm before nuking unless --force. The bulk path is the
		// only one big enough to wreck someone's day; per-id and
		// per-conv are surgical.
		if !p.Force {
			fmt.Fprint(stdout, "Revoke EVERY active sudo grant daemon-wide? [y/N] ")
			var ans string
			_, _ = fmt.Fscanln(stdin, &ans)
			if !strings.EqualFold(strings.TrimSpace(ans), "y") {
				fmt.Fprintln(stdout, "Cancelled.")
				return rcOK
			}
		}
		path := "/v1/sudo?all=1"
		var resp struct {
			Revoked int64  `json:"revoked"`
			Scope   string `json:"scope"`
		}
		if err := DaemonRequest(http.MethodDelete, path, nil, &resp, DaemonOpts{}); err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return MapDaemonErrorToRC(err)
		}
		fmt.Fprintf(stdout, "Revoked %d active grant(s) (scope: %s).\n", resp.Revoked, resp.Scope)
		return rcOK
	}
	return rcInvalidArg
}

func writeJSONIndentSudo(w io.Writer, v any) int {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return rcIOFailure
	}
	return rcOK
}
