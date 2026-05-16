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
	"github.com/tofutools/tclaude/pkg/claude/common/convindex"
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
//
// Succession redirect: after a successful resolve, if the conv-id has a
// recorded successor (because the original was reincarnated), the
// resolver returns the latest conv in the chain. This lets stale
// references from CLI / cron / handlers transparently target the live
// conv without callers having to thread `db.ResolveLatestConv` through
// every code path.
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
		return redirectResolvedToLatest(r), matches, err
	}

	// Cache miss. Refresh the index across all projects and try again.
	refreshAllProjects()
	r, matches, err := tryResolve(selector)
	return redirectResolvedToLatest(r), matches, err
}

// redirectResolvedToLatest follows the agent_conv_succession chain
// from r.ConvID forward and returns a resolved pointing at the live
// conv. Returns r unchanged when there's no chain edge (the common
// case) or when the chain head is the same conv-id we already have.
//
// On a successful redirect we re-fetch the conv_index row for the new
// conv so display titles / metadata reflect the live conv. If that
// fetch fails (e.g. the new conv isn't indexed yet), we keep the old
// row but stamp the new ConvID — the caller can still send messages /
// look up sessions; only the title might be momentarily stale.
func redirectResolvedToLatest(r *resolved) *resolved {
	if r == nil {
		return nil
	}
	latest := db.ResolveLatestConv(r.ConvID)
	if latest == r.ConvID {
		return r
	}
	out := &resolved{ConvID: latest, Row: r.Row}
	if newRow, err := db.GetConvIndex(latest); err == nil && newRow != nil {
		out.Row = newRow
	}
	return out
}

