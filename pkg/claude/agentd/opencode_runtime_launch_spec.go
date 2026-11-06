package agentd

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/managedinstructions"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
	tclcommon "github.com/tofutools/tclaude/pkg/common"
)

func openCodeServerEnvironment(
	ambient []string,
	sandboxSpec *session.TclaudeLayerLaunchSpec,
) []string {
	if sandboxSpec == nil {
		return append([]string(nil), ambient...)
	}
	privateState := len(sandboxSpec.Contract.Environment) > 0
	filtered := openCodeProviderIsolatedSpec(sandboxSpec)
	out := make([]string, 0, len(ambient)+len(sandboxSpec.Effective.Environment)+
		len(sandboxSpec.Contract.Environment)+8)
	for _, entry := range ambient {
		name := strings.SplitN(entry, "=", 2)[0]
		if (privateState && openCodePrivateEnvironmentName(name)) ||
			(filtered && openCodeFilteredControlledEnvironmentName(name)) {
			continue
		}
		out = append(out, entry)
	}
	for _, entry := range sandboxSpec.Effective.Environment {
		if privateState && openCodePrivateEnvironmentName(entry.Name) {
			continue
		}
		if filtered && openCodeFilteredControlledEnvironmentName(entry.Name) {
			continue
		}
		out = append(out, entry.Name+"="+entry.Value)
	}
	for _, entry := range sandboxSpec.Contract.Environment {
		out = append(out, entry.Name+"="+entry.Value)
	}
	if filtered {
		// These are OpenCode 1.18.6's provider-source isolation inputs.
		// They make the frozen inline provider content the only dynamic provider
		// source consumed by the authoritative server.
		out = append(out,
			"HOME="+filepath.Join(
				sandboxSpec.Contract.StateRoot, openCodeFilteredHomeBase),
			"OPENCODE_CONFIG=",
			"OPENCODE_CONFIG_DIR=",
			"OPENCODE_DISABLE_PROJECT_CONFIG=1",
			"OPENCODE_PURE=1",
			"OPENCODE_DISABLE_MODELS_FETCH=1",
			"OPENCODE_DISABLE_AUTOUPDATE=1",
			"OPENCODE_AUTH_CONTENT={}",
		)
	}
	return out
}

func openCodeProviderIsolatedSpec(
	spec *session.TclaudeLayerLaunchSpec,
) bool {
	if spec == nil {
		return false
	}
	// Provider isolation is a persisted contract shape, not a consequence of
	// filtered networking. New sandboxes always retain the harness-required
	// config/auth projection; recognize this shape only so older serialized
	// provider-isolated specs remain replayable.
	stateRoot := canonicalOpenCodeRuntimePath(spec.Contract.StateRoot)
	if stateRoot == "" {
		return false
	}
	want := canonicalOpenCodeRuntimePath(filepath.Join(
		stateRoot, openCodeFilteredConfigBase))
	for _, entry := range spec.Contract.Environment {
		if entry.Name == "XDG_CONFIG_HOME" {
			return canonicalOpenCodeRuntimePath(entry.Value) == want
		}
	}
	return false
}

func openCodeFilteredNetworkSpec(
	spec *session.TclaudeLayerLaunchSpec,
) bool {
	if spec == nil {
		return false
	}
	posture, err := session.TclaudeLayerNetworkPosture(spec.Effective)
	if err != nil || posture != sandboxpolicy.NetworkFiltered {
		return false
	}
	axes, err := sandboxpolicy.PlannedEffectiveAccessAxes(spec.Effective)
	return err == nil &&
		!sandboxpolicy.NetworkRulesArePrivateRoutedOpen(axes.Network)
}

func openCodeFilteredControlledEnvironmentName(name string) bool {
	switch name {
	case "HOME", "OPENCODE_CONFIG", "OPENCODE_CONFIG_DIR",
		"OPENCODE_DISABLE_PROJECT_CONFIG", "OPENCODE_PURE",
		"OPENCODE_DISABLE_MODELS_FETCH", "OPENCODE_DISABLE_AUTOUPDATE",
		"OPENCODE_AUTH_CONTENT":
		return true
	default:
		return false
	}
}

