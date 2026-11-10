package selfupdate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/common/buildversion"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// fakeHost is a System whose PATH and commands are scripted per test.
type fakeHost struct {
	path map[string]string // name -> resolved path on PATH
	run  func(argv, env []string) ([]byte, error)
	ran  [][]string
}

func (h *fakeHost) install(t *testing.T) {
	t.Cleanup(SetSystemForTest(System{
		LookPath: func(name string) (string, error) {
			if p, ok := h.path[name]; ok {
				return p, nil
			}
			return "", errors.New("not found")
		},
		Run: func(_ context.Context, argv, env []string) ([]byte, error) {
			h.ran = append(h.ran, argv)
			if h.run == nil {
				return nil, nil
			}
			return h.run(argv, env)
		},
	}))
}

// versioned fakes report the version written into each binary file.
func versionedService(t *testing.T, dir string, bins []Binary, tag string) *Service {
	t.Helper()
	s, err := New(filepath.Join(dir, "updates"), bins, Hooks{})
	require.NoError(t, err)
	s.SetFakesForTest(TestFakes{
		Release: func(context.Context, string) (Release, error) { return Release{Tag: tag}, nil },
		Stage: func(_ context.Context, r Release, name, dir string) (string, error) {
			p := filepath.Join(dir, name)
			return p, os.WriteFile(p, []byte(r.Tag), 0o700)
		},
		Inspect: func(_ context.Context, path string) (buildversion.Info, error) {
			raw, err := os.ReadFile(path)
			return buildversion.Info{Version: strings.TrimSpace(string(raw)), ProtocolVersion: s.status.ProtocolVersion}, err
		},
	})
	return s
}

func writeBin(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o755))
}

func TestClassifyDetectsPackageManagersAndRecommendsManual(t *testing.T) {
	h := &fakeHost{path: map[string]string{"nix": "/bin/nix", "dpkg": "/bin/dpkg", "brew": "/bin/brew"}, run: func(argv, _ []string) ([]byte, error) {
		switch argv[0] {
		case "nix":
			return []byte(`{"elements":{"tclaude":{"storePaths":["/nix/store/abc-tclaude-1.0"]}}}`), nil
		case "dpkg":
			return []byte("tclaude: /usr/bin/tclaude"), nil
		}
		return nil, errors.New("unexpected")
	}}
	h.install(t)
	cases := map[string][2]string{
		"/opt/homebrew/Cellar/tclaude/1.0/bin/tclaude": {ManagerHomebrew, HomebrewFormula},
		"/nix/store/abc-tclaude-1.0/bin/tclaude":       {ManagerNixProfile, "tclaude"},
		"/nix/store/zzz-system/bin/tclaude":            {ManagerNix, ""},
		"/usr/bin/tclaude":                             {ManagerDpkg, "tclaude"},
		"/home/me/.asdf/installs/tclaude/1.0/bin/x":    {ManagerAsdf, ""},
		"/home/me/go/bin/tclaude":                      {"", ""},
	}
	for path, want := range cases {
		b := Binary{Name: "tclaude", Path: path, Method: "release", Version: "v1.0.0"}
		classify(&b)
		require.Equal(t, want[0], b.ManagedBy, path)
		require.Equal(t, want[1], b.Package, path)
	}
	brew := Binary{Name: "tclaude", Path: "/opt/homebrew/Cellar/tclaude/1.0/bin/tclaude", Method: "release", Version: "v1.0.0"}
	classify(&brew)
	d := Detect([]Binary{brew})
	require.Equal(t, MethodManual, d.Method, "running a package manager is never the recommendation")
	require.Equal(t, "brew upgrade "+HomebrewFormula, d.UpdateHint)
	require.Equal(t, "brew uninstall tclaude", d.UntrackHint)
	var hb MethodInfo
	for _, m := range Methods([]Binary{brew}) {
		if m.ID == MethodHomebrew {
			hb = m
		}
	}
	require.True(t, hb.Available && hb.PackageManager)
	require.Equal(t, "brew upgrade "+HomebrewFormula, hb.Command)

	unknown := Binary{Name: "tclaude", Path: "/opt/tools/tclaude", Method: "unrecognized", Evidence: "tclaude is not a readable Go binary"}
	d = Detect([]Binary{unknown})
	require.False(t, d.Recognized)
	require.Equal(t, MethodManual, d.Method)
	require.Contains(t, d.Reason, "not a readable Go binary")
}