// tryResolve runs the cached lookup chain (UUID → prefix → title →
// group-member alias/prefix) without touching disk beyond the SQLite
// cache.
func tryResolve(selector string) (*resolved, []*resolved, error) {
	// 0) global head-alias lookup. A handle (e.g. "po", "ceo")
	//    resolves to the current head of its conv chain — survives
	//    arbitrary reincarnation depth via ResolveLatestConv. Handles
	//    are validated to never shadow UUIDs / "group:" / "."/"-", so
	//    this branch can take precedence without ambiguity.
	if head, err := db.ResolveHeadAlias(selector); err == nil && head != "" {
		row, _ := db.GetConvIndex(head)
		return &resolved{ConvID: head, Row: row}, nil, nil
	}

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

	// 4) fallback to agent_group_members so per-group aliases (e.g.
	//    `tclaude agent message reviewer "..."`) work, and so freshly-
	//    spawned convs are findable before their .jsonl gets scanned
	//    into conv_index.
	//
	//    Distinct by conv_id: an agent can be a member of multiple
	//    groups under different aliases, but it's still one conv. If
	//    the same alias points at different convs across groups,
	//    that's genuinely ambiguous and we surface it.
	if mem, err := db.FindAgentMembersBySelector(selector); err == nil && len(mem) > 0 {
		seen := map[string]bool{}
		var unique []*resolved
		for _, m := range mem {
			if seen[m.ConvID] {
				continue
			}
			seen[m.ConvID] = true
			// Prefer to attach the conv-index row when it exists so
			// downstream renderers can show titles.
			row, _ := db.GetConvIndex(m.ConvID)
			unique = append(unique, &resolved{ConvID: m.ConvID, Row: row})
		}
		if len(unique) == 1 {
			return unique[0], nil, nil
		}
		if len(unique) > 1 {
			return nil, unique, errAmbiguous
		}
	}

	// 5) succession-chain fallback: a raw conv-id that's been pruned
	//    from conv_index and isn't a member of any group can still be
	//    resolvable if it has a recorded successor. Walking the chain
	//    forward from the input picks up "Bob's old UUID got reincarnated
	//    into Bob-r-1 which IS indexed" cases. The chain row alone is
	//    enough to declare the input a known historical id; we accept
	//    the walked head as the resolution result.
	if walked := db.ResolveLatestConv(selector); walked != selector {
		row, _ := db.GetConvIndex(walked)
		return &resolved{ConvID: walked, Row: row}, nil, nil
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
// can render an agent's bare *name* consistently (custom title, else
// summary, else first prompt). This is the agent-name concept — used
// for identity, group rosters and reincarnate/clone name prefixes. For
// the "[title]: prompt" rendering that conversation-listing surfaces
// show (`conv ls`, the dashboard's plain-conversation list), use
// FreshConvTitle instead.
func DisplayTitle(r *db.ConvIndexRow) string {
	return displayTitle(r)
}

// FreshConvRow refreshes a single conv's index row before returning it.
func FreshConvRow(convID string) *db.ConvIndexRow {
	return freshConvRow(convID)
}

// FreshConvRowAt is FreshConvRow with a cwd hint. When the conv has no
// conv_index row yet (freshly-spawned by reincarnate / clone, never
// touched by `tclaude conv ls` or the watch model), we derive the
// .jsonl path from cwd + convID and scan it directly so callers can
// still read CustomTitle. Returns nil if neither the cache nor the
// derived path produced data.
//
// Why this exists: reincarnate's prevTitle lookup used to call
// FreshConvRow on the parent and silently get nil when the parent's
// conv-id had never been indexed — producing names like `reincarnate-1`
// instead of `<parent-name>-reincarnate-N`. The cwd from the parent's
// session row is the missing piece.
func FreshConvRowAt(convID, cwd string) *db.ConvIndexRow {
	if row := freshConvRow(convID); row != nil {
		return row
	}
	if cwd == "" {
		return nil
	}
	projectPath := conv.GetClaudeProjectPath(cwd)
	if projectPath == "" {
		return nil
	}
	jsonlPath := filepath.Join(projectPath, convID+".jsonl")
	conv.ScanAndUpsertFile(jsonlPath)
	return freshConvRow(convID)
}

// FreshConvRowResolved is FreshConvRow with the cwd hint sourced
// automatically from the most-recent session row for this conv. It's
// the right choice for callers that want the .jsonl-scan fallback but
// don't already have a cwd in scope (the dashboard is the prototypical
// example).
//
// Without this fallback, fresh-spawned reincarnations / clones display
// as "(unknown)" in the dashboard until the first `tclaude conv ls`
// indexes them — same root cause as the prevTitle="" reincarnate-prefix
// bug, just visible in a different surface.
func FreshConvRowResolved(convID string) *db.ConvIndexRow {
	if row := freshConvRow(convID); row != nil {
		return row
	}
	rows, err := db.FindSessionsByConvID(convID)
	if err != nil || len(rows) == 0 {
		return nil
	}
	return FreshConvRowAt(convID, rows[0].Cwd)
}

// UnknownTitle is the placeholder a listing surface shows for a conv
// whose display name can't be resolved at all (no conv_index row, no
// session row to derive a cwd from, deleted .jsonl).
const UnknownTitle = "(unknown)"

// FreshTitle resolves convID to the display name shown on listing
// surfaces (`agent ls`, `groups members`, the dashboard). It refreshes
// the conv_index row from the underlying .jsonl first via
// FreshConvRowResolved, so a conv that was renamed or freshly spawned
// since the last index pass shows its real name rather than a stale
// title or "(unknown)".
//
// This is the common "name an agent for display" helper — prefer it
// over a bare db.GetConvIndex in any handler that renders agent names,
// so every surface picks up source changes uniformly.
func FreshTitle(convID string) string {
	if row := FreshConvRowResolved(convID); row != nil {
		if t := displayTitle(row); t != "" {
			return t
		}
	}
	return UnknownTitle
}

// FreshConvTitle resolves convID to the canonical "[title]: prompt"
// display string — the exact rendering `conv ls` / `conv ls -w` show,
// via the single source of truth convindex.FormatConvTitle —
// refreshing the conv_index row from the .jsonl first (like FreshTitle).
//
// Use this for *plain conversations* (non-agents): the web dashboard's
// promotion list renders through it so a plain conv's title matches the
// CLI instead of leaking a raw, uncleaned first prompt (issue #91).
// FreshTitle (the bare agent name) stays in use for agents — identity,
// group rosters, alias derivation, reincarnate/clone name prefixes.
func FreshConvTitle(convID string) string {
	if row := FreshConvRowResolved(convID); row != nil {
		if t := convindex.FormatConvTitle(row.CustomTitle, row.Summary, row.FirstPrompt); t != "" {
			return t
		}
	}
	return UnknownTitle
}

// FreshBranch resolves convID to the git branch the agent is working
// on *now*. It returns ResolveLocation's CurrentBranch — the branch of
// the dir the agent last edited a file in, which tracks the agent as
// it moves between sub-repos. When the agent hasn't edited anything
// yet (or the hook predates worktree tracking) this falls back to the
// launch dir's branch, so the value degrades sensibly rather than
// going blank. Returns "" when no branch can be determined.
//
// Prefer ResolveLocation directly in any handler that also needs the
// startup branch or the directories; FreshBranch is the convenience
// shorthand for callers that only want the one value.
func FreshBranch(convID string) string {
	return ResolveLocation(convID).CurrentBranch
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
//
// Final fallback: ask the daemon via /v1/whoami. The daemon resolves
// identity from the Unix socket's peer credentials (host PID), which
// works even when our process tree is hidden by a sandbox PID
// namespace (e.g. bwrap with --unshare-pid). Cheap roundtrip — the
// daemon is local and the lookup is one query.
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
	if id := whoamiViaDaemon(); id != "" {
		return id, nil
	}
	return "", fmt.Errorf("could not detect current conversation; pass an explicit conv ID")
}

// whoamiViaDaemon asks the running agentd "who am I?" via peer-cred.
// Returns "" if the daemon isn't running, or the caller is a human
// (no Claude ancestor from the daemon's view), or any other failure.
// Side-effect-free for the not-running case so command-line tools
// don't error on hosts without agentd.
//
// Indirected through a variable so tests can stub it (the test runner
// itself shouldn't reach a real daemon).
var whoamiViaDaemon = func() string {
	if SocketPath() == "" {
		return ""
	}
	var resp struct {
		IsHuman bool   `json:"is_human"`
		ConvID  string `json:"conv_id"`
	}
	if err := DaemonGet("/v1/whoami", &resp); err != nil {
		return ""
	}
	if resp.IsHuman {
		return ""
	}
	return resp.ConvID
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
	State string `long:"state" optional:"true" help:"Filter: online | offline"`
	JSON  bool   `long:"json" help:"Output JSON"`
}

func lsCmd() *cobra.Command {
	return boa.CmdT[lsParams]{
		Use:         "ls",
		Short:       "List agents reachable to me (members of my groups)",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *lsParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.State).SetAlternativesFunc(completeStateFilterValues)
			return nil
		},
		RunFunc: func(p *lsParams, _ *cobra.Command, _ []string) {
			os.Exit(runLs(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

type peerEntry struct {
	ConvID string `json:"conv_id"`
	Title  string `json:"title"`
	Alias  string `json:"alias,omitempty"`
	Role   string `json:"role,omitempty"`
	Descr  string `json:"descr,omitempty"`
	// Branch is the git branch / worktree the agent is working on, as
	// recorded in its conv_index row (Claude Code stamps gitBranch into
	// every .jsonl turn). Empty when the conv isn't indexed yet or the
	// session isn't inside a git repo.
	Branch string   `json:"branch,omitempty"`
	Online bool     `json:"online"`
	Groups []string `json:"groups"`
}

func runLs(p *lsParams, stdout, stderr io.Writer) int {
	if _, _, err := parseStateFilter(p.State); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcInvalidArg
	}
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
	wantOnline, applyState, _ := parseStateFilter(p.State)
	if applyState {
		filtered := make([]*peerEntry, 0, len(peers))
		for _, pe := range peers {
			if pe.Online == wantOnline {
				filtered = append(filtered, pe)
			}
		}
		peers = filtered
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
		table.Column{Header: "ALIAS", MinWidth: 6, Weight: 0.5, Truncate: true},
		table.Column{Header: "NAME", MinWidth: 8, Weight: 0.8, Truncate: true},
		table.Column{Header: "ROLE", MinWidth: 6, Weight: 0.4, Truncate: true},
		table.Column{Header: "GROUPS", MinWidth: 8, Weight: 0.6, Truncate: true},
		table.Column{Header: "BRANCH", MinWidth: 8, Weight: 0.6, Truncate: true},
		table.Column{Header: "DESCR", MinWidth: 10, Weight: 1.2, Truncate: true},
	)
	tbl.SetTerminalWidth(table.GetTerminalWidth())
	for _, pe := range peers {
		// ALIAS is the per-group handle (may be empty for ungrouped
		// online agents); NAME is the conv's display title. Kept as
		// separate columns so a renamed agent and its group alias are
		// both visible at a glance.
		tbl.AddRow(table.Row{Cells: []string{
			onlineMark(pe.Online),
			short(pe.ConvID),
			pe.Alias,
			pe.Title,
			pe.Role,
			strings.Join(pe.Groups, ","),
			pe.Branch,
			pe.Descr,
		}})
	}
	fmt.Fprintln(stdout, tbl.Render())
	return rcOK
}
