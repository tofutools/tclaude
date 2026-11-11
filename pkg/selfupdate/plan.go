package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Plan is a dry run of an apply under the node's update settings: whether
// the method may run here, what it would run, and the local preconditions
// tclaude can check without installing anything. It fetches, builds and
// runs nothing: NotChecked says what only a real update finds out.
type Plan struct {
	// Method is the chosen method; empty means none is chosen and an
	// apply uses each binary's detected behaviour.
	Method string `json:"method"`
	// Ready is false when a check failed: the update would not start, or
	// would fail at that step.
	Ready          bool        `json:"ready"`
	CurrentVersion string      `json:"current_version"`
	LatestVersion  string      `json:"latest_version,omitempty"`
	CheckedAt      *time.Time  `json:"checked_at,omitempty"`
	Checks         []PlanCheck `json:"checks"`
	// Steps is what an update would run or fetch, in order.
	Steps      []string `json:"steps"`
	NotChecked []string `json:"not_checked"`
}

// PlanCheck is one checked precondition. State is ok, warn or fail.
type PlanCheck struct {
	Name   string `json:"name"`
	State  string `json:"state"`
	Detail string `json:"detail"`
}

// PlanOptions shape a Plan for its reader.
type PlanOptions struct {
	// RedactCommand withholds the custom command, which is the
	// operator's free text, from the steps.
	RedactCommand bool
}

const (
	planOK   = "ok"
	planWarn = "warn"
	planFail = "fail"
)

