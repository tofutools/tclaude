package legacy

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/managedinstructions"
	"github.com/tofutools/tclaude/pkg/paf"
)

// ManagedInstructionsExtension is required only when items actually travel.
// Older importers therefore refuse instead of silently dropping instructions;
// ordinary archives retain their original declarations and compatibility.
const ManagedInstructionsExtension = "org.tclaude.managed-instructions"

type ManagedInstructionIndex struct {
	Version int                           `json:"version"`
	Items   []ManagedInstructionReference `json:"items"`
}
type ManagedInstructionReference struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Bytes  int    `json:"bytes"`
}

func writeManagedInstructions(w *paf.Writer, agent *paf.Agent, env *paf.Environment, resources []db.ManagedInstruction) error {
	if len(resources) == 0 {
		return nil
	}
	if len(resources) > 512 {
		return errors.New("managed instructions exceed 512 items")
	}
	if err := w.DeclareExtension(ManagedInstructionsExtension, paf.ExtensionDecl{Version: "1.0", System: "tclaude", Spec: "https://github.com/tofutools/tclaude/blob/main/docs/paf.md#managed-instructions", Copy: "unsafe"}, true); err != nil {
		return err
	}
	index := ManagedInstructionIndex{Version: 1}
	total := 0
	seen := map[string]bool{}
	for _, resource := range resources {
		if err := managedinstructions.Validate(resource.Kind, resource.Name, resource.Content); err != nil {
			return err
		}
		key := resource.Kind + ":" + resource.Name
		if resource.Kind == "rule" {
			key += ":" + managedinstructions.ContentDigest(resource.Content)
		}
		if seen[key] {
			return fmt.Errorf("duplicate managed instruction %q", key)
		}
		seen[key] = true
		total += len(resource.Content)
		if total > db.ManagedInstructionAgentLimit {
			return errors.New("managed instructions exceed 16 MiB")
		}
		sum := sha256.Sum256(resource.Content)
		digest := hex.EncodeToString(sum[:])
		index.Items = append(index.Items, ManagedInstructionReference{Kind: resource.Kind, Name: resource.Name, SHA256: digest, Bytes: len(resource.Content)})
		// Performers of one bundle can share a resource.
		if _, err := w.AddAttachment(resource.Content, "application/octet-stream"); err != nil {
			return err
		}
		if resource.Kind == "skill" {
			env.Skills = append(env.Skills, paf.SkillRef{Name: resource.Name, Source: "attachment:" + digest, SHA256: digest})
		}
	}
	return agent.Extensions.Set(ManagedInstructionsExtension, index)
}

func readManagedInstructions(a *paf.Archive, agent paf.Agent) ([]db.ManagedInstruction, error) {
	var index ManagedInstructionIndex
	present, err := agent.Extensions.Decode(ManagedInstructionsExtension, &index)
	if err != nil {
		return nil, err
	}
	if !present {
		for _, required := range a.Manifest.ExtensionsRequired {
			if required == ManagedInstructionsExtension {
				return nil, errors.New("required managed instruction index is missing")
			}
		}
		return nil, nil
	}
	if !a.UnderstandsExtension(ManagedInstructionsExtension) || index.Version != 1 {
		return nil, errors.New("unsupported managed instruction resource index")
	}
	if len(index.Items) == 0 || len(index.Items) > 512 {
		return nil, errors.New("invalid managed instruction item count")
	}
	resources := []db.ManagedInstruction{}
	seen := map[string]bool{}
	total := 0
	for _, item := range index.Items {
		if item.Bytes < 1 || item.Bytes > db.ManagedInstructionItemLimit || len(item.SHA256) != 64 {
			return nil, errors.New("invalid managed instruction reference")
		}
		digest, err := hex.DecodeString(item.SHA256)
		if err != nil || len(digest) != sha256.Size {
			return nil, errors.New("invalid managed instruction digest")
		}
		total += item.Bytes
		if total > db.ManagedInstructionAgentLimit {
			return nil, errors.New("managed instructions exceed 16 MiB")
		}
		key := item.Kind + ":" + item.Name
		if item.Kind == "rule" {
			key += ":" + item.SHA256
		}
		if seen[key] {
			return nil, fmt.Errorf("duplicate managed instruction %q", key)
		}
		seen[key] = true
		raw, err := a.ReadEntry(paf.AttachmentsDir+item.SHA256, int64(item.Bytes))
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(raw)
		if len(raw) != item.Bytes || hex.EncodeToString(sum[:]) != item.SHA256 {
			return nil, errors.New("managed instruction attachment digest or size mismatch")
		}
		if err := managedinstructions.Validate(item.Kind, item.Name, raw); err != nil {
			return nil, err
		}
		resources = append(resources, db.ManagedInstruction{Kind: item.Kind, Name: item.Name, Digest: item.SHA256, Content: raw, Scope: "carried", Carried: true})
	}
	return resources, nil
}
