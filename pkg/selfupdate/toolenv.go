package selfupdate

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// toolEnv is the environment the custom command and the go builds of job
// run with: Hooks.ToolEnv's fresh terminal environment, asked for once per
// job. agentd's own environment is the one it started with, possibly long
// before a toolchain upgrade it would point back at, so it is only the
// fallback, and the job says so.
func (s *Service) toolEnv(ctx context.Context, job *Job) []string {
	s.envMu.Lock()
	defer s.envMu.Unlock()
	if s.envJob == job.ID && s.env != nil {
		return s.env
	}
	env := os.Environ()
	if s.hooks.ToolEnv != nil {
		fresh, err := s.hooks.ToolEnv(ctx)
		if err == nil && len(fresh) > 0 {
			env = fresh
		} else {
			if err == nil {
				err = fmt.Errorf("it returned no environment")
			}
			slog.Warn("selfupdate: running the update with agentd's environment", "job", job.ID, "error", err)
			job.Warnings = append(job.Warnings, "could not get a fresh environment from your login shell ("+err.Error()+"); the update ran with agentd's environment from when it started, which may point at an old toolchain")
		}
	}
	s.envJob, s.env = job.ID, env
	return env
}

// lookPathIn finds name in env's PATH, the way the command's own shell
// would; exec.Command would search this process's PATH instead. Unfound,
// it returns name for exec to report.
func lookPathIn(name string, env []string) string {
	path := ""
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			path = v
		}
	}
	for _, dir := range filepath.SplitList(path) {
		if dir == "" || !filepath.IsAbs(dir) {
			continue
		}
		p := filepath.Join(dir, name)
		if info, err := os.Stat(p); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return p
		}
	}
	return name
}
