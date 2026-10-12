package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// pi's on-disk layout, observed at PiMinimumVersion:
//
//	<agent dir>   = $PI_CODING_AGENT_DIR, else ~/.pi/agent
//	<sessions>    = $PI_CODING_AGENT_SESSION_DIR, else settings.json
//	                "sessionDir" (relative to the working directory), else
//	                <agent dir>/sessions
//	session file  = <sessions>/--<cwd with separators as ->--/<ISO time>_<id>.jsonl
//
// A `--session-dir` flag outranks all of them, but tclaude never passes one.
const (
	PiAgentDirEnvVar   = "PI_CODING_AGENT_DIR"
	PiSessionDirEnvVar = "PI_CODING_AGENT_SESSION_DIR"
)

// piAgentDir resolves the agent directory for a launch environment. getenv nil
// means the ambient environment.
func piAgentDir(getenv func(string) string) (string, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	if dir := strings.TrimSpace(getenv(PiAgentDirEnvVar)); dir != "" {
		return expandPiHome(dir)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".pi", "agent"), nil
}

// piSessionsDir resolves where pi stores sessions for a process started in cwd.
func piSessionsDir(getenv func(string) string, cwd string) (string, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	if dir := strings.TrimSpace(getenv(PiSessionDirEnvVar)); dir != "" {
		return piResolveDir(dir, cwd)
	}
	agentDir, err := piAgentDir(getenv)
	if err != nil {
		return "", err
	}
	if raw, err := os.ReadFile(filepath.Join(agentDir, "settings.json")); err == nil {
		var settings struct {
			SessionDir string `json:"sessionDir"`
		}
		if json.Unmarshal(raw, &settings) == nil && strings.TrimSpace(settings.SessionDir) != "" {
			return piResolveDir(settings.SessionDir, cwd)
		}
	}
	return filepath.Join(agentDir, "sessions"), nil
}

func piResolveDir(dir, cwd string) (string, error) {
	dir, err := expandPiHome(strings.TrimSpace(dir))
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(dir) && cwd != "" {
		dir = filepath.Join(cwd, dir)
	}
	return filepath.Clean(dir), nil
}

func expandPiHome(dir string) (string, error) {
	if dir == "~" || strings.HasPrefix(dir, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, strings.TrimPrefix(dir, "~")), nil
	}
	return dir, nil
}

// piFindSessionFile returns the absolute path of the session file for id, or
// "" when none is found. pi names files <timestamp>_<id>.jsonl, in a per-cwd
// subdirectory by default and flat under a custom session dir; both are
// searched. An id carrying glob or path syntax never matches.
func piFindSessionFile(sessionsDir, id string) string {
	if id == "" || sessionsDir == "" || strings.ContainsAny(id, `*?[]\/`) {
		return ""
	}
	var matches []string
	for _, pattern := range []string{
		filepath.Join(sessionsDir, "*", "*_"+id+".jsonl"),
		filepath.Join(sessionsDir, "*_"+id+".jsonl"),
	} {
		found, _ := filepath.Glob(pattern)
		matches = append(matches, found...)
	}
	if len(matches) == 0 {
		return ""
	}
	// A session id is unique within a store; should a copy ever exist twice,
	// prefer a stable choice over directory order.
	sort.Strings(matches)
	return matches[0]
}
