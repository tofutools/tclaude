package session

import (
	"fmt"
	"os"
	"sort"
	"strings"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// resolveResumeLast turns --resume-last into a plain --resume of the newest
// conversation in the launch directory, so it gets everything a resume gets:
// recorded posture, and the refusal to open a conversation that is already live.
//
// A session that ended before its first message has no transcript, so it can't
// be picked. Retired and archived conversations are skipped: the operator put
// them away, and one running again stays hidden from the live dashboard lists.
func resolveResumeLast(params *NewParams) error {
	if !params.ResumeLast {
		return nil
	}
	switch {
	case strings.TrimSpace(params.Resume) != "":
		return fmt.Errorf("--resume-last conflicts with --resume")
	case params.SessionID != "":
		return fmt.Errorf("--resume-last conflicts with --session-id")
	case params.Global:
		return fmt.Errorf("--resume-last looks in the current directory; it can't be combined with --global")
	}
	h, err := harness.Resolve(strings.TrimSpace(params.Harness))
	if err != nil {
		return nil // runNew reports the unknown harness with its own message
	}
	if h.Name != harness.DefaultName {
		return fmt.Errorf("--resume-last only supports %s so far; use tclaude conv resume <id>", harness.DefaultName)
	}
	cwd, err := resolveSessionDir(params.Dir)
	if err != nil {
		return nil // likewise for an unusable --dir
	}
	index, err := convops.LoadSessionsIndex(convops.GetClaudeProjectPath(cwd))
	if err != nil {
		return fmt.Errorf("list conversations in %s: %w", cwd, err)
	}
	convID, skippedRetired, err := newestResumableConv(index.Entries)
	if err != nil {
		return err
	}
	if skippedRetired != "" {
		fmt.Fprintf(os.Stderr, "Skipping retired conversation %s; use --resume to open it\n", skippedRetired)
	}
	if convID == "" {
		return fmt.Errorf("no conversation to resume in %s", cwd)
	}
	params.Resume = convID
	return nil
}

// newestResumableConv returns the newest entry worth resuming, plus the id of
// the first retired conversation it passed over on the way, if any.
func newestResumableConv(entries []convops.SessionEntry) (convID, skippedRetired string, err error) {
	sorted := make([]convops.SessionEntry, len(entries))
	copy(sorted, entries)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].FileMtime.After(sorted[j].FileMtime) })
	for _, entry := range sorted {
		if entry.IsSidechain || entry.ArchivedAt != "" {
			continue
		}
		state, err := db.AgentState(entry.SessionID)
		if err != nil {
			return "", "", fmt.Errorf("read agent state of conversation %s: %w", entry.SessionID, err)
		}
		if state == db.AgentStateRetired {
			if skippedRetired == "" {
				skippedRetired = entry.SessionID
			}
			continue
		}
		return entry.SessionID, skippedRetired, nil
	}
	return "", skippedRetired, nil
}

// resumePassthrough is what a `tclaude -- <claude args>` command line asks for
// in terms of resuming.
type resumePassthrough struct {
	convSelector string   // --resume <value>
	last         bool     // --continue
	rest         []string // the arguments that stay pass-through
}

// parseResumePassthrough finds Claude Code's own resume flags. It returns nil
// when there is nothing for tclaude to take over: a bare --resume is Claude
// Code's picker, which works as is, and --fork-session or --print change what
// the flag means.
func parseResumePassthrough(args []string) *resumePassthrough {
	out := &resumePassthrough{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--fork-session" || arg == "--print" || arg == "-p":
			return nil
		case arg == "--continue" || arg == "-c":
			out.last = true
		case arg == "--resume" || arg == "-r":
			if i+1 == len(args) || strings.HasPrefix(args[i+1], "-") {
				return nil
			}
			i++
			out.convSelector = args[i]
		case strings.HasPrefix(arg, "--resume="):
			out.convSelector = strings.TrimPrefix(arg, "--resume=")
		default:
			out.rest = append(out.rest, arg)
		}
	}
	if out.last == (out.convSelector != "") {
		return nil
	}
	return out
}

// adoptResumePassthrough moves `tclaude -- --continue` and `tclaude -- --resume
// <id>` onto tclaude's own resume path. Left as pass-through they start a new
// session row and let Claude Code pick the conversation, which bypasses the
// recorded posture and the live-conversation refusal. A selector tclaude can't
// resolve stays with Claude Code, which also accepts search terms there.
func adoptResumePassthrough(params *NewParams) error {
	if params.ManagedLaunch || params.Shell || params.ResumeLast || strings.TrimSpace(params.Resume) != "" {
		return nil
	}
	selected := strings.TrimSpace(params.Harness)
	if selected == "" {
		// A default profile on another harness means these flags are not ours.
		if prof, err := db.GlobalDefaultSpawnProfile(); err == nil && prof != nil {
			selected = strings.TrimSpace(prof.Harness)
		}
	}
	if selected != "" && selected != harness.DefaultName {
		return nil
	}
	found := parseResumePassthrough(clcommon.ExtractClaudeExtraArgs())
	if found == nil {
		return nil
	}
	if found.last {
		params.ResumeLast = true
	} else {
		probe := *params
		probe.Resume = found.convSelector
		if err := expandAgentResumeSelector(&probe); err != nil {
			return err
		}
		cwd, err := resolveSessionDir(probe.Dir)
		if err != nil {
			return nil // runNew reports the unusable --dir
		}
		if _, _, err := resolveResumeConv(harness.Default(), resumeIDFromParam(harness.Default(), probe.Resume), probe.Global, cwd); err != nil {
			return nil
		}
		params.Resume = found.convSelector
	}
	replaceClaudeExtraArgs(found.rest)
	return nil
}

// replaceClaudeExtraArgs rewrites the arguments after `--`, which is where
// clcommon.ExtractClaudeExtraArgs reads them from.
func replaceClaudeExtraArgs(rest []string) {
	for i, arg := range os.Args {
		if arg != "--" {
			continue
		}
		os.Args = append(os.Args[:i:i], "--")
		os.Args = append(os.Args, rest...)
		return
	}
}
