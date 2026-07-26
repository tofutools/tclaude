package session

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// TclaudeLayerLaunchContract carries writable paths required by the launched
// harness itself rather than granted by the operator's sandbox profile.
type TclaudeLayerLaunchContract struct {
	HarnessName       string
	WriteDirs         []string
	ProfileFilesystem []sandboxpolicy.FilesystemGrant
}

// ResolveTclaudeLayer verifies the host capability before a launch is
// committed. Callers record the returned verdict even when verification fails.
func ResolveTclaudeLayer(posture sandboxpolicy.NetworkPosture) (string, harness.LaunchOSSandbox, error) {
	binary, err := resolveBwrapBinary(posture)
	if err != nil {
		return "", harness.LaunchOSSandbox{
			State:  "off",
			Source: "tclaude-layer unavailable",
		}, err
	}
	return binary, TclaudeLayerLaunchOSSandbox(posture), nil
}

// TclaudeLayerHostAvailability reports whether THIS HOST can create the
// baseline tclaude-layer boundary: bubblewrap on Linux or Seatbelt on macOS.
// nil means available. The returned error names the concrete missing
// capability.
//
// It shares one predicate with the launch boundary — both call
// resolveBwrapBinary — so a pre-flight answer can never disagree with the
// refusal that actually decides. What differs is only the posture, and
// deliberately: this probes the LEAST demanding posture (host-open), because
// the posture a launch really uses derives from the resolved sandbox profile's
// network_access, and isolated-with-agentd needs strictly more (network + PID
// namespaces). So a nil result here rules out an obviously-doomed launch; it
// never claims an isolated launch will succeed. The posture-exact check at the
// launch boundary remains the authority.
//
// Callers that REFUSE on this answer must call it live rather than caching:
// an operator who has just installed bwrap must not be refused by a stale
// negative. Caching is for disclosure only. See TCL-769.
func TclaudeLayerHostAvailability() error {
	_, err := resolveBwrapBinary(sandboxpolicy.NetworkHostOpen)
	return err
}

// TclaudeLayerLaunchOSSandbox records the resolved platform/posture boundary.
// Partial host-open implementations stay visibly unverified; a constructed
// isolated root can report the stronger boundary it actually enforces.
func TclaudeLayerLaunchOSSandbox(posture sandboxpolicy.NetworkPosture) harness.LaunchOSSandbox {
	return tclaudeLayerLaunchOSSandbox(posture)
}

// ValidateTclaudeLayerNetwork refuses an isolated whole-process launch unless
// both the harness descriptor and the operator's resolved profile assert a
// model transport that functions from inside the new network namespace.
func ValidateTclaudeLayerNetwork(h *harness.Harness, effective sandboxpolicy.EffectiveProfile) error {
	posture, err := sandboxpolicy.NetworkPostureForAccess(effective.NetworkAccess)
	if err != nil {
		return err
	}
	if posture != sandboxpolicy.NetworkIsolatedWithAgentd {
		return nil
	}
	if !h.SupportsOfflineModelTransport() {
		return fmt.Errorf(
			"unsupported_sandbox_profile_network: network_access none isolates the whole tclaude-layer process, but harness %q requires hosted model traffic; see docs/sandboxing.md#isolated-with-agentd-network-posture",
			h.Name,
		)
	}
	for _, entry := range effective.Environment {
		if entry.Name == sandboxpolicy.OfflineModelTransportEnv && entry.Value == "1" {
			return nil
		}
	}
	return fmt.Errorf(
		"unsupported_sandbox_profile_network: network_access none requires %s=1 in the resolved sandbox profile, asserting a model transport that functions inside the isolated namespace; see docs/sandboxing.md#isolated-with-agentd-network-posture",
		sandboxpolicy.OfflineModelTransportEnv,
	)
}

