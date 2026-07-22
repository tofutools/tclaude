package harness

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// SandboxCapabilityReopenUnderDeny is the stable wire kind for refusing the
// reopen-under-deny SHAPE: a read/write grant strictly beneath a deny grant in
// the same effective profile. Every supported harness can enforce a plain deny;
// carving a narrower path back out of one relies on documented
// path-specificity semantics that only some harness/mode combinations provide.
const SandboxCapabilityReopenUnderDeny = "unsupported_sandbox_profile_reopen_under_deny"

// CodexSplitPolicyCapability binds a successful split-policy probe to the
// exact Codex executable that was measured. Strict-Home launches carry this
// identity through command construction rather than falling back to PATH.
type CodexSplitPolicyCapability struct {
	ExecutablePath           string
	ExecutableIdentity       string
	RequiresExecutableReopen bool
}

type codexSplitPolicyCapability = CodexSplitPolicyCapability

type codexSplitProbeCacheEntry struct {
	capability codexSplitPolicyCapability
	err        error
}

var codexSplitProbeCache = struct {
	sync.Mutex
	entries map[string]codexSplitProbeCacheEntry
}{entries: map[string]codexSplitProbeCacheEntry{}}

var runCodexSplitPolicyProbe = probeCodexSplitPolicy
var codexExecutableIdentityForProbe = codexExecutableIdentity
var sandboxRuntimeGOOS = runtime.GOOS

// describeReopens renders the offending shape so an operator can see exactly
// which rows to change, without dumping an unbounded rule list into an error.
func describeReopens(shapes []sandboxpolicy.ReopenUnderDeny) string {
	const maxShown = 3
	parts := make([]string, 0, maxShown)
	for i, shape := range shapes {
		if i == maxShown {
			parts = append(parts, fmt.Sprintf("and %d more", len(shapes)-maxShown))
			break
		}
		parts = append(parts, fmt.Sprintf("%s %q beneath deny %q", shape.Reopen.Access, shape.Reopen.Path, shape.Deny))
	}
	return strings.Join(parts, ", ")
}

// ValidateSandboxReopenUnderDeny gates the reopen-under-deny shape on a
// harness/mode combination that can actually enforce it. A profile with no such
// shape passes unconditionally: ordinary deny rows are enforceable everywhere.
// Storing the shape is always allowed; this check runs only at launch/resume.
func ValidateSandboxReopenUnderDeny(harnessName, sandboxMode string, grants []sandboxpolicy.FilesystemGrant) error {
	shapes := sandboxpolicy.ReopensUnderDeny(grants)
	if len(shapes) == 0 {
		return nil
	}
	detail := describeReopens(shapes)
	switch strings.TrimSpace(harnessName) {
	case DefaultName, "":
		// Claude Code documents that the more specific path wins when read rules
		// overlap (denyRead ["~/"] + allowRead ["."]), but only sandbox "on"
		// guarantees the deny AND the reopen are applied at all. An inherit/off
		// launch would quietly drop both.
		if strings.TrimSpace(sandboxMode) != ClaudeSandboxOn {
			return &SandboxCapabilityError{Harness: DefaultName, Kind: SandboxCapabilityReopenUnderDeny, Message: fmt.Sprintf(
				"reopening a path beneath a deny (%s) requires Claude sandbox %q; sandbox %q cannot guarantee the deny or the reopen is enforced",
				detail, ClaudeSandboxOn, sandboxMode)}
		}
		return nil
	case CodexName:
		if strings.TrimSpace(sandboxMode) != SandboxManagedProfile {
			return &SandboxCapabilityError{Harness: CodexName, Kind: SandboxCapabilityReopenUnderDeny, Message: fmt.Sprintf(
				"reopening a path beneath a deny (%s) requires Codex sandbox %q; raw sandbox %q cannot render the managed path policy",
				detail, SandboxManagedProfile, sandboxMode)}
		}
		if sandboxRuntimeGOOS != "linux" {
			return &SandboxCapabilityError{Harness: CodexName, Kind: SandboxCapabilityReopenUnderDeny, Message: fmt.Sprintf(
				"Codex can reopen a path beneath a deny (%s) only on Linux with verified split-policy bubblewrap behavior; on macOS a deny mask dominates any narrower reopen (openai/codex#21081), so the reopen would be silently discarded",
				detail)}
		}
		if _, err := verifiedCodexSplitPolicyCapability(); err != nil {
			return &SandboxCapabilityError{Harness: CodexName, Kind: SandboxCapabilityReopenUnderDeny, Message: fmt.Sprintf(
				"reopening a path beneath a deny (%s) could not verify the required Linux bubblewrap split policy in an isolated probe: %s",
				detail, sanitizeSplitProbeError(err))}
		}
		return nil
	default:
		return &SandboxCapabilityError{Harness: harnessName, Kind: SandboxCapabilityReopenUnderDeny, Message: fmt.Sprintf(
			"harness %q cannot reopen a path beneath a deny (%s)", harnessName, detail)}
	}
}

