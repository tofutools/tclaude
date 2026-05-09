package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/table"
	"github.com/tofutools/tclaude/pkg/claude/conv"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/common"
)

// Exit codes for lookup-shaped commands.
const (
	rcOK         = 0
	rcNotFound   = 1
	rcAmbiguous  = 2
	rcInvalidArg = 3
	rcIOFailure  = 4
	rcAuth       = 5
)

// findClaudePID is the parent-process walker used to detect whether the
// invoker has a Claude Code ancestor. Indirected through a variable so
// tests can stub it (the test runner itself is usually a CC descendant).
var findClaudePID = session.FindClaudePID

// ErrAmbiguous is returned by ResolveSelector when the selector matches
// more than one conversation. The caller should surface candidates.
var ErrAmbiguous = errors.New("selector matches multiple conversations")

// errAmbiguous is the package-internal alias kept for the existing CLI
// callers, which compare against this sentinel.
var errAmbiguous = ErrAmbiguous

// Resolved is a minimal handle to a conversation reachable by a selector.
// Exported so the daemon (pkg/claude/agentd) can reuse the resolver.
type Resolved struct {
	ConvID string
	Row    *db.ConvIndexRow // may be nil if conv not yet indexed
}

// resolved is the package-private alias used by existing CLI code.
type resolved = Resolved

// resolveSelector turns a free-form string (UUID, prefix, or current title)
// into a single conv-id. Search is global (any project) by default — agent
// groups are project-agnostic.
//
// `.` and `-` resolve to the current session via $TCLAUDE_SESSION_ID, with
// the parent CC pid file as fallback.
//
// On a miss, we do a global rescan of `~/.claude/projects/*` and try once
// more so brand-new convs (or convs that were just renamed) get picked up
// without requiring a manual `tclaude conv ls -g` first.
func resolveSelector(selector string) (*resolved, []*resolved, error) {
	if selector == "" {
		return nil, nil, fmt.Errorf("selector is required")
	}

	if selector == "." || selector == "-" {
		id, err := currentConvID()
		if err != nil {
			return nil, nil, err
		}
		selector = id
	}

	if r, matches, err := tryResolve(selector); err == nil || errors.Is(err, errAmbiguous) {
		return r, matches, err
	}

	// Cache miss. Refresh the index across all projects and try again.
	refreshAllProjects()
	return tryResolve(selector)
}

// tryResolve runs the cached lookup chain (UUID → prefix → title) without
// touching disk beyond the SQLite cache.
func tryResolve(selector string) (*resolved, []*resolved, error) {
	// 1) full UUID match
	if row, err := db.GetConvIndex(selector); err == nil && row != nil {
		return &resolved{ConvID: row.ConvID, Row: row}, nil, nil
	}

	// 2) prefix match (uniqueness enforced by db.FindConvIndexByPrefix)
	if row, err := db.FindConvIndexByPrefix(selector); err == nil && row != nil {
		return &resolved{ConvID: row.ConvID, Row: row}, nil, nil
	}

	// 3) match by current display title (custom > summary > first prompt)
	rows, err := db.ListAllConvIndex()
	if err != nil {
		return nil, nil, err
	}
	var matches []*resolved
	for _, r := range rows {
		if displayTitle(r) == selector {
			matches = append(matches, &resolved{ConvID: r.ConvID, Row: r})
		}
	}
	if len(matches) == 1 {
		return matches[0], nil, nil
	}
	if len(matches) > 1 {
		return nil, matches, errAmbiguous
	}
	return nil, nil, fmt.Errorf("no conversation matches %q", selector)
}

// refreshAllProjects walks ~/.claude/projects/* and runs the same
// LoadSessionsIndex pass that `conv ls -g` does, picking up new convs
// and applying the per-file mtime check to refresh stale ones. Errors
// are intentionally swallowed — the caller will surface a clearer
// "no conversation matches" if the second tryResolve still fails.
func refreshAllProjects() {
	projectsDir := conv.ClaudeProjectsDir()
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		_, _ = conv.LoadSessionsIndex(filepath.Join(projectsDir, e.Name()))
	}
}

// ResolveSelector is the daemon-friendly entry point that mirrors the
// internal resolveSelector. Returns (resolved, candidates, err) where err
// may be ErrAmbiguous with candidates populated.
func ResolveSelector(selector string) (*Resolved, []*Resolved, error) {
	return resolveSelector(selector)
}

// CurrentConvID returns the conv-id of the conversation invoking the
// caller, expanded to a full UUID via the DB if necessary. Exported for
// callers outside this package.
func CurrentConvID() (string, error) {
	return currentConvID()
}

