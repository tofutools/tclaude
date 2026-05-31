package ccworkflows

import (
	"os"
	"path/filepath"
	"strings"
)

// CC persists a workflow run under the transcript directory of the CC session
// that launched it:
//
//	<projectsRoot>/<projectEncoded>/<sessionUUID>/
//	  workflows/<runId>.json                       completed-run record (at completion)
//	  workflows/scripts/<name>-<runId>.js          resolved script snapshot (at launch)
//	  subagents/workflows/<runId>/journal.jsonl    append-only live journal
//	  subagents/workflows/<runId>/agent-<id>.jsonl per-agent transcript
//
// projectsRoot is normally ~/.claude/projects. There is no global run index, so
// enumeration is a glob across this tree (see ListRuns).

// runIDPrefix is the stable, observed prefix of every workflow run id
// (e.g. wf_213c457c-3ac). The remainder of the format is not relied upon.
const runIDPrefix = "wf_"

// DefaultProjectsRoot returns ~/.claude/projects, the machine-wide root holding
// every CC session's transcript tree.
func DefaultProjectsRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "projects"), nil
}

// LooksLikeRunID reports whether s has the workflow run-id shape.
func LooksLikeRunID(s string) bool {
	return strings.HasPrefix(s, runIDPrefix) && len(s) > len(runIDPrefix)
}

func sessionWorkflowsDir(sessionDir string) string {
	return filepath.Join(sessionDir, "workflows")
}

func sessionRunScriptsDir(sessionDir string) string {
	return filepath.Join(sessionDir, "workflows", "scripts")
}

func completedRunPath(sessionDir, runID string) string {
	return filepath.Join(sessionDir, "workflows", runID+".json")
}

func runJournalDir(sessionDir, runID string) string {
	return filepath.Join(sessionDir, "subagents", "workflows", runID)
}

func runJournalPath(sessionDir, runID string) string {
	return filepath.Join(runJournalDir(sessionDir, runID), "journal.jsonl")
}

// findRunScript returns the path to a run's resolved script snapshot, found by
// matching the `*-<runId>.js` naming in the session's scripts dir. The leading
// name segment is the workflow name, which we don't know a-priori, hence the
// suffix match.
func findRunScript(sessionDir, runID string) (string, bool) {
	matches, err := filepath.Glob(filepath.Join(sessionRunScriptsDir(sessionDir), "*-"+runID+".js"))
	if err != nil || len(matches) == 0 {
		return "", false
	}
	return matches[0], true
}