// Plan dry-runs an apply under opts. It is cheap and local: PATH lookups,
// a write probe of each destination directory, and git status in a
// checkout. It needs no lock beyond the service's own.
func (s *Service) Plan(ctx context.Context, opts ApplyOptions, po PlanOptions) Plan {
	s.mu.Lock()
	bins := append([]Binary{}, s.binaries...)
	status := s.status
	refusal := s.admit(opts)
	releaseOnly := s.hooks.ReleaseOnly
	s.mu.Unlock()

	p := Plan{Method: opts.Method, CurrentVersion: status.CurrentVersion, LatestVersion: status.LatestVersion, CheckedAt: status.CheckedAt, Checks: []PlanCheck{}, Steps: []string{}, NotChecked: []string{}}
	add := func(name, state, detail string) {
		p.Checks = append(p.Checks, PlanCheck{Name: name, State: state, Detail: detail})
	}
	version := status.LatestVersion
	if version == "" {
		version = "<latest release>"
	}

	if opts.Method == "" {
		add("Method", planWarn, "No method is chosen: scheduled and start-up updates stay off, and Update now uses how each binary was installed.")
	} else if info := methodByID(Methods(bins), opts.Method); info == nil {
		add("Method", planFail, "unknown update method "+opts.Method)
	} else if !info.Available {
		add("Method", planFail, info.Label+" cannot run here: "+info.UnavailableReason)
	} else {
		add("Method", planOK, info.Label+" can run here.")
	}
	switch {
	case errors.Is(refusal, ErrNotifyOnly):
		add("Install", planOK, "This method only checks and notifies; Update now installs nothing.")
		p.Steps = append(p.Steps, "Check GitHub for the latest release and notify you when one is newer.")
		p.NotChecked = append(p.NotChecked, "Whether GitHub answers (Check now asks it).")
		p.Ready = !hasFailed(p.Checks)
		return p
	case refusal != nil:
		add("Allowed", planFail, refusal.Error())
	}

	sys := currentSystem()
	p.Steps = append(p.Steps, "Ask GitHub for the latest release ("+apiRoot+"/releases/latest).")
	var external []Binary
	staged := map[string]bool{}
	seen := map[string]bool{}
	for _, b := range bins {
		if seen[b.Name] {
			continue
		}
		seen[b.Name] = true
		route := s.routeBinary(b, opts)
		switch {
		case route.external:
			external = append(external, b)
			continue
		case route.skipped != "":
			add(b.Name, planWarn, route.skipped)
			continue
		}
		staged[route.how] = true
		switch route.how {
		case MethodRelease:
			name, err := archiveName(b.Name)
			if err != nil {
				add(b.Name, planFail, err.Error())
				continue
			}
			p.Steps = append(p.Steps, fmt.Sprintf("Download %s%s/%s and verify it against the release's checksums.", downloadRoot, version, name))
		case MethodGoInstall:
			v := version
			if opts.Method == "" && b.Method == "source" && !releaseOnly {
				v = "latest"
			}
			p.Steps = append(p.Steps, fmt.Sprintf("Build it: go install %s@%s", binaryPackages[b.Name], v))
		case MethodSourceCheckout:
			// One build for every binary, checked below.
		default:
			add(b.Name, planFail, fmt.Sprintf("unsupported update method %q", route.how))
			continue
		}
		state, detail := writableDir(filepath.Dir(route.dest))
		if route.dest == b.Path {
			add(b.Name, state, fmt.Sprintf("Replaces %s (the current file is kept for rollback). %s", route.dest, detail))
		} else {
			add(b.Name, state, fmt.Sprintf("Installs to %s, next to %s at %s. %s", route.dest, b.ManagedBy, b.Path, detail))
		}
	}
	// The tools an update would run are looked for, and git asked, in the
	// environment the update would give them.
	var env []string
	look := func(tool string) bool {
		_, err := sys.LookPath(tool)
		return err == nil
	}
	if staged[MethodGoInstall] || staged[MethodSourceCheckout] || len(external) > 0 {
		if state, detail, fresh := s.planEnv(ctx, po); detail != "" {
			add("Environment", state, detail)
			if fresh != nil {
				env = fresh
				look = func(tool string) bool { _, ok := lookPathIn(tool, env); return ok }
			}
		}
	}
	run := func(ctx context.Context, argv []string, extra ...string) ([]byte, error) {
		if env == nil {
			return sys.Run(ctx, argv, nil)
		}
		return sys.Run(ctx, argv, withEnv(env, extra))
	}
	if staged[MethodGoInstall] || staged[MethodSourceCheckout] {
		if look("go") {
			add("Go toolchain", planOK, "go is on PATH.")
		} else {
			add("Go toolchain", planFail, "go is not on PATH.")
		}
	}
	if staged[MethodSourceCheckout] {
		p.Steps = append(p.Steps, checkoutSteps(opts.Checkout, version)...)
		state, detail := checkCheckout(ctx, run, opts.Checkout, look("git"))
		add("Checkout", state, detail)
	}
	if len(external) > 0 {
		names := []string{}
		for _, b := range external {
			names = append(names, b.Name+" ("+b.Path+")")
		}
		if opts.Method == MethodCustom {
			cmd := strings.TrimSpace(opts.CustomCommand)
			timeout := opts.CustomTimeout
			if timeout <= 0 {
				timeout = 30 * time.Minute
			}
			switch {
			case cmd == "":
				add("Command", planFail, "No custom command is set (updates.self.custom.command).")
			case po.RedactCommand:
				add("Command", planOK, "A command is set (withheld from peers).")
				p.Steps = append(p.Steps, fmt.Sprintf("Run the node's custom command with /bin/sh -c (timeout %s).", timeout))
			default:
				add("Command", planOK, "A command is set.")
				p.Steps = append(p.Steps, fmt.Sprintf("Run with /bin/sh -c (timeout %s): %s", timeout, cmd))
			}
			if _, err := os.Stat("/bin/sh"); err != nil {
				add("Shell", planFail, "/bin/sh is missing.")
			}
			p.Steps = append(p.Steps, "Check that "+strings.Join(names, ", ")+" still runs as tclaude and speaks the same federation protocol.")
			p.NotChecked = append(p.NotChecked, "What the custom command does: it is not run. A command that leaves the binary unchanged counts as already up to date.")
		} else {
			b := external[0]
			for _, argv := range ManagerCommands(b.ManagedBy, b.Package) {
				p.Steps = append(p.Steps, "Run: "+strings.Join(argv, " "))
			}
			p.Steps = append(p.Steps, "Check the version "+b.ManagedBy+" installed for "+strings.Join(names, ", ")+".")
			p.NotChecked = append(p.NotChecked, "Whether "+b.ManagedBy+" has a newer tclaude, and whether its command succeeds.")
		}
	}
	if len(staged) > 0 {
		p.Steps = append(p.Steps, "Check the new binaries report the release version and the same federation protocol, and accept agentd's current flags, before anything is replaced.")
	}
	p.Steps = append(p.Steps, "Restart agentd if a binary changed; agent sessions keep running.")
	if staged[MethodRelease] || staged[MethodGoInstall] || staged[MethodSourceCheckout] {
		p.NotChecked = append(p.NotChecked, "Network access to GitHub and the Go module proxy, and whether the download or build succeeds: nothing is fetched or built.")
	}
	p.NotChecked = append(p.NotChecked, "Whether the new binary accepts agentd's flags: that needs the new binary, and the real update checks it.")
	p.Ready = !hasFailed(p.Checks)
	return p
}