func openCodePrivateEnvironmentName(name string) bool {
	switch name {
	case "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME",
		"OPENCODE_CONFIG_DIR":
		return true
	default:
		return false
	}
}

func validateOpenCodeV3LaunchContract(
	contract session.TclaudeLayerLaunchContract,
	filtered bool,
) error {
	if len(contract.Environment) == 0 {
		// Grandfathered v3 contracts intentionally keep ambient XDG state.
		if openCodeAgentIDRE.MatchString(filepath.Base(
			canonicalOpenCodeRuntimePath(contract.StateRoot))) {
			return fmt.Errorf("private OpenCode v3 launch contract has no enforced XDG environment")
		}
		if len(contract.FinalHideDirs) != 1 {
			return fmt.Errorf("legacy OpenCode v3 launch contract does not hide the private-state parent")
		}
		return nil
	}
	expectedNames := []string{
		"XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME",
	}
	if len(contract.Environment) != len(expectedNames) ||
		len(contract.StateDirs) != len(expectedNames) {
		return fmt.Errorf("private OpenCode v3 launch contract must carry exactly four XDG roots")
	}
	stateRoot := canonicalOpenCodeRuntimePath(contract.StateRoot)
	if stateRoot == "" || !openCodeAgentIDRE.MatchString(filepath.Base(stateRoot)) {
		return fmt.Errorf("private OpenCode v3 state root %q does not end in a stable agent id",
			contract.StateRoot)
	}
	for i, name := range expectedNames {
		entry := contract.Environment[i]
		if entry.Name != name {
			return fmt.Errorf("private OpenCode v3 launch contract environment %d is %q, want %q",
				i, entry.Name, name)
		}
		wantStateDir := filepath.Join(entry.Value, "opencode")
		actualStateDir := canonicalOpenCodeRuntimePath(contract.StateDirs[i])
		matchesStateDir := actualStateDir == canonicalOpenCodeRuntimePath(wantStateDir)
		// New private layouts reopen each mutable XDG base so tools launched by
		// OpenCode can use their own application directories. Accept the older
		// app-only shape as well: launch specs are persisted and must replay
		// across upgrades. Config remains app-only and read-only below.
		if i != 2 && actualStateDir == canonicalOpenCodeRuntimePath(entry.Value) {
			matchesStateDir = true
		}
		if i == 2 && filtered {
			wantConfigBase := filepath.Join(stateRoot, openCodeFilteredConfigBase)
			if canonicalOpenCodeRuntimePath(entry.Value) !=
				canonicalOpenCodeRuntimePath(wantConfigBase) {
				return fmt.Errorf(
					"private filtered OpenCode XDG_CONFIG_HOME is %q, want empty per-agent base %q",
					entry.Value, wantConfigBase)
			}
			// The normal config state dir remains the daemon-final read-only
			// projection of ambient config, but filtered OpenCode does not
			// select it as XDG_CONFIG_HOME.
			matchesStateDir = canonicalOpenCodeRuntimePath(contract.StateDirs[i]) ==
				canonicalOpenCodeRuntimePath(
					filepath.Join(stateRoot, "config", "opencode"))
		}
		if i == 2 && !matchesStateDir {
			// Darwin keeps XDG_CONFIG_HOME at the real host base while the
			// daemon-final read-only root carries the resolved identity of a
			// possible leaf symlink at <base>/opencode.
			matchesStateDir = openCodeRuntimePathsEquivalent(
				contract.StateDirs[i], wantStateDir)
		}
		if !matchesStateDir {
			return fmt.Errorf("private OpenCode v3 %s does not target state directory %q",
				name, contract.StateDirs[i])
		}
	}
	privatePair := false
	for _, pair := range contract.PrivateWriteDirs {
		if canonicalOpenCodeRuntimePath(pair.Current) == stateRoot &&
			canonicalOpenCodeRuntimePath(pair.Parent) == filepath.Dir(stateRoot) {
			privatePair = true
			break
		}
	}
	if !privatePair {
		return fmt.Errorf("private OpenCode v3 launch contract does not hide siblings and reopen its agent root")
	}
	if len(contract.FinalHideDirs) != 3 {
		return fmt.Errorf("private OpenCode v3 launch contract must hide the three ambient mutable XDG roots")
	}
	if openCodeReadOnlyConfigBindSource(contract) == "" {
		return fmt.Errorf("private OpenCode v3 launch contract does not bind global config read-only")
	}
	if filtered {
		expected := []string{
			filepath.Join(stateRoot, openCodeFilteredConfigBase, "opencode"),
			filepath.Join(stateRoot, openCodeFilteredHomeBase, ".opencode"),
		}
		if len(contract.ReadOnlyBinds) < len(expected) {
			return fmt.Errorf(
				"private filtered OpenCode contract does not seal its provider-empty config roots")
		}
		tail := contract.ReadOnlyBinds[len(contract.ReadOnlyBinds)-len(expected):]
		for index, path := range expected {
			if canonicalOpenCodeRuntimePath(tail[index].Source) != path ||
				canonicalOpenCodeRuntimePath(tail[index].Target) != path {
				return fmt.Errorf(
					"private filtered OpenCode contract does not daemon-final seal provider config root %q",
					path)
			}
		}
	}
	return nil
}

