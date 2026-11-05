package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

func resolveOpenCodeLayerLauncher(spec *session.TclaudeLayerLaunchSpec) (string, error) {
	if spec == nil {
		return "", nil
	}
	posture := sandboxpolicy.NetworkHostOpen
	if spec.Version == session.TclaudeLayerUnixRelaySpecVersion {
		var err error
		posture, err = session.TclaudeLayerNetworkPosture(spec.Effective)
		if err != nil {
			return "", err
		}
	} else {
		filteredDarwinProxy := false
		if goruntime.GOOS == "darwin" && openCodeFilteredNetworkSpec(spec) {
			engine, err := session.TclaudeLayerNetworkEngine(spec.Effective)
			if err != nil {
				return "", err
			}
			filteredDarwinProxy = engine == sandboxpolicy.NetworkEngineProxy
		}
		if !filteredDarwinProxy &&
			spec.Effective.NetworkAccess != sandboxpolicy.NetworkAccessInherit &&
			spec.Effective.NetworkAccess != sandboxpolicy.NetworkAccessInternet {
			return "", fmt.Errorf(
				"unsupported_sandbox_profile_network: OpenCode with tclaude’s sandbox requires the host-open loopback control plane and endpoint-ownership proof")
		}
		if filteredDarwinProxy {
			posture = sandboxpolicy.NetworkFiltered
		}
	}
	root, err := session.TclaudeLayerRootPosture(posture, spec.Effective)
	if err != nil {
		return "", err
	}
	engine, err := session.TclaudeLayerNetworkEngine(spec.Effective)
	if err != nil {
		return "", err
	}
	launcher, _, err := resolveOpenCodeTclaudeLayer(posture, root, engine)
	return launcher, err
}