// ValidateSandboxBreakGlassWithReopenUnderDeny is the integration seam between
// the two gates. When Codex has already proven a verified split policy for the
// profile's reopen-under-deny shape, it can also reopen an acknowledged
// protected child while leaving that child's siblings masked — the behavior the
// conservative break-glass guard otherwise has to refuse. Every other shape
// retains that guard unchanged.
func ValidateSandboxBreakGlassWithReopenUnderDeny(harnessName, sandboxMode string, grants []sandboxpolicy.BreakGlassGrant, filesystem []sandboxpolicy.FilesystemGrant) error {
	if err := ValidateSandboxReopenUnderDeny(harnessName, sandboxMode, filesystem); err != nil {
		return err
	}
	if len(grants) == 0 {
		return nil
	}
	if strings.TrimSpace(harnessName) == CodexName && sandboxpolicy.HasReopenUnderDeny(filesystem) {
		// The validation above proved managed-profile + Linux + the behavioral
		// split probe for this executable. The ordinary protected denies remain
		// more-specific than the operator's broad deny; only these acknowledged
		// child rules reopen beneath them.
		return nil
	}
	return ValidateSandboxBreakGlass(harnessName, sandboxMode, grants)
}

func sanitizeSplitProbeError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "probe timed out"
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "bwrap"), strings.Contains(msg, "namespace"):
		return "bubblewrap/user namespaces are unavailable"
	case strings.Contains(msg, "legacy"), strings.Contains(msg, "landlock"):
		return "the required non-Landlock backend was not established"
	default:
		return "probe returned an unrecognized or failing result"
	}
}