// DisplayTitle is the exported alias of displayTitle so other packages
// can render agent names consistently.
func DisplayTitle(r *db.ConvIndexRow) string {
	return displayTitle(r)
}

// FreshConvRow refreshes a single conv's index row before returning it.
func FreshConvRow(convID string) *db.ConvIndexRow {
	return freshConvRow(convID)
}

// ShortID truncates a conv-id to the first 8 hex chars.
func ShortID(convID string) string {
	return short(convID)
}

// AliasFor returns the recorded alias for (groupID, convID), or the conv's
// display title, or "".
func AliasFor(groupID int64, convID string) string {
	return aliasFor(groupID, convID)
}

// displayTitle returns the title we treat as the agent's name: custom title
// if set, else summary, else first prompt.
func displayTitle(r *db.ConvIndexRow) string {
	if r.CustomTitle != "" {
		return r.CustomTitle
	}
	if r.Summary != "" {
		return r.Summary
	}
	return r.FirstPrompt
}

// freshConvRow returns the conv_index row for convID, rescanning the
// underlying .jsonl when its mtime is newer than the cached row. The actual
// freshness check lives in conv.RefreshConvIndexEntry — same pattern that
// LoadSessionsIndex uses on every `conv ls`.
func freshConvRow(convID string) *db.ConvIndexRow {
	return conv.RefreshConvIndexEntry(convID)
}

// currentConvID returns the conv-id of the conversation invoking us. We
// prefer Claude Code's per-pid session file (tracks `/clear` and `/resume`)
// and fall back to the env var that tclaude sets at session launch.
//
// $TCLAUDE_SESSION_ID is sometimes set to the 8-char prefix rather than
// the full UUID, so we expand prefixes via the DB before returning, so
// downstream callers can rely on getting the canonical full ID.
func currentConvID() (string, error) {
	if id := readCCSessionID(); id != "" {
		return id, nil
	}
	if id := os.Getenv("TCLAUDE_SESSION_ID"); id != "" {
		// Already a full UUID? Take it.
		if len(id) == 36 {
			return id, nil
		}
		// Prefix → full UUID via the DB.
		if row, err := db.FindConvIndexByPrefix(id); err == nil && row != nil {
			return row.ConvID, nil
		}
		return id, nil
	}
	return "", fmt.Errorf("could not detect current conversation; pass an explicit conv ID")
}

// readCCSessionID walks up to the parent CC pid and reads
// ~/.claude/sessions/<pid>.json for its `sessionId`.
func readCCSessionID() string {
	pid := session.FindClaudePID()
	if pid == 0 {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	path := filepath.Join(home, ".claude", "sessions", fmt.Sprintf("%d.json", pid))
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return ""
	}
	id, _ := m["sessionId"].(string)
	return id
}

// printAmbiguous writes the matching candidates so the user can disambiguate.
func printAmbiguous(out io.Writer, selector string, matches []*resolved) {
	fmt.Fprintf(out, "Error: selector %q matches %d conversations:\n", selector, len(matches))
	for _, m := range matches {
		title := ""
		if m.Row != nil {
			title = displayTitle(m.Row)
		}
		fmt.Fprintf(out, "  %s  %s\n", m.ConvID[:8], title)
	}
	fmt.Fprintf(out, "Disambiguate by ID prefix.\n")
}

// --- whoami ---

type whoamiParams struct{}

