package selfupdate

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// Every tool an update runs (the custom command, a package manager, go,
// git, the new binary's setup) runs through a toolRunner, so it gets the
// environment a new terminal would have: agentd's own is the one it
// started with, possibly from before a toolchain upgrade it would point
// back at ("compile: version go1.27.3 does not match go tool version
// go1.27.4"). A method's code reaches tools only through the runner it is
// handed; TestEveryMethodRunsToolsWithTheFreshEnvironment walks them all.
//
// A release download runs no tool: it fetches and verifies an archive, and
// runs only the new tclaude binary itself (its version and flag checks).
// Detection (Detect, classify's probes) and locate/ShadowWarning stay on
// agentd's PATH: they ask where agentd itself finds tclaude, not run an
// update.

// toolRunner runs argv with the job's tool environment, extra (KEY=VALUE)
// set over it. argv[0] is looked up in that environment's PATH.
type toolRunner func(ctx context.Context, argv []string, extra ...string) ([]byte, error)

// runner is the job's toolRunner.
func (s *Service) runner(job *Job) toolRunner {
	return func(ctx context.Context, argv []string, extra ...string) ([]byte, error) {
		return currentSystem().Run(ctx, argv, withEnv(s.toolEnv(ctx, job), extra))
	}
}

// toolEnv is the job's tool environment: Hooks.ToolEnv's, asked for once
// per job. Without the hook, or when it fails, it is this process's
// environment, and the job says why.
func (s *Service) toolEnv(ctx context.Context, job *Job) []string {
	s.envMu.Lock()
	defer s.envMu.Unlock()
	if s.envJob == job.ID && s.env != nil {
		return s.env
	}
	env, err := s.freshEnv(ctx)
	if err != nil {
		slog.Warn("selfupdate: running the update's tools with agentd's environment", "job", job.ID, "error", err)
		job.Warnings = append(job.Warnings, fallbackWarning(err))
	}
	s.envJob, s.env = job.ID, env
	return env
}

// freshEnv asks Hooks.ToolEnv for the environment. On an error it returns
// this process's environment with it; with no hook, that environment and
// no error (a host, like the hub, that runs no tools).
func (s *Service) freshEnv(ctx context.Context) ([]string, error) {
	if s.hooks.ToolEnv == nil {
		return os.Environ(), nil
	}
	env, err := s.hooks.ToolEnv(ctx)
	if err == nil && len(env) == 0 {
		err = fmt.Errorf("it returned no environment")
	}
	if err != nil {
		return os.Environ(), err
	}
	return env, nil
}

func fallbackWarning(err error) string {
	return "could not get a fresh environment from your login shell (" + err.Error() + "); the update ran with agentd's environment from when it started, which may point at an old toolchain"
}

// dropToolEnv forgets the job's environment once the job's tools ran: it
// holds the profile's variables, secrets included.
func (s *Service) dropToolEnv() {
	s.envMu.Lock()
	defer s.envMu.Unlock()
	s.envJob, s.env = "", nil
}

// withEnv returns env with extra set over it: a key extra sets is dropped
// from env.
func withEnv(env, extra []string) []string {
	if len(extra) == 0 {
		return env
	}
	set := map[string]bool{}
	for _, kv := range extra {
		k, _, _ := strings.Cut(kv, "=")
		set[k] = true
	}
	out := make([]string, 0, len(env)+len(extra))
	for _, kv := range env {
		if k, _, _ := strings.Cut(kv, "="); !set[k] {
			out = append(out, kv)
		}
	}
	return append(out, extra...)
}

// lookPathIn finds name in env's PATH, the way the command's own shell
// would; exec.Command would search this process's PATH instead. A name
// with a slash is returned as is.
func lookPathIn(name string, env []string) (string, bool) {
	if strings.ContainsRune(name, '/') {
		return name, true
	}
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
			return p, true
		}
	}
	return name, false
}