// WrapTclaudeLayer renders one effective profile and applies the resulting
// mount plan around the complete harness command.
func WrapTclaudeLayer(
	binary string,
	effective sandboxpolicy.EffectiveProfile,
	contract TclaudeLayerLaunchContract,
	harnessCommand string,
) (string, error) {
	plan, err := sandboxpolicy.RenderMountPlan(effective)
	if err != nil {
		return "", fmt.Errorf("render mount plan: %w", err)
	}
	phase0WriteDirs, err := tclaudeLayerPhase0WriteDirs(contract, effective)
	if err != nil {
		return "", err
	}
	if err := validateTclaudeLayerHarnessStateRules(
		phase0WriteDirs[0],
		contract.ProfileFilesystem,
	); err != nil {
		return "", err
	}
	breakGlassPaths := make([]string, 0, len(effective.BreakGlassFilesystem))
	for _, grant := range effective.BreakGlassFilesystem {
		breakGlassPaths = append(breakGlassPaths, grant.Path)
	}
	return tclaudeLayerCommand(binary, phase0WriteDirs, breakGlassPaths, plan, harnessCommand)
}

// bwrapArgs applies the launch contract, ordered MountPlan, and fixed
// bubblewrap hygiene. Policy/profile interpretation belongs before this
// boundary.
//
// The four precedence classes are established at their normal positions in
// the argument stream, but read-only hardening of class-2 and class-3 hides is
// deferred until plan replay finishes. Bubblewrap creates missing bind
// destinations as it executes filesystem arguments, so remounting an ancestor
// hide immediately would prevent narrower launch-contract, break-glass, and
// most-specific-wins mounts from landing. The deferred tracker records whether
// a hide is still the topmost mount at its exact path; a later exact bind
// cancels its pending remount, while child mounts survive because
// --remount-ro is non-recursive. The isolated posture's constructed tmpfs root
// joins the same flush so unbound paths cannot accept throwaway writes. Class 4
// is materialized and hardened only after that flush, so host control remains
// the final, unshadowable phase.
func bwrapArgs(
	phase0WriteDirs, breakGlassPaths []string,
	plan sandboxpolicy.MountPlan,
) ([]string, error) {
	var hideRemounts tclaudeLayerHideRemounts
	args := []string{
		"--die-with-parent",
		// Give the wrapped process a new terminal session so a copied tmux
		// client or TIOCSTI-capable process cannot inject input into the host
		// pane shell outside this namespace.
		"--new-session",
	}
	switch plan.NetworkPosture {
	case sandboxpolicy.NetworkHostOpen:
		// Preserve the walking skeleton: the host namespace and read-only host
		// root stay visible, including ambient pathname sockets.
		args = append(args, "--ro-bind", "/", "/")
	case sandboxpolicy.NetworkIsolatedWithAgentd:
		// Bubblewrap brings loopback up in the newly created namespace. Start
		// from a fresh root so filesystem AF_UNIX sockets are absent unless a
		// later launch-contract or policy bind explicitly exposes them. Keep
		// bubblewrap as PID 1 (do not use --as-pid-1) so it reaps orphaned
		// harness subprocesses and host processes cannot reopen the host mount
		// namespace through /proc/<pid>/root.
		args = append(args, "--unshare-net", "--unshare-pid")
		args = hideRemounts.appendHide(args, "/")
	case sandboxpolicy.NetworkFiltered:
		return nil, fmt.Errorf("network posture %s is reserved and has no tclaude-layer applier", plan.NetworkPosture)
	default:
		return nil, fmt.Errorf("mount plan has invalid network posture %d", plan.NetworkPosture)
	}
	args = append(args,
		"--dev", "/dev",
		"--proc", "/proc",
		// Never share the host's scratch directory by default.
		"--tmpfs", "/tmp",
	)
	if plan.NetworkPosture == sandboxpolicy.NetworkIsolatedWithAgentd {
		var err error
		args, err = appendTclaudeLayerAliases(args, plan.Aliases)
		if err != nil {
			return nil, err
		}
		args, err = appendTclaudeLayerStaticOSRoot(args)
		if err != nil {
			return nil, err
		}
	}
	// Class 1 baseline: the harness process itself runs inside this wall,
	// unlike the harness-native sandboxes which confine only tools. Reopen only its
	// launch-contract state/workspace paths here; protected children are hidden
	// on top next. An ordinary ancestor hide triggers a narrower repair during
	// plan replay.
	existingPhase0WriteDirs := make([]string, 0, len(phase0WriteDirs))
	for i, path := range phase0WriteDirs {
		path = filepath.Clean(path)
		if path == "." || !filepath.IsAbs(path) {
			return nil, fmt.Errorf("launch-contract write entry %d has non-absolute path %q", i, path)
		}
		exists, err := bwrapBindSourceExists(path)
		if err != nil {
			return nil, fmt.Errorf("launch-contract write entry %d source %q: %w", i, path, err)
		}
		if exists {
			args = append(args, "--bind", path, path)
			existingPhase0WriteDirs = append(existingPhase0WriteDirs, path)
		}
	}
	// Class 3 baseline: protected state is hidden after the harness root is
	// writable.
	// EffectiveProfile deliberately excludes the protected-root baseline:
	// ordinary rules may never name these paths, while acknowledged
	// break-glass reopens arrive later in the plan. Establish the hides first
	// so normal launches cannot read private control/harness state and the
	// ordered plan can still reopen exactly what was acknowledged.
	protectedRoots, err := sandboxpolicy.ProtectedPaths()
	if err != nil {
		return nil, fmt.Errorf("resolve protected sandbox roots: %w", err)
	}
	for _, writeDir := range phase0WriteDirs {
		for _, protected := range protectedRoots {
			if sandboxpolicy.PathContainsOrEqual(protected, writeDir) {
				return nil, fmt.Errorf(
					"tclaude-layer launch-contract path %q is at or below protected root %q",
					writeDir,
					protected,
				)
			}
		}
	}
	for _, root := range protectedRoots {
		args = hideRemounts.appendHide(args, root)
	}
	socketPaths := []string(nil)
	if plan.NetworkPosture == sandboxpolicy.NetworkIsolatedWithAgentd {
		socket := agentipc.CanonicalSocketPath()
		if socket == "" || !filepath.IsAbs(socket) {
			return nil, fmt.Errorf("resolve canonical agentd socket for isolated tclaude-layer")
		}
		exists, err := bwrapBindSourceExists(socket)
		if err != nil {
			return nil, fmt.Errorf("agentd socket source %q: %w", socket, err)
		}
		if !exists {
			return nil, fmt.Errorf("isolated tclaude-layer requires the canonical agentd socket %s", socket)
		}
		args = append(args, "--ro-bind", socket, socket)
		socketPaths = append(socketPaths, socket)
	}
	breakGlass := make(map[string]bool, len(breakGlassPaths))
	for _, path := range breakGlassPaths {
		breakGlass[filepath.Clean(path)] = true
	}
	// Class 2: replay the policy plan exactly as rendered. Repair mounts do not
	// reorder or deduplicate plan entries; they preserve the higher-precedence
	// launch contract after an ordinary ancestor hide.
	for i, entry := range plan.Entries {
		path := filepath.Clean(entry.Path)
		if path == "." || !filepath.IsAbs(path) {
			return nil, fmt.Errorf("mount plan entry %d has non-absolute path %q", i, entry.Path)
		}
		switch entry.Mode {
		case sandboxpolicy.MountRO:
			exists, err := bwrapBindSourceExists(path)
			if err != nil {
				return nil, fmt.Errorf("mount plan entry %d source %q: %w", i, path, err)
			}
			if !exists {
				continue
			}
			hideRemounts.noteReplacement(path)
			args = append(args, "--ro-bind", path, path)
			if !breakGlass[path] {
				args = appendTclaudeLayerProtectedRehides(
					args,
					path,
					protectedRoots,
					&hideRemounts,
				)
			}
		case sandboxpolicy.MountRW:
			exists, err := bwrapBindSourceExists(path)
			if err != nil {
				return nil, fmt.Errorf("mount plan entry %d source %q: %w", i, path, err)
			}
			if !exists {
				continue
			}
			hideRemounts.noteReplacement(path)
			args = append(args, "--bind", path, path)
			if !breakGlass[path] {
				args = appendTclaudeLayerProtectedRehides(
					args,
					path,
					protectedRoots,
					&hideRemounts,
				)
			}
		case sandboxpolicy.MountHide:
			args = hideRemounts.appendHide(args, path)
			if plan.NetworkPosture == sandboxpolicy.NetworkIsolatedWithAgentd {
				args = appendTclaudeLayerAliasRepairs(
					args,
					path,
					plan.Aliases,
					protectedRoots,
					&hideRemounts,
				)
			}
			args = appendTclaudeLayerContractRepairs(
				args,
				path,
				existingPhase0WriteDirs,
				protectedRoots,
				&hideRemounts,
			)
			args = appendTclaudeLayerSocketRepairs(
				args,
				path,
				socketPaths,
				&hideRemounts,
			)
		default:
			return nil, fmt.Errorf("mount plan entry %d has invalid mode %d", i, entry.Mode)
		}
	}
	// Class 4: host tmux control is never profile- or break-glass-reachable.
	// This hide must be last: unlike ProtectedPaths, an ordinary profile may
	// grant a parent of the socket directory, and a later bind would otherwise
	// reopen the host tmux server.
	tmuxSocketDir, err := clcommon.TclaudeTmuxSocketDir()
	if err != nil {
		return nil, fmt.Errorf("resolve tclaude tmux socket directory: %w", err)
	}
	// If an ordinary ancestor hide is pending, create the class-4 mountpoint
	// while that tmpfs is still mutable. No wrapped process runs until all
	// setup operations finish, and the final tmpfs below still supplies the
	// unshadowable host-control view.
	if hideRemounts.hasActiveAncestor(tmuxSocketDir) {
		args = append(args, "--dir", tmuxSocketDir)
	}
	args = hideRemounts.appendRemounts(args)
	args = append(args,
		"--tmpfs", tmuxSocketDir,
		"--remount-ro", tmuxSocketDir,
	)
	return args, nil
}