func buildOpenCodeExecutionBoundary(
	implementation string,
	spec *session.TclaudeLayerLaunchSpec,
	cwd string,
	authority openCodeLaunchAuthority,
) (string, error) {
	environment := map[string]string{}
	for _, entry := range openCodeServerEnvironment(os.Environ(), spec) {
		name, value, found := strings.Cut(entry, "=")
		if found && name == "PATH" {
			environment[name] = value
		}
	}
	boundary, err := session.BuildExecutionBoundary(session.ExecutionBoundaryInput{
		SandboxImplementation:     implementation,
		HarnessName:               harness.OpenCodeName,
		HarnessLookupName:         harness.OpenCodeName,
		HarnessExecutable:         authority.Executable,
		HarnessExecutableResolved: true,
		LauncherBinary:            authority.Launcher,
		LauncherBinaryResolved:    authority.Launcher != "",
		Cwd:                       cwd,
		Environment:               environment,
		LayerSpec:                 spec,
	})
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(boundary)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func openCodeSandboxRecord(
	rawImplementation string,
	spec *session.TclaudeLayerLaunchSpec,
) (string, string, error) {
	implementation, err := sandboxpolicy.NormalizeImplementation(rawImplementation)
	if err != nil {
		return "", "", err
	}
	if spec == nil {
		switch implementation {
		case sandboxpolicy.ImplementationHarnessBuiltin, sandboxpolicy.ImplementationOff,
			sandboxpolicy.ImplementationResourceOnly:
			return string(implementation), "", nil
		default:
			return "", "", fmt.Errorf(
				"OpenCode sandbox implementation %q requires a launch spec", implementation)
		}
	}
	if implementation != sandboxpolicy.ImplementationTclaudeLayer {
		return "", "", fmt.Errorf(
			"OpenCode sandbox launch spec is incompatible with implementation %q", implementation)
	}
	encoded, err := json.Marshal(spec)
	if err != nil {
		return "", "", fmt.Errorf("encode OpenCode with tclaude’s sandbox launch spec: %w", err)
	}
	return string(sandboxpolicy.ImplementationTclaudeLayer), string(encoded), nil
}

func normalizeOpenCodeRuntimeSandboxImplementation(raw string) (sandboxpolicy.Implementation, error) {
	if strings.TrimSpace(raw) == "" {
		return sandboxpolicy.ImplementationHarnessBuiltin, nil
	}
	return sandboxpolicy.NormalizeImplementation(raw)
}

func openCodeRuntimeSandboxSpec(
	runtime db.OpenCodeRuntime,
) (*session.TclaudeLayerLaunchSpec, error) {
	if err := db.ValidateOpenCodeRuntimeTransport(runtime); err != nil {
		return nil, fmt.Errorf("OpenCode runtime transport authority: %w", err)
	}
	implementation, err := normalizeOpenCodeRuntimeSandboxImplementation(
		runtime.SandboxImplementation)
	if err != nil {
		return nil, fmt.Errorf("OpenCode runtime sandbox implementation: %w", err)
	}
	switch implementation {
	case sandboxpolicy.ImplementationHarnessBuiltin, sandboxpolicy.ImplementationOff,
		sandboxpolicy.ImplementationResourceOnly:
		if strings.TrimSpace(runtime.SandboxLaunchSpecJSON) != "" {
			return nil, fmt.Errorf(
				"OpenCode %s runtime unexpectedly carries a tclaude sandbox launch spec",
				implementation)
		}
		return nil, nil
	case sandboxpolicy.ImplementationTclaudeLayer:
	default:
		return nil, fmt.Errorf("unsupported OpenCode runtime sandbox implementation %q", implementation)
	}
	raw := strings.TrimSpace(runtime.SandboxLaunchSpecJSON)
	if raw == "" {
		return nil, fmt.Errorf(
			"OpenCode with tclaude’s sandbox runtime has no persisted launch spec; refusing an unwrapped restart")
	}
	if len(raw) > openCodeSandboxSpecMax {
		return nil, fmt.Errorf("OpenCode with tclaude’s sandbox launch spec exceeds %d bytes", openCodeSandboxSpecMax)
	}
	var spec session.TclaudeLayerLaunchSpec
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&spec); err != nil {
		return nil, fmt.Errorf("decode OpenCode with tclaude’s sandbox launch spec: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = fmt.Errorf("multiple JSON values")
		}
		return nil, fmt.Errorf("decode OpenCode with tclaude’s sandbox launch spec trailer: %w", err)
	}
	if spec.Version != session.TclaudeLayerLaunchSpecVersion &&
		spec.Version != session.TclaudeLayerLegacyLaunchSpecVersion &&
		spec.Version != session.TclaudeLayerUnixRelaySpecVersion {
		return nil, fmt.Errorf("unsupported OpenCode with tclaude’s sandbox launch spec version %d", spec.Version)
	}
	if spec.Contract.HarnessName != harness.OpenCodeName {
		return nil, fmt.Errorf("OpenCode with tclaude’s sandbox launch spec names harness %q",
			spec.Contract.HarnessName)
	}
	if !filepath.IsAbs(spec.Contract.StateRoot) {
		return nil, fmt.Errorf("OpenCode with tclaude’s sandbox launch spec state root %q is not absolute",
			spec.Contract.StateRoot)
	}
	if len(spec.Contract.StateDirs) == 0 {
		return nil, fmt.Errorf("OpenCode with tclaude’s sandbox launch spec has no mutable state directories")
	}
	for _, stateDir := range spec.Contract.StateDirs {
		stateDir = canonicalOpenCodeRuntimePath(stateDir)
		if stateDir == "" {
			return nil, fmt.Errorf("OpenCode with tclaude’s sandbox launch spec has a non-absolute state directory")
		}
		inWriteContract := false
		for _, writeDir := range spec.Contract.WriteDirs {
			if canonicalOpenCodeRuntimePath(writeDir) == stateDir {
				inWriteContract = true
				break
			}
		}
		if !inWriteContract {
			return nil, fmt.Errorf(
				"OpenCode with tclaude’s sandbox state directory %q is not in the writable launch contract",
				stateDir)
		}
	}
	if len(spec.Contract.ReadOnlyStateDirs) == 0 &&
		len(spec.Contract.ReadOnlyBinds) == 0 {
		return nil, fmt.Errorf(
			"OpenCode with tclaude’s sandbox launch spec does not protect its executable state")
	}
	stateRoot := canonicalOpenCodeRuntimePath(spec.Contract.StateRoot)
	for _, stateDir := range spec.Contract.ReadOnlyStateDirs {
		stateDir = canonicalOpenCodeRuntimePath(stateDir)
		if stateDir == "" || stateDir == stateRoot ||
			!sandboxpolicy.PathContainsOrEqual(stateRoot, stateDir) {
			return nil, fmt.Errorf(
				"OpenCode with tclaude’s sandbox launch spec has invalid read-only state directory %q",
				stateDir)
		}
		access, covered := sandboxpolicy.EffectiveAccessAt(spec.Effective.Filesystem, stateDir)
		if !covered || access != sandboxpolicy.AccessRead {
			return nil, fmt.Errorf(
				"OpenCode with tclaude’s sandbox read-only state directory %q is not protected in the rendered contract",
				stateDir)
		}
	}
	if spec.Version == session.TclaudeLayerLaunchSpecVersion ||
		spec.Version == session.TclaudeLayerUnixRelaySpecVersion {
		if err := validateOpenCodeV3LaunchContract(
			spec.Contract, openCodeProviderIsolatedSpec(&spec)); err != nil {
			return nil, err
		}
	}
	if err := validateOpenCodeFilteredProviderAuthority(&spec); err != nil {
		return nil, fmt.Errorf(
			"revalidate OpenCode filtered provider authority: %w", err)
	}
	cwd := canonicalOpenCodeRuntimePath(runtime.Cwd)
	hasCwd := false
	for _, path := range spec.Contract.WriteDirs {
		if openCodeRuntimePathsEquivalent(path, cwd) {
			hasCwd = true
			break
		}
	}
	if cwd == "" || !hasCwd {
		return nil, fmt.Errorf(
			"OpenCode with tclaude’s sandbox launch spec does not preserve runtime cwd %q as a writable contract path",
			runtime.Cwd)
	}
	effective, err := revalidateOpenCodeRuntimeEffective(spec.Effective)
	if err != nil {
		return nil, err
	}
	spec.Effective = effective
	posture, err := session.TclaudeLayerNetworkPosture(spec.Effective)
	if err != nil {
		return nil, err
	}
	// Darwin keeps the authenticated loopback control transport when its
	// filtered model path is carried by the proxy. This is the only filtered
	// v3 replay shape; Linux remains on v4's inherited Unix relay below.
	darwinFilteredLoopback := false
	if goruntime.GOOS == "darwin" &&
		spec.Version == session.TclaudeLayerLaunchSpecVersion &&
		posture == sandboxpolicy.NetworkFiltered &&
		spec.Contract.NetworkEngine == sandboxpolicy.NetworkEngineProxy &&
		runtime.Transport == db.OpenCodeTransportLoopbackTCP {
		engine, engineErr := session.TclaudeLayerNetworkEngine(spec.Effective)
		if engineErr != nil {
			return nil, engineErr
		}
		darwinFilteredLoopback = engine == sandboxpolicy.NetworkEngineProxy
	}
	if spec.Version == session.TclaudeLayerUnixRelaySpecVersion {
		if (posture != sandboxpolicy.NetworkIsolatedWithAgentd &&
			posture != sandboxpolicy.NetworkFiltered) ||
			runtime.Transport != db.OpenCodeTransportUnixRelay ||
			spec.Contract.OpenCodeControl == nil ||
			runtime.ControlSocketPath != spec.Contract.OpenCodeControl.SocketPath {
			return nil, fmt.Errorf(
				"OpenCode with tclaude’s sandbox v4 runtime does not match its Unix-relay authority")
		}
		agentID := filepath.Base(filepath.Dir(runtime.ControlSocketPath))
		expectedControlPath, authorityErr := openCodeControlSocketPath(agentID)
		// Split, because the two arms are not the same statement and the
		// combined one could only ever say the second (TCL-909).
		//
		// When authorityErr is set the daemon could not COMPUTE the authority,
		// so "the control path is outside its allocated agent authority" is a
		// verdict about a comparison that never happened. That mattered
		// concretely: a replayed isolated or filtered spec whose allocation was
		// stranded by a changed XDG_DATA_HOME or HOME lands here, and the one
		// sentence naming that cause — and now the way out of it — was
		// discarded on the line below.
		//
		// Wrapping rather than replacing: the outer sentence still says which
		// contract failed, which is what this layer knows.
		if authorityErr != nil {
			return nil, fmt.Errorf(
				"OpenCode with tclaude’s sandbox v4 runtime control authority could not be established: %w",
				authorityErr)
		}
		if expectedControlPath != runtime.ControlSocketPath {
			// "does not match", not "is outside". The test is string equality
			// against one exact socket path, not containment in a region, and
			// containment language invites the operator to go looking for a
			// directory boundary that was never consulted.
			return nil, fmt.Errorf(
				"OpenCode with tclaude’s sandbox v4 runtime control path %q does not match its allocated agent authority %q",
				runtime.ControlSocketPath, expectedControlPath)
		}
	} else if !darwinFilteredLoopback &&
		(posture != sandboxpolicy.NetworkHostOpen ||
			runtime.Transport == db.OpenCodeTransportUnixRelay) {
		return nil, fmt.Errorf(
			"unsupported_sandbox_profile_network: OpenCode with tclaude’s sandbox restart requires the host-open loopback control plane and endpoint-ownership proof")
	}
	if err := session.ValidateTclaudeLayerLaunchSpec(spec); err != nil {
		return nil, fmt.Errorf("validate OpenCode with tclaude’s sandbox renderer contract: %w", err)
	}
	return &spec, nil
}