func TestApplyNeverOverwritesPackageManagedBinaries(t *testing.T) {
	(&fakeHost{path: map[string]string{}}).install(t)
	dir := testutil.CanonicalTempDir(t)
	cellar := filepath.Join(dir, "Cellar", "tclaude", "1.0", "bin", "tclaude")
	writeBin(t, cellar, "v1.0.0")
	bins := []Binary{{Name: "tclaude", Path: cellar, Version: "v1.0.0", Method: "release", ManagedBy: ManagerHomebrew, Package: HomebrewFormula}}
	s := versionedService(t, dir, bins, "v1.1.0")
	allow := func() bool { return true }
	var managed *PackageManagedError
	_, err := s.StartWith(Request{Action: "apply"}, "operator", allow, ApplyOptions{})
	require.ErrorAs(t, err, &managed)
	require.False(t, managed.NeedsAck)
	require.Contains(t, err.Error(), "brew upgrade "+HomebrewFormula)
	_, err = s.StartWith(Request{Action: "apply"}, "operator", allow, ApplyOptions{Method: MethodRelease})
	require.ErrorAs(t, err, &managed)
	require.True(t, managed.NeedsAck)
	require.Contains(t, err.Error(), "brew uninstall tclaude")
	_, err = s.StartWith(Request{Action: "apply"}, "operator", allow, ApplyOptions{Method: MethodManual})
	require.ErrorIs(t, err, ErrNotifyOnly)

	_, err = s.StartWith(Request{Action: "apply"}, "operator", allow, ApplyOptions{Method: MethodRelease, PackageManagerAck: true, InstallDir: filepath.Dir(cellar)})
	require.ErrorContains(t, err, "install_dir", "never into the managed binary's own directory")

	// With the acknowledgement the update installs next to the Cellar copy.
	installDir := filepath.Join(dir, "home", ".local", "bin")
	job, err := s.StartWith(Request{Action: "apply"}, "operator", allow, ApplyOptions{Method: MethodRelease, PackageManagerAck: true, InstallDir: installDir})
	require.NoError(t, err)
	job = waitJob(t, s, job.ID)
	require.Equal(t, "restarting", job.State, job.Error)
	raw, err := os.ReadFile(cellar)
	require.NoError(t, err)
	require.Equal(t, "v1.0.0", string(raw), "the package manager's file is untouched")
	installed := filepath.Join(installDir, "tclaude")
	raw, err = os.ReadFile(installed)
	require.NoError(t, err)
	require.Equal(t, "v1.1.0", string(raw))
	require.Equal(t, []Relocation{{Name: "tclaude", From: cellar, To: installed}}, job.Relocations)
	require.Equal(t, installed, s.RestartPath(cellar))
	require.Contains(t, strings.Join(job.Warnings, "\n"), "not on PATH")

	// The restart verifies; applying the same release again changes nothing.
	s2 := versionedService(t, dir, bins, "v1.1.0")
	require.Equal(t, "succeeded", s2.Status().Job.State, s2.Status().Job.Error)
	again, err := s2.StartWith(Request{Action: "apply"}, "operator", allow, ApplyOptions{Method: MethodRelease, PackageManagerAck: true, InstallDir: installDir})
	require.NoError(t, err)
	again = waitJob(t, s2, again.ID)
	require.Equal(t, "succeeded", again.State, "an identical install needs no restart")
	require.True(t, s2.Status().RollbackAvailable, "a no-op keeps the previous backup")

	// Rollback removes only what tclaude created and restarts from the
	// package-managed binary again.
	job, err = s2.Start(Request{Action: "rollback"}, "operator", allow)
	require.NoError(t, err)
	require.Equal(t, "restarting", waitJob(t, s2, job.ID).State)
	_, err = os.Stat(installed)
	require.ErrorIs(t, err, os.ErrNotExist)
	require.Equal(t, cellar, s2.RestartPath(installed))
	_, err = os.Stat(cellar)
	require.NoError(t, err)
}

