package selfupdate

import (
	"context"
	"debug/buildinfo"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/common/buildversion"
	"golang.org/x/mod/semver"
)

// ApplyOptions are the operator's updates.self choices for one apply. They
// come from the node's own config, never from request input.
type ApplyOptions struct {
	// Method is a MethodIDs entry, or empty for the per-binary detected
	// behaviour used when no method is chosen.
	Method   string
	Checkout CheckoutOptions
	// CustomCommand runs with /bin/sh -c for the custom method.
	CustomCommand string
	CustomTimeout time.Duration
	// PackageManagerAck allows a tclaude-driven method to install next to a
	// package-manager-owned binary, into InstallDir (absolute).
	PackageManagerAck bool
	InstallDir        string
	// PostSetup is the argv after the tclaude binary for the post-update
	// setup run; nil means off.
	PostSetup []string
}

// CheckoutOptions is the source_checkout method's local git checkout.
type CheckoutOptions struct {
	Path string
	Ref  string
}

// ErrNotifyOnly is returned for an apply under the manual method.
var ErrNotifyOnly = errors.New("the manual update method only checks and notifies; install the update yourself or choose another method (tclaude update config set method <m>)")

// PackageManagedError refuses to overwrite a package-manager-owned binary.
type PackageManagedError struct {
	Binary Binary
	// NeedsAck is set when a tclaude-driven method was chosen but the
	// install-next-to-it acknowledgement is missing.
	NeedsAck bool
}

func (e *PackageManagedError) Error() string {
	b := e.Binary
	hint := UpdateHint(b.ManagedBy, b.Package)
	if e.NeedsAck {
		msg := fmt.Sprintf("%s at %s is owned by %s; tclaude will not write into it. Untrack it first", b.Name, b.Path, b.ManagedBy)
		if u := UntrackHint(b.ManagedBy, b.Package); u != "" {
			msg += " (" + u + ")"
		}
		return msg + ", or acknowledge installing next to it: tclaude update config set package-manager-ack true"
	}
	msg := fmt.Sprintf("%s at %s is managed by %s; tclaude will not overwrite it", b.Name, b.Path, b.ManagedBy)
	if hint != "" {
		msg += ". Update it with: " + hint
	}
	return msg + ". To let tclaude update it, choose a method: tclaude update config set method <m>"
}

func managerFor(method string) string {
	for manager, m := range managerMethod {
		if m == method {
			return manager
		}
	}
	return ""
}

// admit refuses an apply that cannot run under opts before a job exists.
// The caller holds s.mu.
func (s *Service) admit(opts ApplyOptions) error {
	if opts.Method == MethodManual {
		return ErrNotifyOnly
	}
	pm := managerFor(opts.Method)
	matched := false
	for _, b := range s.binaries {
		switch {
		case opts.Method == "" && b.ManagedBy != "":
			return &PackageManagedError{Binary: b}
		case TclaudeDriven(opts.Method) && b.ManagedBy != "" && !opts.PackageManagerAck:
			return &PackageManagedError{Binary: b, NeedsAck: true}
		case TclaudeDriven(opts.Method) && opts.Method != MethodCustom && b.ManagedBy != "":
			if err := checkInstallDir(opts.InstallDir, b); err != nil {
				return err
			}
		case opts.Method != MethodCustom && pm == "" && b.ManagedBy == "" && b.Version == "" && !s.hooks.ReleaseOnly:
			return fmt.Errorf("%s at %s is not an identifiable tclaude binary (%s); choose the custom or manual method", b.Name, b.Path, b.Evidence)
		}
		if pm != "" && b.ManagedBy == pm {
			if ManagerCommands(b.ManagedBy, b.Package) == nil {
				return fmt.Errorf("the %s method cannot run: the %s entry for %s is unknown; update it yourself (%s)", opts.Method, pm, b.Path, UpdateHint(b.ManagedBy, b.Package))
			}
			matched = true
		}
	}
	if pm != "" && !matched {
		return fmt.Errorf("the %s method does not apply: no installed tclaude binary is managed by %s", opts.Method, pm)
	}
	return nil
}