// revalidateOpenCodeRuntimeEffective omits the generated agentd AF_UNIX socket
// floor while re-running directory normalization. A live socket is not a
// directory and therefore cannot pass profile filesystem normalization, but
// the floor is not operator-authored filesystem authority: the launch contract
// binds and revalidates it separately as a Unix socket. Restore the exact
// frozen rows only after every other effective field revalidates unchanged.
func revalidateOpenCodeRuntimeEffective(
	effective sandboxpolicy.EffectiveProfile,
) (sandboxpolicy.EffectiveProfile, error) {
	socketFloor := make(map[string]bool)
	for _, path := range sandboxpolicy.AgentdSocketFloor() {
		path = session.CanonicalTclaudeLayerGeneratedPath(path)
		if path != "" {
			socketFloor[path] = true
		}
	}
	withoutSocketFloor := effective
	withoutSocketFloor.Filesystem = make(
		[]sandboxpolicy.FilesystemGrant, 0, len(effective.Filesystem))
	for _, grant := range effective.Filesystem {
		if socketFloor[filepath.Clean(grant.Path)] {
			if grant.Access != sandboxpolicy.AccessRead {
				return sandboxpolicy.EffectiveProfile{}, fmt.Errorf(
					"revalidate OpenCode with tclaude’s sandbox launch spec: generated agentd socket floor %q has unexpected %s access",
					grant.Path, grant.Access)
			}
			continue
		}
		withoutSocketFloor.Filesystem = append(
			withoutSocketFloor.Filesystem, grant)
	}
	snapshot := sandboxpolicy.NewSnapshot(withoutSocketFloor, nil)
	revalidated, err := sandboxpolicy.RevalidateSnapshot(snapshot)
	if err != nil {
		return sandboxpolicy.EffectiveProfile{}, fmt.Errorf(
			"revalidate OpenCode with tclaude’s sandbox launch spec: %w", err)
	}
	revalidated.Effective.Filesystem =
		append([]sandboxpolicy.FilesystemGrant(nil), effective.Filesystem...)
	return revalidated.Effective, nil
}

// openCodeReadOnlyConfigBindSource answers the one question both the launch
// contract's validation and the pre-sandbox bootstrap ask of a contract: which
// directory, if any, serves this contract's config app directory read-only?
// The validator only needs to know that one exists; the bootstrap needs to know
// WHICH, because that is the directory the sandbox reads through the bind. They
// ask here rather than each deriving "the config bind" for itself.
//
// The LAST matching bind wins, because that is the one the sandbox serves.
func openCodeReadOnlyConfigBindSource(
	contract session.TclaudeLayerLaunchContract,
) string {
	if len(contract.StateDirs) != 4 {
		return ""
	}
	configTarget := canonicalOpenCodeRuntimePath(contract.StateDirs[2])
	if configTarget == "" {
		return ""
	}
	source := ""
	for _, bind := range contract.ReadOnlyBinds {
		if canonicalOpenCodeRuntimePath(bind.Target) == configTarget {
			source = canonicalOpenCodeRuntimePath(bind.Source)
		}
	}
	return source
}

func canonicalOpenCodeRuntimePath(path string) string {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." || !filepath.IsAbs(path) {
		return ""
	}
	return path
}
