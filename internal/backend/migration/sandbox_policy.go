package migration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	sourcev228 "github.com/tofutools/tclaude/internal/backend/migration/source/v228"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/sandboxpolicy"
)

// The source structs deliberately name every accepted v228 JSON field. Unknown
// policy fields keep the complete source row pending; they are never discarded
// while publishing a seemingly complete editable replacement policy.
type legacySandboxFilesystem struct {
	Path      string                        `json:"path"`
	Access    model.SandboxFilesystemAccess `json:"access"`
	MountPath string                        `json:"mount_path"`
	Kind      string                        `json:"kind"`
}
type legacySandboxDestination struct {
	Host              string   `json:"host"`
	Domain            string   `json:"domain"`
	IncludeSubdomains bool     `json:"include_subdomains"`
	CIDR              string   `json:"cidr"`
	Loopback          bool     `json:"loopback"`
	Ports             []uint16 `json:"ports"`
}
type legacySandboxNetwork struct {
	Mode      string                       `json:"mode"`
	Baseline  model.SandboxNetworkBaseline `json:"baseline"`
	Packs     []string                     `json:"packs"`
	DenyPacks []string                     `json:"deny_packs"`
	Allow     []legacySandboxDestination   `json:"allow"`
	Deny      []legacySandboxDestination   `json:"deny"`
	Namespace string                       `json:"namespace"`
	Engine    model.SandboxNetworkEngine   `json:"engine"`
}

