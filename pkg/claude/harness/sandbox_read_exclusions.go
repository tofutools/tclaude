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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

const SandboxCapabilityReadExclusions = "unsupported_sandbox_profile_read_exclusions"

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

func hasReadExclusion(ids []string, wanted string) bool {
	for _, id := range ids {
		if id == wanted {
			return true
		}
	}
	return false
}

// ValidateSandboxReadExclusions gates semantic Default-baseline exclusions on
// an adapter that can enforce every selected category. Portable intent is
// always storable; this check runs only at launch/resume.
func ValidateSandboxReadExclusions(harnessName, sandboxMode string, ids []string) error {
	ids, err := sandboxpolicy.NormalizeReadBaselineExclusions(ids)
	if err != nil || len(ids) == 0 {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return &SandboxCapabilityError{Harness: harnessName, Kind: SandboxCapabilityReadExclusions, Message: "cannot resolve the current home directory required by read exclusions"}
	}
	_, unknown, err := sandboxpolicy.ReadExclusionDenyPathsForOS(ids, home, sandboxRuntimeGOOS)
	if err != nil {
		return err
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return &SandboxCapabilityError{Harness: harnessName, Kind: SandboxCapabilityReadExclusions, Message: fmt.Sprintf("read exclusion IDs %s are unknown on this tclaude/platform; upgrade tclaude or remove them before launch", strings.Join(unknown, ", "))}
	}
	switch strings.TrimSpace(harnessName) {
	case DefaultName, "":
		if strings.TrimSpace(sandboxMode) != ClaudeSandboxOn {
			return &SandboxCapabilityError{Harness: DefaultName, Kind: SandboxCapabilityReadExclusions, Message: fmt.Sprintf("read exclusions require Claude sandbox %q; sandbox %q cannot guarantee the denies or managed reopens", ClaudeSandboxOn, sandboxMode)}
		}
		return nil
	case CodexName:
		if strings.TrimSpace(sandboxMode) != SandboxManagedProfile {
			return &SandboxCapabilityError{Harness: CodexName, Kind: SandboxCapabilityReadExclusions, Message: fmt.Sprintf("read exclusions require Codex sandbox %q; raw sandbox %q cannot render the managed path policy", SandboxManagedProfile, sandboxMode)}
		}
		if !hasReadExclusion(ids, sandboxpolicy.ReadExclusionHome) {
			return nil
		}
		if sandboxRuntimeGOOS != "linux" {
			return &SandboxCapabilityError{Harness: CodexName, Kind: SandboxCapabilityReadExclusions, Message: "Codex Home exclusion is supported only on Linux with verified split-policy bubblewrap behavior; Codex macOS currently masks narrower reopens beneath a denied Home directory (openai/codex#21081)"}
		}
		_, err := verifiedCodexSplitPolicyCapability()
		if err != nil {
			return &SandboxCapabilityError{Harness: CodexName, Kind: SandboxCapabilityReadExclusions, Message: "Codex Home exclusion could not verify the required Linux bubblewrap split policy in an isolated probe: " + sanitizeSplitProbeError(err)}
		}
		return nil
	default:
		return &SandboxCapabilityError{Harness: harnessName, Kind: SandboxCapabilityReadExclusions, Message: fmt.Sprintf("harness %q cannot enforce sandbox read exclusions", harnessName)}
	}
}

// ValidateSandboxBreakGlassWithReadExclusions is the narrow integration seam
// for Home+break-glass. A verified Linux Home split policy can reopen an
// acknowledged protected child while leaving siblings masked. Other Codex
// shapes retain the existing conservative break-glass guard.
func ValidateSandboxBreakGlassWithReadExclusions(harnessName, sandboxMode string, grants []sandboxpolicy.BreakGlassGrant, exclusions []string) error {
	if err := ValidateSandboxReadExclusions(harnessName, sandboxMode, exclusions); err != nil {
		return err
	}
	if len(grants) == 0 {
		return nil
	}
	if strings.TrimSpace(harnessName) == CodexName && hasReadExclusion(exclusions, sandboxpolicy.ReadExclusionHome) {
		// Validation above proved managed-profile + Linux + the behavioral
		// split probe. The ordinary protected denies remain more-specific than
		// Home; only these acknowledged child rules reopen beneath them.
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
