package ccworkflows

import (
	"fmt"
	"os"
)

// This file holds thin, default-resolving convenience wrappers so callers (the
// CLI / web layers) never need to know where things live on disk — all path
// resolution stays inside ccworkflows.

// ListAllRuns enumerates every workflow run on this machine, across all CC
// sessions, reading from the default projects root (~/.claude/projects).
func ListAllRuns() ([]RunRef, error) {
	root, err := DefaultProjectsRoot()
	if err != nil {
		return nil, err
	}
	return ListRuns(root)
}

// DefaultSavedScripts lists saved workflow templates from the user dir
// (~/.claude/workflows/saved) and, when projectDir is non-empty, its
// project-local mirror (<projectDir>/.claude/workflows/saved).
func DefaultSavedScripts(projectDir string) ([]SavedScript, error) {
	return ListSavedScripts(SavedScriptsDirs(projectDir)...)
}

// FindRun locates a run by id across all sessions and loads its full typed
// state, also returning the lightweight ref (session/project join info). The
// ref is non-nil even when loading the state fails, so callers can still report
// where the run lives.
func FindRun(runID string) (*RunState, *RunRef, error) {
	refs, err := ListAllRuns()
	if err != nil {
		return nil, nil, err
	}
	for i := range refs {
		if refs[i].RunID == runID {
			rs, loadErr := LoadRun(refs[i].SessionDir, runID)
			return rs, &refs[i], loadErr
		}
	}
	return nil, nil, fmt.Errorf("run %q not found in any CC session", runID)
}

// FindSavedScript locates a saved template by name (its filename without .js)
// and returns its metadata plus the raw script source. projectDir, when
// non-empty, also searches the project-local mirror.
func FindSavedScript(name, projectDir string) (*SavedScript, string, error) {
	scripts, err := DefaultSavedScripts(projectDir)
	if err != nil {
		return nil, "", err
	}
	for i := range scripts {
		if scripts[i].Name == name {
			data, readErr := os.ReadFile(scripts[i].Path)
			if readErr != nil {
				return &scripts[i], "", fmt.Errorf("reading saved script %q: %w", name, readErr)
			}
			return &scripts[i], string(data), nil
		}
	}
	return nil, "", fmt.Errorf("no saved workflow named %q found", name)
}