func validateOpenCodeFilteredProviderAuthority(
	spec *session.TclaudeLayerLaunchSpec,
) error {
	if !openCodeProviderIsolatedSpec(spec) {
		return nil
	}
	if err := validateOpenCodeFilteredProviderSources(
		spec.Contract.StateRoot); err != nil {
		return err
	}
	return refuseOpenCodeFilteredActiveAccount(spec.Contract.StateRoot)
}

func openCodeRuntimePathsEquivalent(left, right string) bool {
	left = canonicalOpenCodeRuntimePath(left)
	right = canonicalOpenCodeRuntimePath(right)
	if left == "" || right == "" {
		return false
	}
	if left == right {
		return true
	}
	resolvedLeft, leftErr := filepath.EvalSymlinks(left)
	resolvedRight, rightErr := filepath.EvalSymlinks(right)
	return leftErr == nil && rightErr == nil &&
		canonicalOpenCodeRuntimePath(resolvedLeft) ==
			canonicalOpenCodeRuntimePath(resolvedRight)
}

func openCodeServeExec(
	executable, port string,
	sandboxSpec *session.TclaudeLayerLaunchSpec,
) (string, []string, error) {
	launcher, err := resolveOpenCodeLayerLauncher(sandboxSpec)
	if err != nil {
		return "", nil, err
	}
	return openCodeServeExecWithAuthority(executable, port, sandboxSpec, launcher)
}

