package engine

import (
	"fmt"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

// ExecutionCapability is the monotonic process-engine capability vocabulary.
// No public constructor exposes parallel capabilities in this authoring-only
// slice: production callers can negotiate foundation_v1 only.
type ExecutionCapability string

const (
	CapabilityFoundationV1  ExecutionCapability = "foundation_v1"
	CapabilityParallelAllV1 ExecutionCapability = "parallel_all_v1"
	CapabilityParallelAnyV1 ExecutionCapability = "parallel_any_v1"
)

// EngineCapabilities is an opaque capability grant supplied by the engine
// hosting instantiation, never by a run-creation request body or template.
type EngineCapabilities struct {
	supported map[ExecutionCapability]struct{}
}

func FoundationEngineCapabilities() EngineCapabilities {
	return EngineCapabilities{supported: map[ExecutionCapability]struct{}{CapabilityFoundationV1: {}}}
}

func (c EngineCapabilities) Supports(capability ExecutionCapability) bool {
	_, ok := c.supported[capability]
	return ok
}

func (c EngineCapabilities) effective() EngineCapabilities {
	if len(c.supported) == 0 {
		// Preserve source compatibility for embedders while remaining fail
		// closed for every authored parallel/all-scope/any feature.
		return FoundationEngineCapabilities()
	}
	return c
}

func requireInstantiationCapabilities(tmpl *model.Template, capabilities EngineCapabilities) error {
	capabilities = capabilities.effective()
	if capabilities.Supports(CapabilityParallelAllV1) && !capabilities.Supports(CapabilityFoundationV1) {
		return fmt.Errorf("incoherent process engine capabilities: %s requires %s", CapabilityParallelAllV1, CapabilityFoundationV1)
	}
	if capabilities.Supports(CapabilityParallelAnyV1) &&
		(!capabilities.Supports(CapabilityParallelAllV1) || !capabilities.Supports(CapabilityFoundationV1)) {
		return fmt.Errorf("incoherent process engine capabilities: %s requires %s and %s",
			CapabilityParallelAnyV1, CapabilityParallelAllV1, CapabilityFoundationV1)
	}
	if !capabilities.Supports(CapabilityFoundationV1) {
		return fmt.Errorf("process engine does not provide %s", CapabilityFoundationV1)
	}
	hasParallel := false
	hasAnyJoin := false
	for _, node := range tmpl.Nodes {
		if node.Type == model.NodeTypeParallel {
			hasParallel = true
		}
		if node.Join == model.JoinAny {
			hasAnyJoin = true
		}
	}
	required := CapabilityFoundationV1
	if hasParallel {
		required = CapabilityParallelAllV1
		if hasAnyJoin {
			required = CapabilityParallelAnyV1
		}
	}
	if !capabilities.Supports(required) {
		return fmt.Errorf("template requires process engine capability %s", required)
	}
	return nil
}
