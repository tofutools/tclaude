package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
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

// errAmbiguous is returned by resolveSelector when the selector matches more
// than one conversation. The caller should print candidates and exit 2.
var errAmbiguous = errors.New("selector matches multiple conversations")

// resolved is a minimal handle to a conversation reachable by a selector.
type resolved struct {
	convID string
	row    *db.ConvIndexRow // may be nil if conv not yet indexed
}

// resolveSelector turns a free-form string (UUID, prefix, or current title)
// into a single conv-id. Search is global (any project) by default — agent
// groups are project-agnostic.
//
// `.` and `-` resolve to the current session via $TCLAUDE_SESSION_ID, with
// the parent CC pid file as fallback.
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

	// 1) full UUID match
	if row, err := db.GetConvIndex(selector); err == nil && row != nil {
		return &resolved{convID: row.ConvID, row: row}, nil, nil
	}

	// 2) prefix match (uniqueness enforced by db.FindConvIndexByPrefix)
	if row, err := db.FindConvIndexByPrefix(selector); err == nil && row != nil {
		return &resolved{convID: row.ConvID, row: row}, nil, nil
	}

	// 3) match by current display title (custom > summary > first prompt)
	rows, err := db.ListAllConvIndex()
	if err != nil {
		return nil, nil, err
	}
	var matches []*resolved
	for _, r := range rows {
		if displayTitle(r) == selector {
			matches = append(matches, &resolved{convID: r.ConvID, row: r})
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
		if m.row != nil {
			title = displayTitle(m.row)
		}
		fmt.Fprintf(out, "  %s  %s\n", m.convID[:8], title)
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
	id, err := currentConvID()
	if err != nil {
		// No conv-id resolvable. If there's no `claude` ancestor in the
		// process tree, the invoker is the human — say so.
		if session.FindClaudePID() == 0 {
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
		RunFunc: func(p *lookupParams, _ *cobra.Command, _ []string) {
			os.Exit(runLookup(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runLookup(p *lookupParams, stdout, stderr io.Writer) int {
	r, matches, err := resolveSelector(p.Selector)
	if errors.Is(err, errAmbiguous) {
		printAmbiguous(stderr, p.Selector, matches)
		return rcAmbiguous
	}
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcNotFound
	}
	fmt.Fprintln(stdout, r.convID)
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
	Groups []string `json:"groups"`
}

func runLs(p *lsParams, stdout, stderr io.Writer) int {
	myID, err := currentConvID()
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcNotFound
	}
	groups, err := db.ListGroupsForConv(myID)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcIOFailure
	}

	// Aggregate peers across groups.
	byConv := map[string]*peerEntry{}
	for _, g := range groups {
		members, err := db.ListAgentGroupMembers(g.ID)
		if err != nil {
			continue
		}
		for _, m := range members {
			if m.ConvID == myID {
				continue
			}
			pe, ok := byConv[m.ConvID]
			if !ok {
				row, _ := db.GetConvIndex(m.ConvID)
				title := "(unknown)"
				if row != nil {
					if t := displayTitle(row); t != "" {
						title = t
					}
				}
				pe = &peerEntry{
					ConvID: m.ConvID,
					Title:  title,
					Alias:  m.Alias,
					Role:   m.Role,
					Descr:  m.Descr,
				}
				byConv[m.ConvID] = pe
			}
			pe.Groups = append(pe.Groups, g.Name)
		}
	}

	peers := make([]*peerEntry, 0, len(byConv))
	for _, pe := range byConv {
		peers = append(peers, pe)
	}

	if p.JSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(peers)
		return rcOK
	}

	if len(peers) == 0 {
		fmt.Fprintln(stdout, "(no peers — you are not in any groups, or your groups have no other members)")
		return rcOK
	}

	for _, pe := range peers {
		short := pe.ConvID
		if len(short) >= 8 {
			short = short[:8]
		}
		alias := pe.Alias
		if alias == "" {
			alias = pe.Title
		}
		fmt.Fprintf(stdout, "%s  %-20s  %-15s  groups=%v\n", short, alias, pe.Role, pe.Groups)
		if pe.Descr != "" {
			fmt.Fprintf(stdout, "          %s\n", pe.Descr)
		}
	}
	return rcOK
}