// tclaudeLayerHideRemounts tracks the topmost exact-path operation without
// disturbing MountPlan order. The first sighting fixes deterministic flush
// order; later hide/replacement operations only update which mount currently
// owns that path.
type tclaudeLayerHideRemounts struct {
	order  []string
	active map[string]bool
}

func (r *tclaudeLayerHideRemounts) appendHide(args []string, path string) []string {
	if r.active == nil {
		r.active = make(map[string]bool)
	}
	r.noteAncestorReplacement(path)
	if _, seen := r.active[path]; !seen {
		r.order = append(r.order, path)
	}
	r.active[path] = true
	return append(args, "--tmpfs", path)
}

func (r *tclaudeLayerHideRemounts) noteReplacement(path string) {
	r.noteAncestorReplacement(path)
	if _, tracked := r.active[path]; tracked {
		r.active[path] = false
	}
}

// noteAncestorReplacement deactivates child hides shadowed by a later mount.
// A protected re-hide emitted after the replacement will reactivate its exact
// path; a child left covered only by the new ancestor is not itself a visible
// mountpoint and must not receive --remount-ro at flush time.
func (r *tclaudeLayerHideRemounts) noteAncestorReplacement(path string) {
	for candidate, active := range r.active {
		if active && candidate != path && sandboxpolicy.PathContainsOrEqual(path, candidate) {
			r.active[candidate] = false
		}
	}
}