func decodeSandboxField(row sourcev228.Row, field string, out any) error {
	raw := sourcev228.String(row.Values[field])
	if raw == "" {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return fmt.Errorf("%s: %w", field, err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("%s contains trailing data", field)
	}
	return nil
}

func legacySandboxPolicy(row sourcev228.Row) (model.SandboxPolicy, []string, error) {
	var out model.SandboxPolicy
	known := map[string]bool{}
	for _, field := range []string{"id", "name", "created_at", "updated_at", "filesystem_json", "environment_json", "includes_json", "tmpfs_json", "agent_directories_json", "resource_limits_json", "pre_launch_json", "filesystem_spellings_json", "unix_sockets_json", "network_json", "filesystem_root", "harness_config", "darwin_allow_mach_register", "network_access"} {
		known[field] = true
	}
	for field := range row.Values {
		if !known[field] {
			return out, nil, fmt.Errorf("unrecognized sandbox source column")
		}
	}
	var includes []string
	var filesystem []legacySandboxFilesystem
	var environment []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	var tmpfs []struct {
		Path      string `json:"path"`
		Size      string `json:"size"`
		SizeBytes uint64 `json:"size_bytes"`
	}
	var resources struct {
		Memory      string          `json:"memory"`
		MemoryBytes uint64          `json:"memory_bytes"`
		CPU         json.RawMessage `json:"cpu"`
	}
	var setup []struct {
		Name    string   `json:"name"`
		Script  string   `json:"script"`
		Exports []string `json:"exports"`
	}
	var spelling *struct {
		Version int               `json:"version"`
		Rules   []json.RawMessage `json:"rules"`
	}
	var sockets *struct {
		Mode  string `json:"mode"`
		Allow []struct {
			Path     string `json:"path"`
			PathGlob string `json:"path_glob"`
		} `json:"allow"`
	}
	var network *legacySandboxNetwork
	for _, field := range []struct {
		name  string
		value any
	}{
		{"filesystem_json", &filesystem}, {"environment_json", &environment}, {"includes_json", &includes},
		{"tmpfs_json", &tmpfs}, {"agent_directories_json", &out.AgentDirectories}, {"resource_limits_json", &resources},
		{"pre_launch_json", &setup}, {"filesystem_spellings_json", &spelling}, {"unix_sockets_json", &sockets}, {"network_json", &network},
	} {
		if err := decodeSandboxField(row, field.name, field.value); err != nil {
			return out, nil, err
		}
	}
	if spelling != nil && (spelling.Version != 1 || len(spelling.Rules) != 0) {
		return out, nil, fmt.Errorf("filesystem spelling aliases require a target alias representation")
	}
	for _, rule := range filesystem {
		out.Filesystem = append(out.Filesystem, model.SandboxFilesystemRule{HostPath: rule.Path, GuestPath: rule.MountPath, Access: rule.Access, ExpectedKind: rule.Kind})
	}
	if len(environment) > 0 {
		out.Environment = model.Environment{}
	}
	for _, entry := range environment {
		if _, exists := out.Environment[entry.Name]; exists {
			return out, nil, fmt.Errorf("duplicate environment variable")
		}
		out.Environment[entry.Name] = entry.Value
	}
	for _, mount := range tmpfs {
		if err := legacyDerivedBytes(mount.Size, mount.SizeBytes); err != nil {
			return out, nil, err
		}
		out.Tmpfs = append(out.Tmpfs, model.SandboxTmpfs{GuestPath: mount.Path, Size: mount.Size})
	}
	if err := legacyDerivedBytes(resources.Memory, resources.MemoryBytes); err != nil {
		return out, nil, err
	}
	out.Resources = model.SandboxResources{Memory: resources.Memory}
	if len(resources.CPU) != 0 && string(resources.CPU) != "null" {
		decoder := json.NewDecoder(bytes.NewReader(resources.CPU))
		decoder.UseNumber()
		var value any
		if err := decoder.Decode(&value); err != nil {
			return out, nil, err
		}
		number, ok := value.(json.Number)
		if !ok {
			return out, nil, fmt.Errorf("CPU must be a JSON number")
		}
		out.Resources.CPU = number.String()
	}
	for _, block := range setup {
		out.PreLaunch = append(out.PreLaunch, model.SandboxSetupBlock{Name: block.Name, Script: block.Script, Exports: block.Exports})
	}
	out.FilesystemRoot = model.SandboxFilesystemRoot(sourcev228.String(row.Values["filesystem_root"]))
	out.HarnessConfig = model.SandboxHarnessConfig(sourcev228.String(row.Values["harness_config"]))
	if raw := row.Values["darwin_allow_mach_register"]; raw != nil {
		enabled, ok := sourcev228.Int64(raw)
		if !ok || enabled < 0 || enabled > 1 {
			return out, nil, fmt.Errorf("invalid Mach registration flag")
		}
		out.DarwinAllowMachRegister = enabled == 1
	}
	if sockets != nil {
		out.UnixSockets = &model.SandboxUnixSockets{Mode: sockets.Mode}
		for _, selector := range sockets.Allow {
			out.UnixSockets.Allow = append(out.UnixSockets.Allow, model.SandboxSocketSelector{Path: selector.Path, PathGlob: selector.PathGlob})
		}
	}
	legacy := sourcev228.String(row.Values["network_access"])
	if network == nil {
		switch legacy {
		case "":
		case "internet":
			out.Network = &model.SandboxNetwork{Baseline: model.SandboxNetworkAllow}
		case "none":
			out.Network = &model.SandboxNetwork{Baseline: model.SandboxNetworkDeny}
			if out.UnixSockets == nil {
				out.UnixSockets = &model.SandboxUnixSockets{Mode: "closed"}
			}
		default:
			return out, nil, fmt.Errorf("unrecognized legacy network posture")
		}
	} else {
		if network.Mode != "" && network.Baseline != "" {
			return out, nil, fmt.Errorf("dual legacy network representations require explicit reconciliation")
		}
		baseline := network.Baseline
		if baseline == "" {
			if len(network.Packs) > 0 || len(network.DenyPacks) > 0 || len(network.Deny) > 0 || network.Mode != "list" && len(network.Allow) > 0 {
				return out, nil, fmt.Errorf("legacy mode cannot carry compositional destination rows")
			}
			switch network.Mode {
			case "":
				baseline = model.SandboxNetworkInherit
			case "open":
				baseline = model.SandboxNetworkAllow
			case "closed", "list":
				baseline = model.SandboxNetworkDeny
			default:
				return out, nil, fmt.Errorf("unrecognized network mode")
			}
		}
		agrees := legacy == "" || (legacy == "internet" && baseline == model.SandboxNetworkAllow && len(network.Deny) == 0 && len(network.DenyPacks) == 0) || (legacy == "none" && baseline == model.SandboxNetworkDeny && len(network.Allow) == 0 && len(network.Packs) == 0)
		if !agrees {
			return out, nil, fmt.Errorf("conflicting legacy network posture")
		}
		out.Network = &model.SandboxNetwork{Baseline: baseline, Packs: network.Packs, DenyPacks: network.DenyPacks, Namespace: network.Namespace, Engine: network.Engine}
		convert := func(entries []legacySandboxDestination) []model.SandboxDestination {
			var result []model.SandboxDestination
			for _, entry := range entries {
				result = append(result, model.SandboxDestination{Host: entry.Host, Domain: entry.Domain, IncludeSubdomains: entry.IncludeSubdomains, CIDR: entry.CIDR, Loopback: entry.Loopback, Ports: entry.Ports})
			}
			return result
		}
		out.Network.Allow, out.Network.Deny = convert(network.Allow), convert(network.Deny)
	}
	return out, includes, nil
}

func legacyDerivedBytes(authored string, derived uint64) error {
	if derived == 0 {
		return nil
	}
	actual, err := sandboxpolicy.ParseMemoryLimitBytes(authored)
	if err != nil || actual != derived {
		return fmt.Errorf("derived byte quantity disagrees with authored quantity")
	}
	return nil
}
