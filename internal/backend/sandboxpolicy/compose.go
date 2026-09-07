package sandboxpolicy

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"

	"github.com/tofutools/tclaude/internal/backend/model"
)

// HostPathResolver is supplied by trusted host composition. It resolves only
// host spelling; it cannot replace access, guest path or expected-kind intent.
// A composed result is not a release receipt: host identity, kind, protected
// roots and implementation capabilities must still be checked at admission.
type HostPathResolver interface {
	ResolveSandboxHostPath(context.Context, string) (string, error)
}

type NetworkConjunct struct {
	Source model.SandboxProfileRef
	Policy model.SandboxNetwork
}
type NetworkEngineChoice struct {
	Source model.SandboxProfileRef
	Engine model.SandboxNetworkEngine
}

type SocketConjunct struct {
	Source model.SandboxProfileRef
	Policy model.SandboxUnixSockets
}

// Composition is an intermediate include result, never an authored profile or
// enforcement claim. Every network/socket conjunct must hold. Keeping these
// axes as conjunctions avoids lossy expansion of domain/CIDR/glob intersections.
// Values contains only the remaining axes; Includes/Network/UnixSockets are nil.
type Composition struct {
	// Mechanism selection has include precedence; it is not an access conjunct.
	NetworkEngine    *NetworkEngineChoice
	NetworkNamespace string
	Root             model.SandboxProfileRef
	Values           model.SandboxPolicy
	NetworkAll       []NetworkConjunct
	SocketAll        []SocketConjunct
}

// ComposeIncludes resolves exact content first, then memoizes composition per
// graph node. Applying unique closure entries as a flat sequence is incorrect:
// a later sibling must bring its own inherited values back into precedence.
func ComposeIncludes(ctx context.Context, root model.SandboxProfileRef, reader RevisionReader, paths HostPathResolver) (Composition, error) {
	closure, err := Resolve(ctx, root, reader)
	if err != nil {
		return Composition{}, err
	}
	memo := map[model.SandboxProfileRevisionID]Composition{}
	total := 0
	for _, entry := range closure.Entries {
		if err := ctx.Err(); err != nil {
			return Composition{}, err
		}
		out := Composition{Root: entry.Ref, Values: model.SandboxPolicy{Environment: model.Environment{}}}
		for _, ref := range entry.Policy.Includes {
			if err := mergeComposition(&out, memo[ref.RevisionID]); err != nil {
				return Composition{}, err
			}
		}
		own := Composition{Root: entry.Ref, Values: entry.Policy}
		own.Values.Includes = nil
		own.Values.Network = nil
		own.Values.UnixSockets = nil
		own.Values.Filesystem = slices.Clone(entry.Policy.Filesystem)
		for i, rule := range own.Values.Filesystem {
			if paths == nil {
				return Composition{}, invalidClosure("filesystem composition requires a host path resolver")
			}
			canonical, err := paths.ResolveSandboxHostPath(ctx, rule.HostPath)
			if err != nil {
				return Composition{}, err
			}
			if !filepath.IsAbs(canonical) || filepath.Clean(canonical) != canonical {
				return Composition{}, invalidClosure("host resolver returned an invalid canonical path")
			}
			own.Values.Filesystem[i].HostPath = canonical
			// Unremapped paths retain the canonical guest key, as in host admission.
		}
		if entry.Policy.Network != nil {
			own.NetworkAll = []NetworkConjunct{{entry.Ref, *entry.Policy.Network}}
			if entry.Policy.Network.Engine != "" {
				own.NetworkEngine = &NetworkEngineChoice{entry.Ref, entry.Policy.Network.Engine}
			}
			own.NetworkNamespace = entry.Policy.Network.Namespace
		}
		if entry.Policy.UnixSockets != nil {
			own.SocketAll = []SocketConjunct{{entry.Ref, *entry.Policy.UnixSockets}}
		}
		if err := mergeComposition(&out, own); err != nil {
			return Composition{}, err
		}
		if err := validateCompositionValues(out.Values); err != nil {
			return Composition{}, err
		}
		encoded, err := json.Marshal(out)
		if err != nil {
			return Composition{}, err
		}
		total += len(encoded)
		if len(encoded) > 4<<20 || total > 64<<20 {
			return Composition{}, invalidClosure("sandbox composition exceeds its bounded result budget")
		}
		// Detach each cached result: later sibling overrides must not mutate it.
		var detached Composition
		if err := json.Unmarshal(encoded, &detached); err != nil {
			return Composition{}, err
		}
		memo[entry.Ref.RevisionID] = detached
	}
	return memo[root.RevisionID], nil
}