func (r *tclaudeLayerHideRemounts) hasActiveAncestor(path string) bool {
	for candidate, active := range r.active {
		if active && sandboxpolicy.PathContainsOrEqual(candidate, path) {
			return true
		}
	}
	return false
}

func (r *tclaudeLayerHideRemounts) appendRemounts(args []string) []string {
	for _, path := range r.order {
		if r.active[path] {
			args = append(args, "--remount-ro", path)
		}
	}
	return args
}

var tclaudeLayerStaticOSPaths = []string{
	"/usr",
	"/bin",
	"/sbin",
	"/lib",
	"/lib64",
	"/lib32",
	"/libx32",
	"/etc",
	"/opt",
}

// appendTclaudeLayerStaticOSRoot constructs the fixed executable/runtime
// surface approved for the isolated posture. Merged-usr aliases remain
// symlinks instead of becoming separate recursive host binds.
func appendTclaudeLayerStaticOSRoot(args []string) ([]string, error) {
	for _, path := range tclaudeLayerStaticOSPaths {
		info, err := os.Lstat(path)
		switch {
		case err == nil:
		case os.IsNotExist(err):
			continue
		default:
			return nil, fmt.Errorf("inspect isolated-root path %q: %w", path, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return nil, fmt.Errorf("read isolated-root symlink %q: %w", path, err)
			}
			args = append(args, "--symlink", target, path)
			continue
		}
		args = append(args, "--ro-bind", path, path)
	}
	return args, nil
}