func openCodeServeExecWithAuthority(
	executable, port string,
	sandboxSpec *session.TclaudeLayerLaunchSpec,
	launcher string,
	httpProxySessionIDs ...string,
) (string, []string, error) {
	serveArgs := []string{
		"serve", "--hostname", "127.0.0.1",
		"--port", port, "--log-level", openCodeServeLogLevel,
	}
	serveCommand := shellJoinOpenCodeCommand(executable, serveArgs)
	httpBridge := false
	if len(httpProxySessionIDs) > 0 {
		gatewayCLIPath, err := session.HTTPProxyCLIForLayerSpec(sandboxSpec)
		if err != nil {
			return "", nil, err
		}
		wrapped := session.WrapHTTPProxyRuntimeCommand(httpProxySessionIDs[0], serveCommand, gatewayCLIPath)
		httpBridge = wrapped != serveCommand
		serveCommand = wrapped
	}
	if sandboxSpec == nil {
		if httpBridge {
			return clcommon.BootstrapShellPath(), []string{"-c", "exec " + serveCommand}, nil
		}
		return executable, serveArgs, nil
	}
	filteredDarwinProxy := false
	if goruntime.GOOS == "darwin" && openCodeFilteredNetworkSpec(sandboxSpec) {
		engine, engineErr := session.TclaudeLayerNetworkEngine(sandboxSpec.Effective)
		if engineErr != nil {
			return "", nil, engineErr
		}
		filteredDarwinProxy = engine == sandboxpolicy.NetworkEngineProxy
	}
	if !filteredDarwinProxy &&
		sandboxSpec.Effective.NetworkAccess != sandboxpolicy.NetworkAccessInherit &&
		sandboxSpec.Effective.NetworkAccess != sandboxpolicy.NetworkAccessInternet {
		return "", nil, fmt.Errorf(
			"unsupported_sandbox_profile_network: OpenCode with tclaude’s sandbox requires the host-open loopback control plane and endpoint-ownership proof",
		)
	}
	wrapped := ""
	var err error
	if filteredDarwinProxy {
		bindPort, parseErr := strconv.Atoi(port)
		if parseErr != nil || bindPort < 1 || bindPort > 65535 {
			return "", nil, fmt.Errorf("parse OpenCode loopback control port %q", port)
		}
		wrapped, err = wrapOpenCodeTclaudeLayerWithLoopbackBind(
			launcher, *sandboxSpec, bindPort, serveCommand)
	} else {
		wrapped, err = wrapOpenCodeTclaudeLayer(launcher, *sandboxSpec, serveCommand)
	}
	if err != nil {
		return "", nil, fmt.Errorf("wrap OpenCode server with tclaude’s sandbox: %w", err)
	}
	// exec makes the PID agentd records the top wrapper rather than an
	// intermediate shell. Stop/recovery can therefore target the boundary,
	// while endpoint ownership continues to prove the listener in its subtree.
	return "sh", []string{"-c", "exec " + wrapped}, nil
}

func openCodeTclaudeLayerLaunchSpec(
	implementation, cwd string,
	gitWriteDirs []string,
	snapshot *sandboxpolicy.Snapshot,
	agentID string,
	privateSessionIDs ...string,
) (*session.TclaudeLayerLaunchSpec, error) {
	normalized, err := sandboxpolicy.NormalizeImplementation(implementation)
	if err != nil {
		return nil, err
	}
	if normalized != sandboxpolicy.ImplementationTclaudeLayer {
		return nil, nil
	}
	effective := sandboxpolicy.EffectiveProfile{}
	if snapshot != nil {
		effective = snapshot.Effective
	}
	posture, err := session.TclaudeLayerNetworkPosture(effective)
	if err != nil {
		return nil, err
	}
	if posture != sandboxpolicy.NetworkHostOpen {
		if posture == sandboxpolicy.NetworkFiltered {
			axes, axesErr := sandboxpolicy.PlannedEffectiveAccessAxes(effective)
			if axesErr != nil {
				return nil, axesErr
			}
			privateRouted := sandboxpolicy.NetworkRulesArePrivateRoutedOpen(
				axes.Network)
			// The engine comes from the composed policy, through the same
			// resolution the launch itself performs, so the preflight probes
			// the floor the launch will actually build. Never re-derived here:
			// TclaudeLayerNetworkEngine is the one answer, and asking a second
			// way is how a preflight ends up probing a different floor.
			engine, engineErr := session.TclaudeLayerNetworkEngine(effective)
			if engineErr != nil {
				return nil, engineErr
			}
			_, _, filteredErr := resolveOpenCodeTclaudeLayer(
				posture, sandboxpolicy.RootConstructed, engine)
			if filteredErr != nil {
				return nil, filteredErr
			}
			if goruntime.GOOS == "darwin" {
				if privateRouted {
					return nil, fmt.Errorf("private routed networking requires Linux")
				}
				return buildOpenCodeTclaudeLayerLaunchSpec(
					cwd, gitWriteDirs, snapshot, agentID, false, false,
					privateSessionIDs...)
			}
			if goruntime.GOOS != "linux" {
				return nil, fmt.Errorf("OpenCode filtered networking requires Linux or macOS")
			}
			return buildOpenCodeTclaudeLayerLaunchSpec(
				cwd, gitWriteDirs, snapshot, agentID, true, false,
				privateSessionIDs...)
		}
		openCodeHarness, resolveErr := harness.Resolve(harness.OpenCodeName)
		if resolveErr != nil {
			return nil, resolveErr
		}
		if _, err := session.ValidateTclaudeLayerNetwork(
			openCodeHarness,
			effective,
			harness.ResolvedModelTransport{},
		); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("unsupported OpenCode with tclaude’s sandbox network posture %s", posture)
	}
	return buildOpenCodeTclaudeLayerLaunchSpec(
		cwd, gitWriteDirs, snapshot, agentID, false, false,
		privateSessionIDs...)
}

