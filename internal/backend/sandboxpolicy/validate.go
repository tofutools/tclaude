package sandboxpolicy

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/tofutools/tclaude/internal/backend/model"
)

// Validate checks authoring syntax only. In particular, it neither resolves
// symlinks nor decides whether a path may intersect a protected host resource.
// Those checks require the host's explicit composition and admission boundary.
func Validate(p model.SandboxPolicy) error {
	if len(p.Includes) > 32 || len(p.Filesystem) > 512 || len(p.Tmpfs) > 128 || len(p.PreLaunch) > 32 {
		return fmt.Errorf("sandbox policy exceeds its include, filesystem, tmpfs, or setup entry limit")
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if len(raw) > 4<<20 {
		return fmt.Errorf("sandbox policy exceeds 4 MiB")
	}
	seenRefs := map[model.SandboxProfileID]bool{}
	for _, ref := range p.Includes {
		if err := ValidateRef(ref); err != nil {
			return err
		}
		if seenRefs[ref.ProfileID] {
			return fmt.Errorf("duplicate included sandbox profile")
		}
		seenRefs[ref.ProfileID] = true
	}
	switch p.FilesystemRoot {
	case model.SandboxRootAutomatic, model.SandboxRootInherit, model.SandboxRootSeparate:
	default:
		return fmt.Errorf("invalid filesystem root posture")
	}
	switch p.HarnessConfig {
	case model.SandboxHarnessConfigDefault, model.SandboxHarnessConfigRead, model.SandboxHarnessConfigWrite:
	default:
		return fmt.Errorf("invalid harness configuration access")
	}
	for _, rule := range p.Filesystem {
		if err := absolutePath(rule.HostPath); err != nil {
			return fmt.Errorf("filesystem host path: %w", err)
		}
		switch rule.Access {
		case model.SandboxFilesystemRead, model.SandboxFilesystemWrite, model.SandboxFilesystemDeny:
		default:
			return fmt.Errorf("invalid filesystem access")
		}
		switch rule.ExpectedKind {
		case "", "file", "directory":
		default:
			return fmt.Errorf("invalid filesystem expected kind")
		}
		if rule.Access == model.SandboxFilesystemDeny && rule.ExpectedKind == "file" {
			return fmt.Errorf("a deny rule requires a directory")
		}
		if rule.GuestPath != "" {
			if rule.GuestPath == "/" {
				return fmt.Errorf("a filesystem remap cannot replace the namespace root")
			}
			if rule.Access == model.SandboxFilesystemDeny {
				return fmt.Errorf("a deny rule cannot remap a host path")
			}
			if err := absolutePath(rule.GuestPath); err != nil {
				return fmt.Errorf("filesystem guest path: %w", err)
			}
		}
	}
	for _, mount := range p.Tmpfs {
		if err := absolutePath(mount.GuestPath); err != nil {
			return fmt.Errorf("tmpfs guest path: %w", err)
		}
		if mount.GuestPath == "/" {
			return fmt.Errorf("tmpfs cannot replace the namespace root")
		}
		if mount.Size != "" {
			if _, err := parseByteQuantity("tmpfs size", mount.Size); err != nil {
				return err
			}
		}
	}
	if err := p.Environment.Validate(); err != nil {
		return err
	}
	if len(p.AgentDirectories)+len(p.Environment) > 128 {
		return fmt.Errorf("environment and agent directories exceed 128 entries")
	}
	seenNames := map[string]bool{}
	for _, name := range p.AgentDirectories {
		if err := (model.Environment{name: ""}).Validate(); err != nil {
			return err
		}
		if _, exists := p.Environment[name]; exists {
			return fmt.Errorf("agent directory %q also has a literal environment value", name)
		}
		if seenNames[name] {
			return fmt.Errorf("duplicate agent directory %q", name)
		}
		seenNames[name] = true
	}
	if err := validateNetwork(p.Network); err != nil {
		return err
	}
	if err := validateSockets(p.UnixSockets); err != nil {
		return err
	}
	if p.Resources.Memory != "" {
		if _, err := ParseMemoryLimitBytes(p.Resources.Memory); err != nil {
			return err
		}
	}
	if p.Resources.CPU != "" {
		if _, err := CPUQuotaMicros(p.Resources.CPU); err != nil {
			return err
		}
	}
	seenBlocks := map[string]bool{}
	for _, block := range p.PreLaunch {
		if len(block.Name) > 128 || !setupName.MatchString(block.Name) {
			return fmt.Errorf("invalid setup block name")
		}
		if seenBlocks[block.Name] {
			return fmt.Errorf("duplicate setup block %q", block.Name)
		}
		seenBlocks[block.Name] = true
		if strings.TrimSpace(block.Script) == "" || len(block.Script) > 65536 || !literal(block.Script) {
			return fmt.Errorf("invalid setup script for %q", block.Name)
		}
		if len(block.Exports) > 64 {
			return fmt.Errorf("setup block exports exceed 64 entries")
		}
		for _, name := range block.Exports {
			// Exports describe an executable block, so PATH and similar names are
			// deliberately permitted, as in the retained setup-script contract.
			if len(name) > 128 || !environmentName.MatchString(name) {
				return fmt.Errorf("invalid setup export name")
			}
		}
	}
	return nil
}

func ValidateRef(ref model.SandboxProfileRef) error {
	if err := ref.ProfileID.Validate(); err != nil {
		return err
	}
	if ref.RevisionID == "" && ref.ContentHash == "" {
		return nil
	}
	if err := ref.RevisionID.Validate(); err != nil {
		return err
	}
	hash, err := hex.DecodeString(ref.ContentHash)
	if err != nil || len(hash) != 32 || ref.ContentHash != strings.ToLower(ref.ContentHash) {
		return fmt.Errorf("sandbox reference requires a lowercase SHA-256 content hash")
	}
	return nil
}

var setupName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
var environmentName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func literal(value string) bool { return utf8.ValidString(value) && !strings.ContainsRune(value, 0) }

func absolutePath(value string) error {
	if value == "" || len(value) > 4096 || !literal(value) || !path.IsAbs(value) || path.Clean(value) != value {
		return fmt.Errorf("requires a clean absolute path of at most 4096 bytes")
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return fmt.Errorf("path contains a control character")
		}
	}
	return nil
}

func validateSockets(s *model.SandboxUnixSockets) error {
	if s == nil {
		return nil
	}
	switch s.Mode {
	case "", "open", "closed", "list":
	default:
		return fmt.Errorf("invalid Unix socket mode")
	}
	if s.Mode != "list" && len(s.Allow) > 0 {
		return fmt.Errorf("socket selectors require list mode")
	}
	if len(s.Allow) > 64 {
		return fmt.Errorf("socket selectors exceed 64 entries")
	}
	for _, selector := range s.Allow {
		if (selector.Path == "") == (selector.PathGlob == "") {
			return fmt.Errorf("socket selector requires exactly one path or path glob")
		}
		value := selector.Path
		if value == "" {
			value = selector.PathGlob
		}
		if err := absolutePath(value); err != nil {
			return err
		}
		if selector.PathGlob != "" {
			if strings.Contains(value, "**") || strings.ContainsAny(value, "?[]\\") {
				return fmt.Errorf("socket glob permits only single-segment * matching")
			}
			count := 0
			for _, segment := range strings.Split(value, "/") {
				if strings.Contains(segment, "*") {
					count++
				}
			}
			if count != 1 {
				return fmt.Errorf("socket glob requires * in exactly one segment")
			}
		}
	}
	return nil
}