func appendTclaudeLayerAliases(
	args []string,
	aliases []sandboxpolicy.MountAlias,
) ([]string, error) {
	for i, alias := range aliases {
		link := filepath.Clean(alias.Link)
		target := filepath.Clean(alias.Target)
		if link == "." || !filepath.IsAbs(link) || link == string(filepath.Separator) {
			return nil, fmt.Errorf("mount alias %d has invalid link %q", i, alias.Link)
		}
		if target == "." || !filepath.IsAbs(target) {
			return nil, fmt.Errorf("mount alias %d has non-absolute target %q", i, alias.Target)
		}
		if tclaudeLayerStaticOSRootProvides(link) {
			continue
		}
		args = append(args, "--symlink", target, link)
	}
	return args, nil
}

func tclaudeLayerStaticOSRootProvides(path string) bool {
	for _, static := range tclaudeLayerStaticOSPaths {
		if sandboxpolicy.PathContainsOrEqual(static, path) {
			return true
		}
	}
	return false
}

func appendTclaudeLayerAliasRepairs(
	args []string,
	hide string,
	aliases []sandboxpolicy.MountAlias,
	protectedRoots []string,
	hideRemounts *tclaudeLayerHideRemounts,
) []string {
	for _, alias := range aliases {
		link := filepath.Clean(alias.Link)
		if hide == link || !sandboxpolicy.PathContainsOrEqual(hide, link) {
			continue
		}
		args = append(args, "--symlink", filepath.Clean(alias.Target), link)
		args = appendTclaudeLayerProtectedRehides(
			args,
			link,
			protectedRoots,
			hideRemounts,
		)
	}
	return args
}

func appendTclaudeLayerSocketRepairs(
	args []string,
	hide string,
	socketPaths []string,
	hideRemounts *tclaudeLayerHideRemounts,
) []string {
	for _, socket := range socketPaths {
		if hide != socket && sandboxpolicy.PathContainsOrEqual(hide, socket) {
			hideRemounts.noteReplacement(socket)
			args = append(args, "--ro-bind", socket, socket)
		}
	}
	return args
}

func appendTclaudeLayerProtectedRehides(
	args []string,
	mountedPath string,
	protectedRoots []string,
	hideRemounts *tclaudeLayerHideRemounts,
) []string {
	for _, protected := range protectedRoots {
		if sandboxpolicy.PathContainsOrEqual(mountedPath, protected) ||
			sandboxpolicy.PathContainsOrEqual(protected, mountedPath) {
			args = hideRemounts.appendHide(args, protected)
		}
	}
	return args
}

func appendTclaudeLayerContractRepairs(
	args []string,
	hide string,
	existingWriteDirs, protectedRoots []string,
	hideRemounts *tclaudeLayerHideRemounts,
) []string {
	repaired := make([]string, 0, len(existingWriteDirs))
	for _, writeDir := range existingWriteDirs {
		// An exact or narrower policy row wins. The launch seam refuses an
		// ordinary profile row at/below the harness state root separately;
		// workspace and agent-directory rows retain normal plan precedence.
		if hide != writeDir && sandboxpolicy.PathContainsOrEqual(hide, writeDir) {
			hideRemounts.noteReplacement(writeDir)
			args = append(args, "--bind", writeDir, writeDir)
			repaired = append(repaired, writeDir)
		}
	}
	// A repaired parent bind would cover an earlier protected child hide.
	// Restore every overlapping protected root after all repairs so protected
	// state still outranks launch-contract authority. A later break-glass plan
	// entry remains able to reopen its acknowledged path.
	for _, protected := range protectedRoots {
		for _, writeDir := range repaired {
			if sandboxpolicy.PathContainsOrEqual(writeDir, protected) ||
				sandboxpolicy.PathContainsOrEqual(protected, writeDir) {
				args = hideRemounts.appendHide(args, protected)
				break
			}
		}
	}
	return args
}

