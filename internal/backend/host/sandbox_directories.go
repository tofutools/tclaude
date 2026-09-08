//go:build linux || darwin

package host

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/tofutools/tclaude/internal/backend/model"
	"golang.org/x/sys/unix"
)

// ForExecution binds generated cache ownership to a trusted admitted actor. A
// copy keeps concurrently prepared executions from changing one another's owner.
// Standalone shells, which have no agent, retain directories by execution ID.
func (p *SandboxLaunchPreparer) ForExecution(spec model.ResolvedExecutionSpec) *SandboxLaunchPreparer {
	copy := *p
	copy.directoryOwner = ""
	copy.directoryOwnerError = nil
	if spec.AgentID != "" {
		copy.directoryOwnerError = spec.AgentID.Validate()
		copy.directoryOwner = "agent-" + string(spec.AgentID)
	} else {
		copy.directoryOwnerError = spec.ExecutionID.Validate()
		copy.directoryOwner = "execution-" + string(spec.ExecutionID)
	}
	return &copy
}

func (p *SandboxLaunchPreparer) prepareAgentDirectories(ctx context.Context, names []string) (model.Environment, []SandboxProviderResource, error) {
	if len(names) == 0 {
		return nil, nil, nil
	}
	if p.directoryOwner == "" || p.directoryOwnerError != nil {
		return nil, nil, fmt.Errorf("generated sandbox directories require a valid admitted agent or execution identity")
	}
	if len(names) > 128 {
		return nil, nil, fmt.Errorf("too many generated sandbox directories")
	}
	environment := model.Environment{}
	for _, name := range names {
		if _, exists := environment[name]; exists {
			return nil, nil, fmt.Errorf("duplicate generated sandbox directory")
		}
		environment[name] = ""
	}
	if err := environment.Validate(); err != nil {
		return nil, nil, err
	}
	parent, err := unix.Open(p.config.Artifacts, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = unix.Close(parent) }()
	container, err := openGeneratedDirectory(parent, "agent-directories")
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = unix.Close(container) }()
	owner, err := openGeneratedDirectory(container, p.directoryOwner)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = unix.Close(owner) }()
	root := filepath.Join(p.config.Artifacts, "agent-directories", p.directoryOwner)
	var resources []SandboxProviderResource
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		directory, err := openGeneratedDirectory(owner, name)
		if err != nil {
			return nil, nil, err
		}
		_ = unix.Close(directory)
		path := filepath.Join(root, name)
		environment[name] = path
		if p.config.AgentDirectoriesMountIndividually {
			resources = append(resources, SandboxProviderResource{Path: path, Access: model.SandboxFilesystemWrite, rejectAliases: true, preserveDirectory: true})
		}
	}
	if !p.config.AgentDirectoriesMountIndividually {
		resources = append(resources, SandboxProviderResource{Path: root, Access: model.SandboxFilesystemWrite, rejectAliases: true})
	}
	// These are stable actor caches, not per-attempt artifacts. A failed or
	// aborted preparation must never delete another attempt's retained contents.
	return environment, resources, nil
}

func openGeneratedDirectory(parent int, name string) (int, error) {
	if err := unix.Mkdirat(parent, name, 0700); err != nil && err != unix.EEXIST {
		return -1, err
	}
	return unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
}