// openCodeUnixRelayLaunchSpec builds the isolated v4 control-plane smoke
// boundary. Public filtered launches use the same builder through
// openCodeTclaudeLayerLaunchSpec after provider and endpoint preflight.
func openCodeUnixRelayLaunchSpec(
	implementation, cwd string,
	gitWriteDirs []string,
	snapshot *sandboxpolicy.Snapshot,
	agentID string,
	privateSessionIDs ...string,
) (*session.TclaudeLayerLaunchSpec, error) {
	if goruntime.GOOS != "linux" {
		return nil, fmt.Errorf("OpenCode Unix relay is Linux-only")
	}
	normalized, err := sandboxpolicy.NormalizeImplementation(implementation)
	if err != nil {
		return nil, err
	}
	if normalized != sandboxpolicy.ImplementationTclaudeLayer {
		return nil, fmt.Errorf("OpenCode Unix relay requires tclaude’s sandbox")
	}
	if snapshot == nil {
		return nil, fmt.Errorf("OpenCode Unix relay requires an isolated effective profile")
	}
	posture, err := session.TclaudeLayerNetworkPosture(snapshot.Effective)
	if err != nil {
		return nil, err
	}
	if posture != sandboxpolicy.NetworkIsolatedWithAgentd {
		return nil, fmt.Errorf("OpenCode Unix relay requires the isolated network posture")
	}
	return buildOpenCodeTclaudeLayerLaunchSpec(
		cwd, gitWriteDirs, snapshot, agentID, true, false,
		privateSessionIDs...)
}