func TestPackageManagerMethodRunsFixedCommandAndVerifies(t *testing.T) {
	dir := testutil.CanonicalTempDir(t)
	old := filepath.Join(dir, "Cellar", "tclaude", "1.0", "bin", "tclaude")
	upgraded := filepath.Join(dir, "Cellar", "tclaude", "1.1", "bin", "tclaude")
	writeBin(t, old, "v1.0.0")
	h := &fakeHost{path: map[string]string{"tclaude": old}}
	h.run = func(argv, _ []string) ([]byte, error) {
		writeBin(t, upgraded, "v1.1.0")
		h.path["tclaude"] = upgraded
		return []byte("==> Upgrading tofutools/tap/tclaude"), nil
	}
	h.install(t)
	s := versionedService(t, dir, []Binary{{Name: "tclaude", Path: old, Version: "v1.0.0", Method: "release", ManagedBy: ManagerHomebrew, Package: HomebrewFormula}}, "v1.1.0")
	job, err := s.StartWith(Request{Action: "apply"}, "operator", func() bool { return true }, ApplyOptions{Method: MethodHomebrew, PostSetup: []string{"setup", "--yes"}})
	require.NoError(t, err)
	job = waitJob(t, s, job.ID)
	require.Equal(t, "restarting", job.State, job.Error)
	require.Equal(t, [][]string{{"brew", "upgrade", HomebrewFormula}}, h.ran)
	require.Contains(t, job.Output, "Upgrading")
	require.Equal(t, []Relocation{{Name: "tclaude", From: old, To: upgraded}}, job.Relocations)
	require.False(t, s.Status().RollbackAvailable, "a package-manager update keeps no tclaude backup")
	require.Equal(t, "pending", job.PostSetup.State)

	// The new daemon verifies the restart and runs the NEW binary's setup.
	h.ran = nil
	h.run = func(argv, _ []string) ([]byte, error) { return []byte("setup done"), nil }
	s2 := versionedService(t, dir, []Binary{{Name: "tclaude", Path: upgraded, Version: "v1.1.0", Method: "release", ManagedBy: ManagerHomebrew}}, "v1.1.0")
	require.NotNil(t, s2.PendingPostSetup())
	done, err := s2.RunPostSetup(context.Background(), upgraded)
	require.NoError(t, err)
	require.Equal(t, [][]string{{upgraded, "setup", "--yes"}}, h.ran)
	require.Equal(t, "succeeded", done.PostSetup.State)
	require.Equal(t, 0, *done.PostSetup.ExitCode)
	require.Equal(t, "setup done", done.PostSetup.Output)
	require.Nil(t, s2.PendingPostSetup(), "setup runs once")
}

// customService is a service with one tclaude binary at bin (reporting
// v1.0.0) whose latest release is v1.1.0; run is the custom command.
func customService(t *testing.T, run func(argv, env []string) ([]byte, error)) (*Service, *fakeHost, string) {
	t.Helper()
	dir := testutil.CanonicalTempDir(t)
	bin := filepath.Join(dir, "bin", "tclaude")
	writeBin(t, bin, "v1.0.0")
	h := &fakeHost{path: map[string]string{"tclaude": bin}, run: run}
	h.install(t)
	return versionedService(t, dir, []Binary{{Name: "tclaude", Path: bin, Version: "v1.0.0", Method: "unrecognized"}}, "v1.1.0"), h, bin
}