func whoamiCmd() *cobra.Command {
	return boa.CmdT[whoamiParams]{
		Use:         "whoami",
		Short:       "Print the current conversation's ID and display name",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(_ *whoamiParams, _ *cobra.Command, _ []string) {
			os.Exit(runWhoami(os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

// HumanIdentity is what whoami prints when the invocation is not from
// inside a Claude Code session — i.e. the coordinating human running
// tclaude directly from a shell.
const HumanIdentity = "<human>"

func runWhoami(stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	return runWhoamiDaemon(stdout, stderr)
}

func runWhoamiDaemon(stdout, stderr io.Writer) int {
	var resp struct {
		IsHuman bool   `json:"is_human"`
		ConvID  string `json:"conv_id"`
		Title   string `json:"title"`
	}
	if err := DaemonGet("/v1/whoami", &resp); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if resp.IsHuman {
		fmt.Fprintln(stdout, HumanIdentity)
		return rcOK
	}
	title := resp.Title
	if title == "" {
		title = "(unnamed)"
	}
	fmt.Fprintf(stdout, "%s\t%s\n", resp.ConvID, title)
	return rcOK
}

func runWhoamiDirect(stdout, stderr io.Writer) int {
	id, err := currentConvID()
	if err != nil {
		// No conv-id resolvable. If there's no `claude` ancestor in the
		// process tree, the invoker is the human — say so.
		if findClaudePID() == 0 {
			fmt.Fprintln(stdout, HumanIdentity)
			return rcOK
		}
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcNotFound
	}
	row := freshConvRow(id)
	title := "(unnamed)"
	if row != nil {
		if t := displayTitle(row); t != "" {
			title = t
		}
	}
	fmt.Fprintf(stdout, "%s\t%s\n", id, title)
	return rcOK
}

// --- lookup ---

type lookupParams struct {
	Selector string `pos:"true" help:"Conversation: UUID, prefix, or current title"`
}

func lookupCmd() *cobra.Command {
	return boa.CmdT[lookupParams]{
		Use:         "lookup",
		Short:       "Resolve an agent name (or ID prefix) to a full conversation ID",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *lookupParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Selector).SetAlternativesFunc(completeConvSelectors)
			return nil
		},
		RunFunc: func(p *lookupParams, _ *cobra.Command, _ []string) {
			os.Exit(runLookup(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runLookup(p *lookupParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	return runLookupDaemon(p, stdout, stderr)
}

func runLookupDaemon(p *lookupParams, stdout, stderr io.Writer) int {
	var resp struct {
		ConvID string `json:"conv_id"`
	}
	err := DaemonGet("/v1/lookup?selector="+url.QueryEscape(p.Selector), &resp)
	if de, ok := err.(*DaemonError); ok && de.Code == "ambiguous" {
		// The daemon returns 409 with a candidates list. Surface the
		// candidates the same way the direct path does.
		fmt.Fprintf(stderr, "%s\n", de.Msg)
		return rcAmbiguous
	}
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	fmt.Fprintln(stdout, resp.ConvID)
	return rcOK
}

func runLookupDirect(p *lookupParams, stdout, stderr io.Writer) int {
	r, matches, err := resolveSelector(p.Selector)
	if errors.Is(err, errAmbiguous) {
		printAmbiguous(stderr, p.Selector, matches)
		return rcAmbiguous
	}
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcNotFound
	}
	fmt.Fprintln(stdout, r.ConvID)
	return rcOK
}


// --- ls (peers in my groups) ---

type lsParams struct {
	JSON bool `long:"json" help:"Output JSON"`
}

func lsCmd() *cobra.Command {
	return boa.CmdT[lsParams]{
		Use:         "ls",
		Short:       "List agents reachable to me (members of my groups)",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *lsParams, _ *cobra.Command, _ []string) {
			os.Exit(runLs(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

type peerEntry struct {
	ConvID string   `json:"conv_id"`
	Title  string   `json:"title"`
	Alias  string   `json:"alias,omitempty"`
	Role   string   `json:"role,omitempty"`
	Descr  string   `json:"descr,omitempty"`
	Online bool     `json:"online"`
	Groups []string `json:"groups"`
}

func runLs(p *lsParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	return runLsDaemon(p, stdout, stderr)
}

func runLsDaemon(p *lsParams, stdout, stderr io.Writer) int {
	var peers []*peerEntry
	if err := DaemonGet("/v1/peers", &peers); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	return renderPeers(p, peers, stdout)
}

func renderPeers(p *lsParams, peers []*peerEntry, stdout io.Writer) int {
	if p.JSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(peers); err != nil {
			return rcIOFailure
		}
		return rcOK
	}
	if len(peers) == 0 {
		fmt.Fprintln(stdout, "(no peers — you are not in any groups, or your groups have no other members)")
		return rcOK
	}
	tbl := table.New(
		table.Column{Header: "", Width: 1},
		table.Column{Header: "ID", Width: 8},
		table.Column{Header: "ALIAS", MinWidth: 8, Weight: 0.8, Truncate: true},
		table.Column{Header: "ROLE", MinWidth: 6, Weight: 0.4, Truncate: true},
		table.Column{Header: "GROUPS", MinWidth: 8, Weight: 0.6, Truncate: true},
		table.Column{Header: "DESCR", MinWidth: 10, Weight: 1.2, Truncate: true},
	)
	tbl.SetTerminalWidth(table.GetTerminalWidth())
	for _, pe := range peers {
		alias := pe.Alias
		if alias == "" {
			alias = pe.Title
		}
		tbl.AddRow(table.Row{Cells: []string{
			onlineMark(pe.Online),
			short(pe.ConvID),
			alias,
			pe.Role,
			strings.Join(pe.Groups, ","),
			pe.Descr,
		}})
	}
	fmt.Fprintln(stdout, tbl.Render())
	return rcOK
}