// checkInstallDir refuses an install_dir that would write into the package
// manager's own files: a package-manager tree, the managed binary's own
// directory, or a destination the manager owns.
func checkInstallDir(dir string, b Binary) error {
	if !filepath.IsAbs(dir) || PackageManagerTree(filepath.Clean(dir)) {
		return fmt.Errorf("updates.self.install_dir must be an absolute path outside package-manager trees")
	}
	resolvedDir := filepath.Clean(dir)
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		resolvedDir = r
	}
	if resolvedDir == filepath.Dir(b.Path) {
		return fmt.Errorf("updates.self.install_dir %s is the directory %s manages; choose another", dir, b.ManagedBy)
	}
	dest := Binary{Name: b.Name, Path: filepath.Join(resolvedDir, b.Name)}
	if resolved, err := filepath.EvalSymlinks(dest.Path); err == nil {
		dest.Path = resolved
		classify(&dest)
		if dest.ManagedBy != "" {
			return fmt.Errorf("%s is owned by %s; choose another updates.self.install_dir", dest.Path, dest.ManagedBy)
		}
	}
	return nil
}

type applyTarget struct {
	binary  Binary
	dest    string
	staged  string
	created bool
}

func (s *Service) apply(ctx context.Context, release Release, job *Job, authorize func() bool, pin string, opts ApplyOptions) error {
	current := len(s.binaries) > 0
	for _, b := range s.binaries {
		current = current && b.Version == release.Tag
	}
	if current {
		return nil
	}
	if err := s.phase(job, "staging"); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(s.dir, "stage-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	pm := managerFor(opts.Method)
	targets := []applyTarget{}
	external := []Binary{}
	var checkout map[string]string
	seen := map[string]bool{}
	for _, b := range s.binaries {
		if seen[b.Name] {
			continue
		}
		seen[b.Name] = true
		how := opts.Method
		switch {
		case opts.Method == MethodCustom:
			external = append(external, b)
			continue
		case pm != "":
			if b.ManagedBy == pm {
				external = append(external, b)
				continue
			}
			if b.ManagedBy != "" || b.Method != MethodRelease && b.Method != MethodGoInstall {
				job.Warnings = append(job.Warnings, fmt.Sprintf("%s is not managed by %s and was not installed from a release or go install; it was left as is", b.Name, pm))
				continue
			}
			how = b.Method
		case opts.Method == "":
			how = MethodGoInstall
			if b.Method == MethodRelease || s.hooks.ReleaseOnly {
				how = MethodRelease
			}
		}
		t := applyTarget{binary: b, dest: b.Path}
		if b.ManagedBy != "" {
			t.dest = filepath.Join(opts.InstallDir, b.Name)
		}
		switch how {
		case MethodRelease:
			t.staged, err = s.stageRelease(ctx, release, b.Name, dir)
		case MethodGoInstall:
			version := release.Tag
			if opts.Method == "" && b.Method == "source" && pin == "" {
				version = "latest"
			}
			t.staged, err = s.stageGo(ctx, b.Name, version, dir)
		case MethodSourceCheckout:
			if checkout == nil {
				if checkout, err = s.stageCheckout(ctx, opts.Checkout, release.Tag, dir); err != nil {
					return err
				}
			}
			if t.staged = checkout[b.Name]; t.staged == "" {
				return fmt.Errorf("the checkout build did not produce %s", b.Name)
			}
		default:
			return fmt.Errorf("unsupported update method %q", how)
		}
		if err != nil {
			return err
		}
		info, err := s.inspect(ctx, t.staged)
		if err != nil {
			return err
		}
		if how == MethodSourceCheckout && opts.Checkout.Ref != "" {
			if info.Version != release.Tag {
				job.Warnings = append(job.Warnings, fmt.Sprintf("%s built from %s reports %s, not the release %s", b.Name, opts.Checkout.Ref, info.Version, release.Tag))
			}
		} else if info.Version != release.Tag {
			return fmt.Errorf("staged %s version does not match %s", b.Name, release.Tag)
		}
		if info.ProtocolVersion != s.status.ProtocolVersion {
			return fmt.Errorf("target federation protocol %d differs from current %d; linked peers may be incompatible", info.ProtocolVersion, s.status.ProtocolVersion)
		}
		targets = append(targets, t)
	}
	if !authorize() {
		return fmt.Errorf("update authority was revoked before replacement")
	}
	if err := s.phase(job, "backing_up"); err != nil {
		return err
	}
	backupDir := filepath.Join(s.dir, "backup-"+job.ID)
	if err := os.Mkdir(backupDir, 0700); err != nil {
		return err
	}
	manifest := backupManifest{ID: job.ID, Version: release.Tag, Entries: []backupEntry{}}
	backup := func(b Binary, target, after string) (backupEntry, error) {
		info, err := os.Lstat(target)
		if err != nil || !info.Mode().IsRegular() {
			return backupEntry{}, fmt.Errorf("update target is not a regular file")
		}
		hash, err := fileHash(target)
		if err != nil {
			return backupEntry{}, err
		}
		path := filepath.Join(backupDir, fmt.Sprint(len(manifest.Entries)))
		if err := copyExclusive(target, path, 0600); err != nil {
			return backupEntry{}, fmt.Errorf("backup failed: %w", err)
		}
		copied, err := fileHash(path)
		if err != nil || copied != hash {
			return backupEntry{}, fmt.Errorf("update target changed during backup")
		}
		e := backupEntry{Binary: b, Backup: path, BeforeHash: hash, AfterHash: after, Mode: uint32(info.Mode().Perm())}
		if target != b.Path {
			e.Target = target
		}
		return e, nil
	}
	kept := targets[:0]
	for _, t := range targets {
		after, err := fileHash(t.staged)
		if err != nil {
			return err
		}
		// Already installed (say, a shadowed copy next to a package-managed
		// binary): nothing to write, nothing to restart for.
		if hash, err := fileHash(t.dest); err == nil && hash == after {
			continue
		}
		kept = append(kept, t)
	}
	targets = kept
	for i := range targets {
		t := &targets[i]
		after, err := fileHash(t.staged)
		if err != nil {
			return err
		}
		if _, err := os.Lstat(t.dest); errors.Is(err, os.ErrNotExist) {
			t.created = true
			manifest.Entries = append(manifest.Entries, backupEntry{Binary: t.binary, Target: t.dest, Created: true, AfterHash: after, Mode: 0755})
			continue
		}
		e, err := backup(t.binary, t.dest, after)
		if err != nil {
			return err
		}
		manifest.Entries = append(manifest.Entries, e)
	}
	changed := false
	if len(external) > 0 {
		var customBackups []backupEntry
		if opts.Method == MethodCustom {
			for _, b := range external {
				e, err := backup(b, b.Path, "")
				if err != nil {
					return err
				}
				customBackups = append(customBackups, e)
			}
		}
		if !authorize() {
			return fmt.Errorf("update authority revoked")
		}
		ext, err := s.runExternal(ctx, release, job, external, opts)
		if err != nil {
			return err
		}
		changed = ext
		for _, e := range customBackups {
			if hash, err := fileHash(e.Binary.Path); err == nil && hash != e.BeforeHash {
				e.AfterHash = hash
				manifest.Entries = append(manifest.Entries, e)
			}
		}
	}
	if len(manifest.Entries) == 0 {
		_ = os.Remove(backupDir)
	}
	if len(manifest.Entries) == 0 && changed {
		// A package manager replaced the binaries; the older backup no
		// longer matches them.
		if err := os.Remove(filepath.Join(s.dir, "backup.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		s.mu.Lock()
		s.status.RollbackAvailable = false
		s.mu.Unlock()
	} else if len(manifest.Entries) > 0 {
		if err := syncDir(backupDir); err != nil {
			return fmt.Errorf("could not persist backup directory: %w", err)
		}
		if err := writeJSONFile(filepath.Join(s.dir, "backup.json"), manifest); err != nil {
			return err
		}
		s.mu.Lock()
		s.status.RollbackAvailable = true
		s.mu.Unlock()
	}
	if len(targets) > 0 {
		if err := s.phase(job, "replacing"); err != nil {
			return err
		}
	}
	done := []backupEntry{}
	for i, t := range targets {
		e := manifest.Entries[i]
		if !authorize() {
			return s.undo(done, fmt.Errorf("update authority revoked"))
		}
		if t.created {
			if err := os.MkdirAll(filepath.Dir(t.dest), 0755); err != nil {
				return s.undo(done, err)
			}
			if err := copyExclusive(t.staged, t.dest, 0755); err != nil {
				return s.undo(done, err)
			}
			if err := syncDir(filepath.Dir(t.dest)); err != nil {
				return s.undo(append(done, e), err)
			}
			done = append(done, e)
			job.Relocations = append(job.Relocations, Relocation{Name: t.binary.Name, From: t.binary.Path, To: t.dest})
			continue
		}
		hash, err := fileHash(t.dest)
		if err != nil || hash != e.BeforeHash {
			return s.undo(done, fmt.Errorf("update target changed before replacement"))
		}
		if err := replaceFile(t.staged, t.dest, os.FileMode(e.Mode)); err != nil {
			return s.undo(append(done, e), err)
		}
		done = append(done, e)
		if t.dest != t.binary.Path {
			job.Relocations = append(job.Relocations, Relocation{Name: t.binary.Name, From: t.binary.Path, To: t.dest})
		}
	}
	for _, t := range targets {
		if t.dest != t.binary.Path {
			if w := ShadowWarning(t.binary.Name, t.dest); w != "" {
				job.Warnings = append(job.Warnings, w)
			}
		}
	}
	job.RestartRequired = changed || len(targets) > 0
	return nil
}

// ShadowWarning reports when the binary on PATH is not the one at dest.
func ShadowWarning(name, dest string) string {
	found, err := currentSystem().LookPath(name)
	if err != nil {
		return fmt.Sprintf("%s is not on PATH; add %s to PATH", name, filepath.Dir(dest))
	}
	if resolved, err := filepath.EvalSymlinks(found); err == nil {
		found = resolved
	}
	want := dest
	if resolved, err := filepath.EvalSymlinks(dest); err == nil {
		want = resolved
	}
	if found != want {
		return fmt.Sprintf("%s on PATH resolves to %s, not %s; put %s ahead of it on PATH", name, found, dest, filepath.Dir(dest))
	}
	return ""
}

// locate re-resolves a binary after an external tool updated it.
func locate(b Binary) string {
	if p, err := currentSystem().LookPath(b.Name); err == nil {
		if resolved, err := filepath.EvalSymlinks(p); err == nil {
			if abs, err := filepath.Abs(resolved); err == nil {
				return abs
			}
		}
	}
	return b.Path
}

// runExternal runs a package manager's fixed commands or the custom command,
// then verifies what it installed. It reports whether any binary changed.
func (s *Service) runExternal(ctx context.Context, release Release, job *Job, bins []Binary, opts ApplyOptions) (bool, error) {
	sys := currentSystem()
	before := map[string]string{}
	for _, b := range bins {
		before[b.Name], _ = fileHash(b.Path)
	}
	var output []byte
	if opts.Method == MethodCustom {
		if err := s.phase(job, "running_custom_command"); err != nil {
			return false, err
		}
		timeout := opts.CustomTimeout
		if timeout <= 0 {
			timeout = 30 * time.Minute
		}
		cctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		pairs := []string{}
		for _, b := range bins {
			pairs = append(pairs, b.Name+"="+b.Path)
		}
		sort.Strings(pairs)
		env := append(os.Environ(), "TCLAUDE_UPDATE_VERSION="+release.Tag, "TCLAUDE_UPDATE_CURRENT_VERSION="+job.CurrentVersion, "TCLAUDE_UPDATE_JOB_ID="+job.ID, "TCLAUDE_UPDATE_BINARIES="+strings.Join(pairs, ","))
		out, err := sys.Run(cctx, []string{"/bin/sh", "-c", opts.CustomCommand}, env)
		output = out
		job.Output = tail(output, 4096)
		if err != nil {
			return false, fmt.Errorf("custom update command failed: %w", err)
		}
	} else {
		if err := s.phase(job, "running_package_manager"); err != nil {
			return false, err
		}
		b := bins[0]
		for _, argv := range ManagerCommands(b.ManagedBy, b.Package) {
			out, err := sys.Run(ctx, argv, nil)
			output = append(output, out...)
			job.Output = tail(output, 4096)
			if err != nil {
				return false, fmt.Errorf("%s failed: %w", strings.Join(argv, " "), err)
			}
		}
	}
	if err := s.phase(job, "verifying"); err != nil {
		return false, err
	}
	changed := false
	for _, b := range bins {
		path := locate(b)
		hash, err := fileHash(path)
		if err != nil {
			return false, fmt.Errorf("%s is missing after the update: %w", b.Name, err)
		}
		if path == b.Path && hash == before[b.Name] {
			if opts.Method == MethodCustom {
				return false, fmt.Errorf("the custom update command left %s unchanged", b.Name)
			}
			job.Warnings = append(job.Warnings, fmt.Sprintf("%s has no newer tclaude than %s yet", b.ManagedBy, b.Version))
			continue
		}
		if err := s.identify(path); err != nil {
			return false, fmt.Errorf("%s at %s is not a tclaude binary after the update", b.Name, path)
		}
		info, err := s.inspect(ctx, path)
		if err != nil {
			return false, err
		}
		if info.ProtocolVersion != s.status.ProtocolVersion {
			return false, fmt.Errorf("%s now reports federation protocol %d, not %d; linked peers may be incompatible (the new binary is installed; restart agentd yourself once resolved)", b.Name, info.ProtocolVersion, s.status.ProtocolVersion)
		}
		switch {
		case info.Version == release.Tag:
		case opts.Method == MethodCustom:
			return false, fmt.Errorf("the custom update command installed %s %s, expected %s", b.Name, info.Version, release.Tag)
		case semver.IsValid(job.CurrentVersion) && semver.IsValid(info.Version) && semver.Compare(info.Version, job.CurrentVersion) < 0:
			return false, fmt.Errorf("%s downgraded %s to %s", b.ManagedBy, b.Name, info.Version)
		default:
			job.Warnings = append(job.Warnings, fmt.Sprintf("%s installed %s %s; the latest release is %s", b.ManagedBy, b.Name, info.Version, release.Tag))
		}
		if path != b.Path {
			job.Relocations = append(job.Relocations, Relocation{Name: b.Name, From: b.Path, To: path})
		}
		changed = true
	}
	return changed, nil
}

func stageCheckout(ctx context.Context, opts CheckoutOptions, tag, dir string) (map[string]string, error) {
	sys := currentSystem()
	git := func(args ...string) (string, error) {
		out, err := sys.Run(ctx, append([]string{"git", "-C", opts.Path}, args...), nil)
		return strings.TrimSpace(string(out)), err
	}
	status, err := git("status", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("%s is not a usable git checkout: %s", opts.Path, tail([]byte(status), 512))
	}
	if status != "" {
		return nil, fmt.Errorf("the checkout %s has uncommitted changes; commit or stash them first (tclaude never touches them)", opts.Path)
	}
	if out, err := git("fetch", "--tags", "--quiet", "origin"); err != nil {
		return nil, fmt.Errorf("git fetch failed: %s", tail([]byte(out), 512))
	}
	ref := opts.Ref
	if ref == "" {
		ref = tag
	}
	if out, err := git("checkout", "--quiet", ref); err != nil {
		return nil, fmt.Errorf("git checkout %s failed: %s", ref, tail([]byte(out), 512))
	}
	if opts.Ref != "" {
		if _, err := git("symbolic-ref", "-q", "HEAD"); err == nil {
			if out, err := git("merge", "--ff-only", "--quiet", "@{u}"); err != nil {
				return nil, fmt.Errorf("could not fast-forward %s: %s", ref, tail([]byte(out), 512))
			}
		}
	}
	env := append(filteredGoEnv(os.Environ()), "GOBIN="+dir, "GOWORK=off")
	if out, err := sys.Run(ctx, []string{"go", "-C", opts.Path, "install", ".", "./cmd/..."}, env); err != nil {
		return nil, fmt.Errorf("building the checkout failed: %s", tail(out, 1024))
	}
	built := map[string]string{}
	for name, pkg := range binaryPackages {
		path := filepath.Join(dir, name)
		bi, err := buildinfo.ReadFile(path)
		if err != nil {
			continue
		}
		if bi.Main.Path != module || bi.Path != pkg {
			return nil, fmt.Errorf("the checkout at %s does not build the tclaude module", opts.Path)
		}
		built[name] = path
	}
	return built, nil
}

// PendingPostSetup returns the completed job whose post-update setup has not
// run yet (or was interrupted).
func (s *Service) PendingPostSetup() *Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	j := s.status.Job
	if j == nil || j.State != "succeeded" || j.PostSetup == nil || j.PostSetup.State != "pending" && j.PostSetup.State != "running" {
		return nil
	}
	out := *j
	return &out
}

// RunPostSetup runs `<tclaude> <args>` for the pending job and records the
// exit code and bounded output on it.
func (s *Service) RunPostSetup(ctx context.Context, tclaude string) (Job, error) {
	s.mu.Lock()
	if s.status.Job == nil || s.status.Job.PostSetup == nil {
		s.mu.Unlock()
		return Job{}, fmt.Errorf("no pending post-update setup")
	}
	if s.active {
		s.mu.Unlock()
		return Job{}, ErrBusy
	}
	// No update job starts while the setup runs; its result belongs to
	// this job.
	s.active = true
	job := *s.status.Job
	setup := *job.PostSetup
	now := time.Now().UTC()
	setup.State, setup.StartedAt = "running", &now
	job.PostSetup = &setup
	err := s.saveJob(job)
	if err != nil {
		s.active = false
	}
	s.mu.Unlock()
	if err != nil {
		return job, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	out, runErr := currentSystem().Run(ctx, append([]string{tclaude}, setup.Args...), nil)
	finished := time.Now().UTC()
	code := 0
	if runErr != nil {
		code = -1
		var exit interface{ ExitCode() int }
		if errors.As(runErr, &exit) {
			code = exit.ExitCode()
		}
	}
	setup.ExitCode, setup.FinishedAt, setup.Output = &code, &finished, tail(out, 8192)
	setup.State = "succeeded"
	if runErr != nil {
		setup.State = "failed"
		if setup.Output == "" {
			setup.Output = runErr.Error()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active = false
	job.PostSetup = &setup
	return job, s.saveJob(job)
}

// RestartPath maps the running daemon's executable to where it now lives
// after the latest job relocated it.
func (s *Service) RestartPath(current string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status.Job == nil {
		return current
	}
	for _, r := range s.status.Job.Relocations {
		if r.From == current {
			return r.To
		}
	}
	return current
}

func identifyModule(path string) error {
	bi, err := buildinfo.ReadFile(path)
	if err != nil {
		return err
	}
	if bi.Main.Path != module {
		return fmt.Errorf("not the tclaude module")
	}
	return nil
}

// TestFakes replace the network, build and binary-inspection boundaries.
type TestFakes struct {
	Release func(context.Context, string) (Release, error)
	// Stage writes the named binary into dir for every staging method.
	Stage   func(ctx context.Context, release Release, name, dir string) (string, error)
	Inspect func(context.Context, string) (buildversion.Info, error)
}

// SetFakesForTest swaps the service's external boundaries; tests only.
func (s *Service) SetFakesForTest(f TestFakes) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f.Release != nil {
		s.release = f.Release
	}
	if f.Stage != nil {
		s.stageRelease = f.Stage
		s.stageGo = func(ctx context.Context, name, version, dir string) (string, error) {
			return f.Stage(ctx, Release{Tag: version}, name, dir)
		}
		s.stageCheckout = func(ctx context.Context, _ CheckoutOptions, tag, dir string) (map[string]string, error) {
			out := map[string]string{}
			for _, b := range s.binaries {
				p, err := f.Stage(ctx, Release{Tag: tag}, b.Name, dir)
				if err != nil {
					return nil, err
				}
				out[b.Name] = p
			}
			return out, nil
		}
	}
	if f.Inspect != nil {
		s.inspect = f.Inspect
	}
	s.identify = func(string) error { return nil }
}
