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

func writeJSONIndentAlias(w io.Writer, v any) int {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return rcIOFailure
	}
	return rcOK
}

// aliasCmd is `tclaude agent alias …` — global head-alias layer.
// Stable handles that resolve to the live head of a conv-succession
// chain via db.ResolveLatestConv. Distinct from an agent's name (its
// conversation title): a head alias is a deliberately-stable handle
// that survives reincarnation, useful where a fixed global name is
// more memorable than whatever the chain head is currently titled.
//
// Read verbs (`ls`, `get`) are open; mutating verbs (`set`, `rm`)
// are human-only at the daemon today (no slug yet — agents who need
// to write here can ladder up via a future slug).
func aliasCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:   "alias",
		Short: "Manage global head aliases (handle → live conv head)",
		Long: "Set, list, and remove daemon-wide head aliases. A head alias is a stable " +
			"handle (e.g. \"po\", \"ceo\") that always resolves to the live head of a conv " +
			"chain — survives arbitrary reincarnation depth without re-pointing the row. " +
			"Distinct from an agent's name (its conversation title): a head alias is a " +
			"fixed handle the human curates. Resolved by `tclaude agent message <handle>` " +
			"and friends.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			aliasSetCmd(),
			aliasRmCmd(),
			aliasLsCmd(),
			aliasGetCmd(),
		},
	}.ToCobra()
}

// --- shared types ---

type headAliasJSON struct {
	Handle    string `json:"handle"`
	Anchor    string `json:"anchor_conv_id"`
	Head      string `json:"head_conv_id"`
	HeadTitle string `json:"head_title,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
	ByConv    string `json:"by_conv,omitempty"`
}

// --- alias set ---

type aliasSetParams struct {
	Handle string `pos:"true" help:"The handle to set (lower-cased; cannot look like a UUID, start with 'group:', or be . / -)"`
	Conv   string `pos:"true" help:"Conv selector (UUID / prefix / title) to anchor the handle to"`
}

func aliasSetCmd() *cobra.Command {
	return boa.CmdT[aliasSetParams]{
		Use:         "set <handle> <conv>",
		Short:       "Anchor a handle to a conv (re-pointable)",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *aliasSetParams, _ *cobra.Command, _ []string) {
			os.Exit(runAliasSet(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runAliasSet(p *aliasSetParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	body := map[string]string{"handle": p.Handle, "conv": p.Conv}
	var resp headAliasJSON
	if err := DaemonPost("/v1/agent/aliases", body, &resp); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	headLabel := resp.HeadTitle
	if headLabel == "" {
		headLabel = "(no title)"
	}
	if resp.Head != resp.Anchor {
		fmt.Fprintf(stdout, "Set head alias %q → anchor %s (current head %s, %q via succession chain)\n",
			resp.Handle, short(resp.Anchor), short(resp.Head), headLabel)
	} else {
		fmt.Fprintf(stdout, "Set head alias %q → %s (%q)\n",
			resp.Handle, short(resp.Anchor), headLabel)
	}
	return rcOK
}

// --- alias rm ---

type aliasRmParams struct {
	Handle string `pos:"true" help:"The handle to remove"`
}

func aliasRmCmd() *cobra.Command {
	return boa.CmdT[aliasRmParams]{
		Use:         "rm <handle>",
		Short:       "Remove a head alias",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *aliasRmParams, _ *cobra.Command, _ []string) {
			os.Exit(runAliasRm(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runAliasRm(p *aliasRmParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	path := "/v1/agent/aliases/" + url.PathEscape(strings.ToLower(strings.TrimSpace(p.Handle)))
	if err := DaemonRequest(http.MethodDelete, path, nil, nil, DaemonOpts{}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	fmt.Fprintf(stdout, "Removed head alias %q\n", strings.ToLower(strings.TrimSpace(p.Handle)))
	return rcOK
}

// --- alias ls ---

type aliasLsParams struct {
	JSON bool `long:"json" help:"Output JSON"`
}

func aliasLsCmd() *cobra.Command {
	return boa.CmdT[aliasLsParams]{
		Use:         "ls",
		Short:       "List every head alias and its current head conv",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *aliasLsParams, _ *cobra.Command, _ []string) {
			os.Exit(runAliasLs(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runAliasLs(p *aliasLsParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var rows []headAliasJSON
	if err := DaemonGet("/v1/agent/aliases", &rows); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if len(rows) == 0 {
		fmt.Fprintln(stdout, "No head aliases set. Add one with: tclaude agent alias set <handle> <conv>")
		return rcOK
	}
	if p.JSON {
		return writeJSONIndentAlias(stdout, rows)
	}
	for _, h := range rows {
		title := h.HeadTitle
		if title == "" {
			title = "(no title)"
		}
		if h.Head != h.Anchor {
			fmt.Fprintf(stdout, "  %-20s anchor:%s  →  head:%s  %q\n",
				h.Handle, short(h.Anchor), short(h.Head), title)
		} else {
			fmt.Fprintf(stdout, "  %-20s %s  %q\n", h.Handle, short(h.Anchor), title)
		}
	}
	return rcOK
}

// --- alias get ---

type aliasGetParams struct {
	Handle string `pos:"true" help:"The handle to look up"`
	JSON   bool   `long:"json" help:"Output JSON"`
}

func aliasGetCmd() *cobra.Command {
	return boa.CmdT[aliasGetParams]{
		Use:         "get <handle>",
		Short:       "Resolve one head alias to its current head conv-id",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *aliasGetParams, _ *cobra.Command, _ []string) {
			os.Exit(runAliasGet(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runAliasGet(p *aliasGetParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	path := "/v1/agent/aliases/" + url.PathEscape(strings.ToLower(strings.TrimSpace(p.Handle)))
	var resp headAliasJSON
	if err := DaemonGet(path, &resp); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if p.JSON {
		return writeJSONIndentAlias(stdout, resp)
	}
	title := resp.HeadTitle
	if title == "" {
		title = "(no title)"
	}
	fmt.Fprintf(stdout, "Handle:        %s\n", resp.Handle)
	fmt.Fprintf(stdout, "Anchor conv:   %s\n", resp.Anchor)
	fmt.Fprintf(stdout, "Current head:  %s  %q\n", resp.Head, title)
	if resp.CreatedAt != "" {
		fmt.Fprintf(stdout, "Created at:    %s\n", resp.CreatedAt)
	}
	return rcOK
}