func applyCustom(t *testing.T, s *Service, opts ApplyOptions) Job {
	t.Helper()
	opts.Method, opts.CustomCommand = MethodCustom, "my-installer"
	job, err := s.StartWith(Request{Action: "apply"}, "operator", func() bool { return true }, opts)
	require.NoError(t, err)
	return waitJob(t, s, job.ID)
}

// The TCLAUDE_UPDATE_* variables are information: a custom command that
// builds a branch (a devel or lower-sorting version) succeeds, with a
// warning naming the latest release.
func TestCustomMethodAcceptsAnyVersionWithAWarning(t *testing.T) {
	var env []string
	var bin string
	s, h, bin := customService(t, func(argv, e []string) ([]byte, error) {
		env = e
		writeBin(t, bin, "v0.0.0-20261110120000-abcdef123456")
		return nil, nil
	})
	job := applyCustom(t, s, ApplyOptions{})
	require.Equal(t, "restarting", job.State, job.Error)
	require.Equal(t, []string{"/bin/sh", "-c", "my-installer"}, h.ran[0])
	require.Contains(t, env, "TCLAUDE_UPDATE_VERSION=v1.1.0")
	require.Contains(t, strings.Join(job.Warnings, "\n"), "installed tclaude v0.0.0-20261110120000-abcdef123456; the latest release is v1.1.0")
	require.True(t, s.Status().RollbackAvailable, "an in-place custom install is backed up")
}

// A custom command that changes nothing found nothing to install: the job
// succeeds as already up to date and nothing restarts.
func TestCustomMethodUnchangedBinaryIsUpToDate(t *testing.T) {
	s, _, _ := customService(t, func([]string, []string) ([]byte, error) { return []byte("Already up to date."), nil })
	job := applyCustom(t, s, ApplyOptions{})
	require.Equal(t, "succeeded", job.State, job.Error)
	require.False(t, job.RestartRequired)
	require.Contains(t, strings.Join(job.Warnings, "\n"), "already up to date")
	backups, err := filepath.Glob(filepath.Join(s.dir, "backup-*"))
	require.NoError(t, err)
	require.Empty(t, backups, "an up-to-date run keeps no backup copy")
}

// A new binary that speaks another federation protocol still fails.
func TestCustomMethodProtocolMismatchFails(t *testing.T) {
	var bin string
	s, _, bin := customService(t, func([]string, []string) ([]byte, error) { writeBin(t, bin, "v1.1.0"); return nil, nil })
	s.SetFakesForTest(TestFakes{Inspect: func(_ context.Context, path string) (buildversion.Info, error) {
		return buildversion.Info{Version: "v1.1.0", ProtocolVersion: s.status.ProtocolVersion + 1}, nil
	}})
	job := applyCustom(t, s, ApplyOptions{})
	require.Equal(t, "failed", job.State)
	require.Contains(t, job.Error, "federation protocol")
}

// At the latest release an automatic run skips the custom command; the
// operator's own Update now (RunWhenCurrent) runs it, since it may track a
// branch.
func TestCustomMethodRunsAtLatestOnlyWhenAsked(t *testing.T) {
	var bin string
	s, h, bin := customService(t, func([]string, []string) ([]byte, error) { return nil, nil })
	writeBin(t, bin, "v1.1.0")
	s.binaries[0].Version = "v1.1.0"
	job := applyCustom(t, s, ApplyOptions{})
	require.Equal(t, "succeeded", job.State, job.Error)
	require.Empty(t, h.ran, "an automatic run at the latest release runs nothing")
	job = applyCustom(t, s, ApplyOptions{RunWhenCurrent: true})
	require.Equal(t, "succeeded", job.State, job.Error)
	require.Equal(t, [][]string{{"/bin/sh", "-c", "my-installer"}}, h.ran)
}
