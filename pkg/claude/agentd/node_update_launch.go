package agentd

import (
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/tofutools/tclaude/pkg/launcher"
	"github.com/tofutools/tclaude/pkg/selfupdate"
)

// launchEnviron is the environment this process was started with, captured
// before anything in it runs: the CLI entry prepends the managed-harness
// directory to PATH and serve exports the socket and resource-delegation
// paths. An in-place restart hands the new binary this environment, so it
// starts as this one was started, and the update's before/after comparison
// reads the same thing on both sides.
// A login-shell failure the launcher handed over is reported once, by the
// daemon it started, not again after every in-place restart. The relaunch
// helper's PID (relaunchHelperVar) is this process's own, not a launch
// parameter.
var launchEnviron = slices.DeleteFunc(os.Environ(), func(kv string) bool {
	return strings.HasPrefix(kv, launcher.LoginEnvErrorVar+"=") || strings.HasPrefix(kv, relaunchHelperVar+"=")
})

// selfUpdateProcess returns this daemon's argv and launch environment; tests
// swap it.
var selfUpdateProcess = func() (argv, environ []string) {
	return append([]string(nil), os.Args...), append([]string(nil), launchEnviron...)
}

// selfUpdateLaunchPlan snapshots how this daemon was launched for an update
// job: its argv and environment, the launcher, and the command lines a
// restart may use, for the pre-flight check of the new binary. nil when the
// snapshot cannot be taken (the update then runs without it).
func selfUpdateLaunchPlan() *selfupdate.LaunchPlan {
	argv, environ := selfUpdateProcess()
	info := currentLauncher(false)
	snap, err := selfupdate.CaptureLaunch(argv, environ, info.Kind, info.Unit, info.RestartMethod)
	if err != nil {
		slog.Warn("self-update: could not snapshot the daemon's launch", "error", err)
		return nil
	}
	plan := &selfupdate.LaunchPlan{Snapshot: snap, Binary: daemonBinaryName(argv)}
	// An in-place re-exec reuses this argv; a supervisor restart or an exit
	// starts the unit's own command line.
	candidates := [][]string{argv}
	if raw := info.RawArgv(); info.Supervised() && !info.Wrapped && len(raw) > 0 && !slices.Equal(raw, argv) {
		candidates = append(candidates, raw)
	}
	for _, c := range candidates {
		if launcher.IsServeArgv(c) && daemonBinaryName(c) == plan.Binary {
			plan.Args = append(plan.Args, append([]string(nil), c[1:]...))
		}
	}
	return plan
}

// daemonBinaryName is the binary argv runs the daemon with: the standalone
// tclaude-agentd, or tclaude (`tclaude agentd serve`).
func daemonBinaryName(argv []string) string {
	if len(argv) > 0 && filepath.Base(argv[0]) == "tclaude-agentd" {
		return "tclaude-agentd"
	}
	if exe, err := os.Executable(); err == nil && filepath.Base(exe) == "tclaude-agentd" && (len(argv) < 2 || argv[1] != "agentd") {
		return "tclaude-agentd"
	}
	return "tclaude"
}

// compareRestartedLaunch records on the job that restarted this daemon how
// its flags and environment compare with the snapshot taken before the
// update.
func compareRestartedLaunch(svc *selfupdate.Service) *selfupdate.Job {
	argv, environ := selfUpdateProcess()
	info := currentLauncher(false)
	job, err := svc.CompareLaunch(argv, environ, info.Kind, info.Unit, info.RestartMethod)
	if err != nil {
		slog.Warn("self-update: could not record the launch comparison", "error", err)
		return nil
	}
	if job != nil {
		if exp := expectedLaunchAfterUpdate(); exp != nil {
			if withExpected, err := svc.RecordExpectedLaunch(job.ID, *exp); err != nil {
				slog.Warn("self-update: could not record the comparison with the configured launch", "error", err)
			} else if withExpected != nil {
				job = withExpected
			}
		}
	}
	if job != nil && job.Launch != nil && job.Launch.State == "differs" {
		slog.Warn("self-update: the restarted daemon's flags or environment differ from before the update", "differences", job.Launch.Summary())
	}
	return job
}