func validateTclaudeLayerHarnessStateRules(
	stateRoot string,
	profileFilesystem []sandboxpolicy.FilesystemGrant,
) error {
	for _, grant := range profileFilesystem {
		if sandboxpolicy.PathContainsOrEqual(stateRoot, grant.Path) {
			return fmt.Errorf(
				"tclaude-layer profile filesystem rule %q (%s) is at or below harness state root %q; refusing a launch that cannot persist harness state",
				grant.Path,
				grant.Access,
				stateRoot,
			)
		}
	}
	return nil
}

func tclaudeLayerPhase0WriteDirs(
	contract TclaudeLayerLaunchContract,
	effective sandboxpolicy.EffectiveProfile,
) ([]string, error) {
	stateRoot, err := tclaudeLayerHarnessStateRoot(contract.HarnessName)
	if err != nil {
		return nil, err
	}
	candidates := append([]string{stateRoot}, contract.WriteDirs...)
	agentDirectoryNames := make(map[string]bool, len(effective.AgentDirectories))
	for _, name := range effective.AgentDirectories {
		agentDirectoryNames[name] = true
	}
	for _, entry := range effective.Environment {
		if agentDirectoryNames[entry.Name] {
			candidates = append(candidates, entry.Value)
		}
	}
	out := make([]string, 0, len(candidates))
	seen := make(map[string]bool, len(candidates))
	for _, path := range candidates {
		path = filepath.Clean(strings.TrimSpace(path))
		if path == "." || !filepath.IsAbs(path) {
			return nil, fmt.Errorf("tclaude-layer launch-contract path %q is not absolute", path)
		}
		if resolved, resolveErr := filepath.EvalSymlinks(path); resolveErr == nil {
			path = filepath.Clean(resolved)
		}
		if !seen[path] {
			seen[path] = true
			out = append(out, path)
		}
	}
	return out, nil
}

func tclaudeLayerHarnessStateRoot(harnessName string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory for harness state: %w", err)
	}
	switch harnessName {
	case harness.DefaultName:
		return filepath.Join(home, ".claude"), nil
	case harness.CodexName:
		if root := strings.TrimSpace(os.Getenv("CODEX_HOME")); root != "" {
			return root, nil
		}
		return filepath.Join(home, ".codex"), nil
	default:
		return "", fmt.Errorf("tclaude-layer has no launch-contract state root for harness %q", harnessName)
	}
}

// ValidateTclaudeLayerHarness refuses topologies where the wrapped pane does
// not contain the process that executes tools.
func ValidateTclaudeLayerHarness(harnessName string) error {
	if harnessName == harness.OpenCodeName {
		return fmt.Errorf("tclaude-layer does not yet support OpenCode: its tool-executing server runs outside the wrapped pane")
	}
	return nil
}

func bwrapBindSourceExists(path string) (bool, error) {
	_, err := os.Stat(path)
	switch {
	case err == nil:
		return true, nil
	case os.IsNotExist(err):
		// Policy authoring intentionally permits future paths. Never create one
		// on the host merely to make a bind possible.
		return false, nil
	default:
		return false, err
	}
}

func bwrapCommand(
	binary string,
	phase0WriteDirs, breakGlassPaths []string,
	plan sandboxpolicy.MountPlan,
	harnessCommand string,
) (string, error) {
	args, err := bwrapArgs(phase0WriteDirs, breakGlassPaths, plan)
	if err != nil {
		return "", err
	}
	command := clcommon.ShellQuoteArg(binary)
	for _, arg := range args {
		command += " " + clcommon.ShellQuoteArg(arg)
	}
	// Harness spawners return a safe shell command rather than argv. Keep that
	// contract intact inside the namespace instead of reparsing it here.
	command += " -- sh -c " + clcommon.ShellQuoteArg(harnessCommand)
	return command, nil
}