func buildOpenCodeTclaudeLayerLaunchSpec(
	cwd string,
	gitWriteDirs []string,
	snapshot *sandboxpolicy.Snapshot,
	agentID string,
	unixRelay bool,
	filtered bool,
	privateSessionIDs ...string,
) (*session.TclaudeLayerLaunchSpec, error) {
	allocation, err := requireOpenCodeStateAllocation(agentID)
	if err != nil {
		return nil, err
	}
	if filtered {
		if err := refuseOpenCodeFilteredActiveAccount(allocation.StateRoot); err != nil {
			return nil, err
		}
	}
	layout, err := openCodeStateLayoutForAllocation(*allocation)
	if err != nil {
		return nil, err
	}
	if filtered {
		if err := isolateOpenCodeFilteredConfig(layout); err != nil {
			return nil, err
		}
	}
	var privateWriteDirs []session.TclaudeLayerPrivateWriteDir
	if allocation.Mode == db.OpenCodeStatePrivate {
		privateWriteDirs = append(privateWriteDirs, session.TclaudeLayerPrivateWriteDir{
			Parent: layout.parent, Current: allocation.StateRoot,
		})
	}
	if len(privateSessionIDs) > 0 && strings.TrimSpace(privateSessionIDs[0]) != "" {
		privateWriteDirs = append(privateWriteDirs, session.TclaudeLayerPrivateWriteDir{
			Parent:  tclcommon.SpawnAttachmentsPrivateBase(),
			Current: tclcommon.SpawnAttachmentsPrivateDir(privateSessionIDs[0]),
		})
	}
	if dir := tclcommon.ExistingMorphArtifactsDir(agentID); dir != "" {
		privateWriteDirs = append(privateWriteDirs, session.TclaudeLayerPrivateWriteDir{
			Parent: tclcommon.MorphArtifactsBase(), Current: dir,
		})
	}
	input := session.TclaudeLayerLaunchInput{
		HarnessName:      harness.OpenCodeName,
		Cwd:              cwd,
		GitWriteDirs:     gitWriteDirs,
		Snapshot:         snapshot,
		PrivateWriteDirs: privateWriteDirs,
		Environment:      layout.environment,
		FinalHideDirs:    layout.finalHideDirs,
		ReadOnlyBinds:    layout.readOnlyBinds,
	}
	managed, err := db.GetManagedInstructionLaunch(agentID)
	if err != nil {
		return nil, err
	}
	if managed != nil && managed.Root != "" {
		if err = managedinstructions.VerifyLaunchRoot(managed.Root); err != nil {
			return nil, err
		}
		input.HarnessReadPaths = append(input.HarnessReadPaths, managed.Root)
	}
	if unixRelay {
		controlPath, controlErr := openCodeControlSocketPath(agentID)
		if controlErr != nil {
			return nil, controlErr
		}
		input.OpenCodeControl = &session.TclaudeLayerOpenCodeControl{
			Transport: session.TclaudeLayerUnixRelayTransport, SocketPath: controlPath,
		}
	}
	if allocation.Mode == db.OpenCodeStatePrivate {
		input.StateRoot = allocation.StateRoot
		input.StateDirs = layout.stateDirs
	}
	spec, err := session.BuildTclaudeLayerLaunchSpec(input)
	if err != nil {
		return nil, err
	}
	return &spec, nil
}

// refuseOpenCodeFilteredActiveAccount prevents OpenCode's persistent
// account/org service from loading a remote provider config after the frozen
// inline config. The pinned server keeps this state in its per-agent database,
// independently of auth.json and OPENCODE_AUTH_CONTENT.
func refuseOpenCodeFilteredActiveAccount(stateRoot string) error {
	databasePath := filepath.Join(stateRoot, "data", "opencode", "opencode.db")
	info, err := os.Lstat(databasePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf(
			"OpenCode filtered cannot inspect persistent account authority: %w; fix the private state or use network open",
			err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf(
			"OpenCode filtered cannot inspect non-regular persistent account database %q; fix the private state or use network open",
			databasePath)
	}
	dsn := (&url.URL{
		Scheme:   "file",
		Path:     databasePath,
		RawQuery: "mode=ro&_pragma=busy_timeout(1000)&_pragma=query_only(1)",
	}).String()
	store, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf(
			"OpenCode filtered cannot inspect persistent account authority: %w; sign out of OpenCode or use network open",
			err)
	}
	defer func() { _ = store.Close() }()
	var activeAccount, activeOrg sql.NullString
	err = store.QueryRow(
		`SELECT active_account_id, active_org_id FROM account_state WHERE id = 1`,
	).Scan(&activeAccount, &activeOrg)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf(
			"OpenCode filtered cannot prove persistent account/org provider configuration absent: %w; migrate or repair the OpenCode state under network open, then retry",
			err)
	}
	if activeAccount.Valid && strings.TrimSpace(activeAccount.String) != "" &&
		activeOrg.Valid && strings.TrimSpace(activeOrg.String) != "" {
		return fmt.Errorf(
			"OpenCode filtered does not support an active persistent account/org because its remote config loads after OPENCODE_CONFIG_CONTENT; sign out or clear the active organization, or use network open")
	}
	return nil
}
