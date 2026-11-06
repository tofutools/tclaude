package agentd

import (
	"errors"
	"fmt"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	tclcommon "github.com/tofutools/tclaude/pkg/common"
)

// startManagedOpenCodeServer starts the agentd-managed OpenCode server a
// relaunch of an existing agent attaches to, and fills the launch args the
// forked `tclaude session new` (or resume) needs to reach it. Resume and
// reincarnation share it; a fresh spawn has its own preparation
// (prepareManagedServer) because it also allocates private state and
// installs imported history around the server start.
//
// runtimeKey names the runtime and its resource cgroup (the resumed conv-id,
// or a successor's session label). resumeID is the conversation the server
// reopens; empty asks it to mint a new ses_ conversation, which the returned
// launch carries as ConvID. A tclaude-layer launch needs agentID and the
// agent's existing private-state allocation. On error nothing is left running.
func startManagedOpenCodeServer(a *clcommon.SpawnArgs, runtimeKey, agentID, title, resumeID string,
	resuming bool) (*openCodeLaunch, error) {
	resolvedCwd, err := resolveOpenCodeLaunchCwd(a.Cwd)
	if err != nil {
		return nil, err
	}
	a.Cwd = resolvedCwd
	permissionJSON, err := openCodePermissionJSONForLaunch(
		a.Cwd, a.Sandbox, a.Approval, a.ToolGovernance, a.EffectiveSandbox)
	if err != nil {
		return nil, err
	}
	implementation, err := sandboxpolicy.NormalizeImplementation(a.SandboxImplementation)
	if err != nil {
		return nil, err
	}
	if agentID == "" && implementation == sandboxpolicy.ImplementationTclaudeLayer {
		return nil, fmt.Errorf("OpenCode with tclaude’s sandbox has no stable agent identity to relaunch")
	}
	sandboxSpec, err := openCodeTclaudeLayerLaunchSpec(
		a.SandboxImplementation,
		a.Cwd,
		a.GitWorktreeWriteDirs,
		a.EffectiveSandbox,
		agentID,
		runtimeKey,
	)
	if err != nil {
		return nil, err
	}
	resourceCgroupDir, resourceCleanup, err := prepareManagedServerResourceCgroup(
		runtimeKey, a.EffectiveSandbox, a.SandboxImplementation, a.AllowUnenforcedSandbox, resuming)
	if err != nil {
		return nil, err
	}
	launch, err := startOpenCodeRuntimeForSpawn(
		runtimeKey, a.Cwd, title, resumeID, permissionJSON,
		a.SandboxImplementation, sandboxSpec, resourceCgroupDir)
	if err != nil && resourceCgroupDir != "" &&
		errors.Is(err, errOpenCodeResourceCgroup) &&
		degradeManagedServerResourceCgroup(
			a.EffectiveSandbox, a.SandboxImplementation, a.AllowUnenforcedSandbox, resuming, err) {
		resourceCleanup()
		resourceCgroupDir = ""
		launch, err = startOpenCodeRuntimeForSpawn(
			runtimeKey, a.Cwd, title, resumeID, permissionJSON,
			a.SandboxImplementation, sandboxSpec, "")
	}
	if err != nil {
		resourceCleanup()
		return nil, err
	}
	a.OpenCodeServerURL = launch.ServerURL
	a.OpenCodeServerPassword = launch.Password
	a.OpenCodeTransport = launch.Transport
	a.OpenCodeControlSocketPath = launch.ControlSocketPath
	a.OpenCodeControlSocketDevice = launch.ControlSocketDevice
	a.OpenCodeControlSocketInode = launch.ControlSocketInode
	a.OpenCodeServerPID = launch.PID
	a.ResourceCgroupDir = resourceCgroupDir
	if sandboxSpec != nil {
		a.OpenCodeEnvironment = append(
			[]sandboxpolicy.EnvironmentEntry(nil), sandboxSpec.Contract.Environment...)
		allocation, allocationErr := requireOpenCodeStateAllocation(agentID)
		if allocationErr != nil {
			// The runtime is already serving this launch; stop it, or it
			// outlives a relaunch that never gets a pane.
			_ = stopOpenCodeRuntime(launch.SessionID)
			return nil, allocationErr
		}
		a.OpenCodeStateIsolation = allocation.Mode
	}
	return launch, nil
}

// prepareOpenCodeLaunchAttachments creates a new launch's (a reincarnation
// successor's, a clone's) private attachments directory before its OpenCode
// server starts. Under tclaude's sandbox the server's launch spec grants that
// directory and requires it to exist; spawn reserves it with the label and
// resume prepares it the same way. created reports whether this call made it.
func prepareOpenCodeLaunchAttachments(rawImplementation, label string, created *bool) error {
	implementation, err := sandboxpolicy.NormalizeImplementation(rawImplementation)
	if err != nil {
		return err
	}
	if implementation != sandboxpolicy.ImplementationTclaudeLayer {
		return nil
	}
	_, made, err := tclcommon.PrepareSpawnAttachmentsPrivateDir(label)
	if err != nil {
		return err
	}
	*created = made
	return nil
}