func mergeComposition(out *Composition, next Composition) error {
	dst, src := &out.Values, next.Values
	if next.NetworkEngine != nil {
		choice := *next.NetworkEngine
		out.NetworkEngine = &choice
	}
	if out.NetworkNamespace != "private" && next.NetworkNamespace != "" {
		out.NetworkNamespace = next.NetworkNamespace
	}
	for _, rule := range src.Filesystem {
		key := guestKey(rule)
		index := slices.IndexFunc(dst.Filesystem, func(old model.SandboxFilesystemRule) bool { return guestKey(old) == key })
		if index < 0 {
			dst.Filesystem = append(dst.Filesystem, rule)
			continue
		}
		old := dst.Filesystem[index]
		if old.HostPath != rule.HostPath {
			return invalidClosure(fmt.Sprintf("guest path %q is claimed by different host paths", key))
		}
		if old.ExpectedKind != "" && rule.ExpectedKind != "" && old.ExpectedKind != rule.ExpectedKind {
			return invalidClosure("included filesystem kind commitments conflict")
		}
		if rule.ExpectedKind == "" {
			rule.ExpectedKind = old.ExpectedKind
		}
		dst.Filesystem[index] = rule
	}
	for _, mount := range src.Tmpfs {
		index := slices.IndexFunc(dst.Tmpfs, func(old model.SandboxTmpfs) bool { return old.GuestPath == mount.GuestPath })
		if index < 0 {
			dst.Tmpfs = append(dst.Tmpfs, mount)
		} else {
			dst.Tmpfs[index] = mount
		}
	}
	for name, value := range src.Environment {
		dst.Environment[name] = value
		dst.AgentDirectories = slices.DeleteFunc(dst.AgentDirectories, func(old string) bool { return old == name })
	}
	for _, name := range src.AgentDirectories {
		delete(dst.Environment, name)
		if !slices.Contains(dst.AgentDirectories, name) {
			dst.AgentDirectories = append(dst.AgentDirectories, name)
		}
	}
	if rootRank(src.FilesystemRoot) > rootRank(dst.FilesystemRoot) {
		dst.FilesystemRoot = src.FilesystemRoot
	}
	if configRank(src.HarnessConfig) > configRank(dst.HarnessConfig) {
		dst.HarnessConfig = src.HarnessConfig
	}
	if src.Resources.Memory != "" {
		dst.Resources.Memory = src.Resources.Memory
	}
	if src.Resources.CPU != "" {
		dst.Resources.CPU = src.Resources.CPU
	}
	dst.DarwinAllowMachRegister = dst.DarwinAllowMachRegister || src.DarwinAllowMachRegister
	for _, block := range src.PreLaunch {
		index := slices.IndexFunc(dst.PreLaunch, func(old model.SandboxSetupBlock) bool { return old.Name == block.Name })
		if index < 0 {
			dst.PreLaunch = append(dst.PreLaunch, block)
		} else {
			dst.PreLaunch[index] = block
		}
	}
	for _, term := range next.NetworkAll {
		if !slices.ContainsFunc(out.NetworkAll, func(old NetworkConjunct) bool { return old.Source == term.Source }) {
			out.NetworkAll = append(out.NetworkAll, term)
		}
	}
	for _, term := range next.SocketAll {
		if !slices.ContainsFunc(out.SocketAll, func(old SocketConjunct) bool { return old.Source == term.Source }) {
			out.SocketAll = append(out.SocketAll, term)
		}
	}
	return nil
}
func guestKey(rule model.SandboxFilesystemRule) string {
	if rule.GuestPath != "" {
		return rule.GuestPath
	}
	return rule.HostPath
}
func rootRank(value model.SandboxFilesystemRoot) int {
	switch value {
	case model.SandboxRootSeparate:
		return 2
	case model.SandboxRootInherit:
		return 1
	default:
		return 0
	}
}
func configRank(value model.SandboxHarnessConfig) int {
	switch value {
	case model.SandboxHarnessConfigRead:
		return 2
	case model.SandboxHarnessConfigWrite:
		return 1
	default:
		return 0
	}
}

func validateCompositionValues(values model.SandboxPolicy) error {
	if err := Validate(values); err != nil {
		return invalidClosure(err.Error())
	}
	guests := make(map[string]bool, len(values.Filesystem))
	for _, rule := range values.Filesystem {
		guests[guestKey(rule)] = true
	}
	for _, mount := range values.Tmpfs {
		if guests[mount.GuestPath] {
			return invalidClosure("filesystem and tmpfs claim the same guest path")
		}
	}
	return nil
}
