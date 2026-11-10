package selfupdate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func planCheck(t *testing.T, p Plan, name string) PlanCheck {
	t.Helper()
	for _, c := range p.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("plan has no %q check: %+v", name, p.Checks)
	return PlanCheck{}
}

// A release plan names the archive, probes the binary's directory and runs
// nothing.
func TestPlanReleaseProbesTheInstallDir(t *testing.T) {
	if _, err := archiveName("tclaude"); err != nil {
		t.Skip("no release artifact for this platform")
	}
	dir := testutil.CanonicalTempDir(t)
	bin := filepath.Join(dir, "bin", "tclaude")
	writeBin(t, bin, "v1.0.0")
	h := &fakeHost{path: map[string]string{}}
	h.install(t)
	s := versionedService(t, dir, []Binary{{Name: "tclaude", Path: bin, Version: "v1.0.0", Method: MethodRelease}}, "v1.1.0")
	p := s.Plan(context.Background(), ApplyOptions{Method: MethodRelease}, PlanOptions{})
	require.True(t, p.Ready, "%+v", p.Checks)
	require.Equal(t, planOK, planCheck(t, p, "tclaude").State)
	require.Contains(t, strings.Join(p.Steps, "\n"), downloadRoot)
	require.Empty(t, h.ran, "a plan runs nothing")
	entries, err := os.ReadDir(filepath.Dir(bin))
	require.NoError(t, err)
	require.Len(t, entries, 1, "the write probe leaves nothing behind")

	require.NoError(t, os.Chmod(filepath.Dir(bin), 0o555))
	t.Cleanup(func() { _ = os.Chmod(filepath.Dir(bin), 0o755) })
	if f, err := os.CreateTemp(filepath.Dir(bin), "root-"); err == nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		t.Skip("running as root: a read-only dir is still writable")
	}
	p = s.Plan(context.Background(), ApplyOptions{Method: MethodRelease}, PlanOptions{})
	require.False(t, p.Ready)
	require.Equal(t, planFail, planCheck(t, p, "tclaude").State)
}

// go install needs go on PATH; the plan says so instead of failing late.
func TestPlanGoInstallNeedsGo(t *testing.T) {
	dir := testutil.CanonicalTempDir(t)
	bin := filepath.Join(dir, "bin", "tclaude")
	writeBin(t, bin, "v1.0.0")
	(&fakeHost{path: map[string]string{}}).install(t)
	s := versionedService(t, dir, []Binary{{Name: "tclaude", Path: bin, Version: "v1.0.0", Method: MethodGoInstall}}, "v1.1.0")
	p := s.Plan(context.Background(), ApplyOptions{Method: MethodGoInstall}, PlanOptions{})
	require.False(t, p.Ready)
	require.Equal(t, planFail, planCheck(t, p, "Method").State)
	require.Equal(t, planFail, planCheck(t, p, "Go toolchain").State)
}

// A checkout with uncommitted changes fails the plan, as the update would.
func TestPlanSourceCheckoutChecksGitStatus(t *testing.T) {
	dir := testutil.CanonicalTempDir(t)
	bin := filepath.Join(dir, "bin", "tclaude")
	writeBin(t, bin, "v1.0.0")
	checkout := filepath.Join(dir, "src")
	require.NoError(t, os.MkdirAll(checkout, 0o755))
	dirty := " M main.go\n"
	h := &fakeHost{path: map[string]string{"go": "/bin/go", "git": "/bin/git"}, run: func(argv, _ []string) ([]byte, error) {
		return []byte(dirty), nil
	}}
	h.install(t)
	s := versionedService(t, dir, []Binary{{Name: "tclaude", Path: bin, Version: "v1.0.0", Method: "source"}}, "v1.1.0")
	opts := ApplyOptions{Method: MethodSourceCheckout, Checkout: CheckoutOptions{Path: checkout, Ref: "main"}}
	p := s.Plan(context.Background(), opts, PlanOptions{})
	require.False(t, p.Ready)
	require.Contains(t, planCheck(t, p, "Checkout").Detail, "uncommitted changes")
	require.Equal(t, [][]string{{"git", "-C", checkout, "status", "--porcelain"}}, h.ran, "only git status runs")

	dirty = ""
	p = s.Plan(context.Background(), opts, PlanOptions{})
	require.True(t, p.Ready, "%+v", p.Checks)
	require.Contains(t, strings.Join(p.Steps, "\n"), "git checkout main")
}

// The custom command shows to the operator, is withheld from a peer, and
// is never run.
func TestPlanCustomCommandIsShownOrWithheld(t *testing.T) {
	s, h, _ := customService(t, nil)
	opts := ApplyOptions{Method: MethodCustom, CustomCommand: "cd ~/src && go install ."}
	p := s.Plan(context.Background(), opts, PlanOptions{})
	require.True(t, p.Ready, "%+v", p.Checks)
	require.Contains(t, strings.Join(p.Steps, "\n"), "cd ~/src && go install .")
	p = s.Plan(context.Background(), opts, PlanOptions{RedactCommand: true})
	require.NotContains(t, strings.Join(p.Steps, "\n"), "go install")
	require.Empty(t, h.ran)

	p = s.Plan(context.Background(), ApplyOptions{Method: MethodCustom}, PlanOptions{})
	require.False(t, p.Ready)
	require.Equal(t, planFail, planCheck(t, p, "Command").State)
}

// A refusal the apply would give (a package-managed binary, no ack) fails
// the plan with the same message.
func TestPlanReportsTheApplyRefusal(t *testing.T) {
	dir := testutil.CanonicalTempDir(t)
	bin := filepath.Join(dir, "bin", "tclaude")
	writeBin(t, bin, "v1.0.0")
	(&fakeHost{path: map[string]string{}}).install(t)
	s := versionedService(t, dir, []Binary{{Name: "tclaude", Path: bin, Version: "v1.0.0", Method: MethodRelease, ManagedBy: ManagerHomebrew, Package: HomebrewFormula}}, "v1.1.0")
	p := s.Plan(context.Background(), ApplyOptions{Method: MethodRelease}, PlanOptions{})
	require.False(t, p.Ready)
	require.Contains(t, planCheck(t, p, "Allowed").Detail, "acknowledge")

	p = s.Plan(context.Background(), ApplyOptions{Method: MethodManual}, PlanOptions{})
	require.True(t, p.Ready)
	require.Contains(t, planCheck(t, p, "Install").Detail, "installs nothing")
}