func methodByID(list []MethodInfo, id string) *MethodInfo {
	for i := range list {
		if list[i].ID == id {
			return &list[i]
		}
	}
	return nil
}

func hasFailed(checks []PlanCheck) bool {
	for _, c := range checks {
		if c.State == planFail {
			return true
		}
	}
	return false
}

// writableDir probes whether a file can be created in dir, the way an
// install replaces a binary (a new file renamed over the old). A missing
// dir is judged by the nearest existing parent, which an install creates
// it under.
func writableDir(dir string) (string, string) {
	probe := dir
	for {
		info, err := os.Stat(probe)
		if err == nil {
			if !info.IsDir() {
				return planFail, probe + " is not a directory."
			}
			break
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return planFail, "No existing parent of " + dir + "."
		}
		probe = parent
	}
	f, err := os.CreateTemp(probe, ".tclaude-update-probe-")
	if err != nil {
		return planFail, "Cannot write to " + probe + ": " + err.Error()
	}
	_ = f.Close()
	_ = os.Remove(f.Name())
	if probe != dir {
		return planOK, dir + " will be created (" + probe + " is writable)."
	}
	return planOK, dir + " is writable."
}

func checkoutSteps(c CheckoutOptions, version string) []string {
	ref := c.Ref
	if ref == "" {
		ref = version
	}
	path := c.Path
	if path == "" {
		path = "<checkout>"
	}
	steps := []string{"In " + path + ": git fetch --tags origin, then git checkout " + ref}
	if c.Ref != "" {
		steps = append(steps, "If "+c.Ref+" is a branch: git merge --ff-only @{u} (and compare commits, not release versions)")
	}
	return append(steps, "Build it: go -C "+path+" install . ./cmd/...")
}

// checkCheckout checks a source checkout's local preconditions: it exists,
// is a git work tree, and has no uncommitted changes (an update refuses
// those).
func checkCheckout(ctx context.Context, run toolRunner, c CheckoutOptions, hasGit bool) (string, string) {
	switch {
	case c.Path == "":
		return planFail, "No checkout path is set (updates.self.source_checkout.path)."
	case !filepath.IsAbs(c.Path):
		return planFail, c.Path + " is not an absolute path."
	case !hasGit:
		return planFail, "git is not on PATH."
	}
	if info, err := os.Stat(c.Path); err != nil || !info.IsDir() {
		return planFail, c.Path + " is not a directory."
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := run(cctx, []string{"git", "-C", c.Path, "status", "--porcelain"})
	switch {
	case err != nil:
		return planFail, c.Path + " is not a usable git checkout: " + tail(out, 256)
	case strings.TrimSpace(string(out)) != "":
		return planFail, c.Path + " has uncommitted changes; an update refuses until they are committed or stashed."
	}
	return planOK, c.Path + " is a clean git checkout."
}

// planEnvTTL is how long Plan reuses the login shell's answer.
const planEnvTTL = 30 * time.Second

// planEnv says which environment an update's tools would run with, asking
// the login shell the way the update would. fresh is that environment, or
// nil for the fallback; an empty detail means there is nothing to say (no
// ToolEnv hook). A peer is not told why the shell failed.
func (s *Service) planEnv(ctx context.Context, po PlanOptions) (state, detail string, fresh []string) {
	if s.hooks.ToolEnv == nil {
		return "", "", nil
	}
	s.planEnvMu.Lock()
	if s.planEnvAt.IsZero() || time.Since(s.planEnvAt) > planEnvTTL {
		s.planEnvVal, s.planEnvErr = s.freshEnv(ctx)
		s.planEnvAt = time.Now()
	}
	env, err := s.planEnvVal, s.planEnvErr
	s.planEnvMu.Unlock()
	if err != nil {
		reason := err.Error()
		if po.RedactCommand {
			reason = "withheld from peers"
		}
		return planWarn, "Your login shell gave no environment (" + reason + "): the update would run its tools with agentd's environment from when it started, which may point at an old toolchain.", nil
	}
	return planOK, "The update runs its tools with a fresh environment from your login shell, as a new terminal would.", env
}
