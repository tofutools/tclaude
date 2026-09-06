package model

import "fmt"

const MaxProviderEvidenceBytes = 64 << 10

// ProviderEvidence is a bounded, versioned recovery envelope. Payload is
// provider-private data; application and transport code must not inspect it.
type ProviderEvidence struct {
	Provider string
	Version  uint32
	Payload  []byte
}

func NewProviderEvidence(provider string, version uint32, payload []byte) (ProviderEvidence, error) {
	if provider == "" {
		return ProviderEvidence{}, fmt.Errorf("provider is required")
	}
	if version == 0 {
		return ProviderEvidence{}, fmt.Errorf("provider evidence version is required")
	}
	if len(payload) > MaxProviderEvidenceBytes {
		return ProviderEvidence{}, fmt.Errorf("provider evidence exceeds %d bytes", MaxProviderEvidenceBytes)
	}
	return ProviderEvidence{Provider: provider, Version: version, Payload: append([]byte(nil), payload...)}, nil
}
