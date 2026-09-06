package providers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

type registryProvider string

func (p registryProvider) Name() string { return string(p) }
func (p registryProvider) Capabilities() ports.ProviderCapabilities {
	return ports.ProviderCapabilities{}
}
func (registryProvider) Prepare(context.Context, ports.PreparationRequest) (ports.PreparedAttempt, error) {
	return nil, nil
}
func (registryProvider) Recover(context.Context, ports.RecoveryRequest) (ports.RecoveryResult, error) {
	return ports.RecoveryResult{}, nil
}

func TestRegistrySelectsOneCohesiveProvider(t *testing.T) {
	registry := NewRegistry(registryProvider("claude"), registryProvider("opencode"))
	provider, ok := registry.Provider("opencode")
	require.True(t, ok)
	require.Equal(t, "opencode", provider.Name())
	_, ok = registry.Provider("unknown")
	require.False(t, ok)
	var _ ports.ProviderRegistry = registry
	_ = model.ExecutionID("")
}

func TestRegistryRejectsDuplicateProvider(t *testing.T) {
	require.Panics(t, func() { NewRegistry(registryProvider("claude"), registryProvider("claude")) })
}