func codexExecutableIdentity() (string, string, error) {
	path, err := exec.LookPath("codex")
	if err != nil {
		return "", "", err
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return "", "", err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", "", err
	}
	f, err := os.Open(path)
	if err != nil {
		return "", "", err
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, f)
	closeErr := f.Close()
	if copyErr != nil {
		return "", "", copyErr
	}
	if closeErr != nil {
		return "", "", closeErr
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	cmd.Env = []string{"HOME=/", "CODEX_HOME=/", "PATH=" + os.Getenv("PATH")}
	version, err := cmd.Output()
	if err != nil {
		return "", "", err
	}
	identity := strings.Join([]string{runtime.GOOS, runtime.GOARCH, path, fmt.Sprint(info.Size()), fmt.Sprint(info.ModTime().UnixNano()), hex.EncodeToString(hash.Sum(nil)), strings.TrimSpace(string(version)), "legacy-landlock=false"}, "\x00")
	return path, identity, nil
}

func verifiedCodexSplitPolicyCapability() (codexSplitPolicyCapability, error) {
	path, identity, err := codexExecutableIdentityForProbe()
	if err != nil {
		return codexSplitPolicyCapability{}, err
	}
	codexSplitProbeCache.Lock()
	if cached, ok := codexSplitProbeCache.entries[identity]; ok {
		codexSplitProbeCache.Unlock()
		return cached.capability, cached.err
	}
	codexSplitProbeCache.Unlock()
	capability, err := runCodexSplitPolicyProbe(path)
	if err == nil {
		capability.ExecutablePath = path
		capability.ExecutableIdentity = identity
	}
	codexSplitProbeCache.Lock()
	codexSplitProbeCache.entries[identity] = codexSplitProbeCacheEntry{capability: capability, err: err}
	codexSplitProbeCache.Unlock()
	return capability, err
}

// VerifyCodexHomeSplitPolicy returns the exact measured launch identity.
// Callers must revalidate it immediately before BuildCommand/exec.
func VerifyCodexHomeSplitPolicy() (CodexSplitPolicyCapability, error) {
	return verifiedCodexSplitPolicyCapability()
}

// RevalidateCodexHomeSplitPolicyCapability closes the probe-to-launch gap: it
// rejects an executable swap and reruns the behavioral probe against the exact
// path immediately before command construction, so backend availability/state
// is not trusted from the earlier cache hit.
func RevalidateCodexHomeSplitPolicyCapability(want CodexSplitPolicyCapability) error {
	path, identity, err := codexExecutableIdentityForProbe()
	if err != nil {
		return err
	}
	if path != want.ExecutablePath || identity != want.ExecutableIdentity {
		return fmt.Errorf("codex executable identity changed after split-policy verification")
	}
	got, err := runCodexSplitPolicyProbe(path)
	if err != nil {
		return fmt.Errorf("codex split-policy backend changed after verification: %w", err)
	}
	if got.RequiresExecutableReopen != want.RequiresExecutableReopen {
		return fmt.Errorf("codex split-policy executable-reopen behavior changed after verification")
	}
	return nil
}

// CodexHomeRuntimeReadPaths returns the exact executable leaf only when the
// isolated behavioral probe demonstrated that a sandboxed command cannot run
// without it but succeeds with that single reopen.
func CodexHomeRuntimeReadPaths() ([]string, error) {
	capability, err := verifiedCodexSplitPolicyCapability()
	if err != nil || !capability.RequiresExecutableReopen {
		return nil, err
	}
	return []string{capability.ExecutablePath}, nil
}

func probeCodexSplitPolicy(sourceBinary string) (codexSplitPolicyCapability, error) {
	root, err := os.MkdirTemp("", "tclaude-codex-split-probe-")
	if err != nil {
		return codexSplitPolicyCapability{}, err
	}
	defer func() { _ = os.RemoveAll(root) }()
	home := filepath.Join(root, "home")
	config := filepath.Join(root, "config")
	child := filepath.Join(home, "workspace")
	sibling := filepath.Join(home, "private")
	outsideBin := filepath.Join(root, "bin", "codex")
	insideBin := filepath.Join(home, ".codex", "runtime", "codex")
	for _, dir := range []string{config, child, sibling, filepath.Dir(outsideBin), filepath.Dir(insideBin)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return codexSplitPolicyCapability{}, err
		}
	}
	if err := os.WriteFile(filepath.Join(child, "allowed"), []byte("ok\n"), 0o600); err != nil {
		return codexSplitPolicyCapability{}, err
	}
	if err := os.WriteFile(filepath.Join(sibling, "secret"), []byte("secret\n"), 0o600); err != nil {
		return codexSplitPolicyCapability{}, err
	}
	for _, target := range []string{outsideBin, insideBin} {
		if err := stageProbeExecutable(sourceBinary, target); err != nil {
			return codexSplitPolicyCapability{}, fmt.Errorf("stage isolated executable: %w", err)
		}
	}
	run := func(binary string, reopenBinary bool) error {
		filesystem := fmt.Sprintf("%q = \"read\"\n%q = \"write\"\n%q = \"none\"\n%q = \"read\"\n", ":minimal", ":slash_tmp", home, child)
		if reopenBinary {
			filesystem += fmt.Sprintf("%q = \"read\"\n", binary)
		}
		profile := "default_permissions = \"split-probe\"\n\n[features]\nuse_legacy_landlock = false\n\n[permissions.split-probe]\n\n[permissions.split-probe.filesystem]\n" + filesystem + "\n[permissions.split-probe.network]\nenabled = false\n"
		if err := os.WriteFile(filepath.Join(config, "split-probe.config.toml"), []byte(profile), 0o600); err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		script := fmt.Sprintf("test \"$(cat %q)\" = ok && test ! -r %q && printf tclaude-split-policy-ok", filepath.Join(child, "allowed"), filepath.Join(sibling, "secret"))
		cmd := exec.CommandContext(ctx, binary, "sandbox", "-p", "split-probe", "-P", "split-probe", "-C", child, "--", "/bin/sh", "-c", script)
		cmd.Env = []string{"HOME=" + home, "CODEX_HOME=" + config, "PATH=" + os.Getenv("PATH"), "TMPDIR=" + root}
		output, runErr := cmd.CombinedOutput()
		if ctx.Err() != nil {
			return context.DeadlineExceeded
		}
		if runErr != nil {
			return fmt.Errorf("split probe failed: %w: %s", runErr, strings.TrimSpace(string(output)))
		}
		if !strings.Contains(string(output), "tclaude-split-policy-ok") {
			return fmt.Errorf("split probe returned unknown output")
		}
		return nil
	}
	// First prove the backend split itself with the runtime outside denied Home.
	if err := run(outsideBin, false); err != nil {
		return codexSplitPolicyCapability{}, err
	}
	// Then test the actual standalone shape. Prefer no reopen; add exactly the
	// executable leaf only when this executing fixture proves it is necessary.
	if err := run(insideBin, false); err == nil {
		return codexSplitPolicyCapability{}, nil
	}
	if err := run(insideBin, true); err != nil {
		return codexSplitPolicyCapability{}, err
	}
	return codexSplitPolicyCapability{RequiresExecutableReopen: true}, nil
}

func stageProbeExecutable(source, target string) error {
	if err := os.Link(source, target); err == nil {
		return nil
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}
