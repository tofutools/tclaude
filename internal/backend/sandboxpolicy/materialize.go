package sandboxpolicy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/tofutools/tclaude/internal/backend/model"
)

// PolicyMaterialization pins resolved authoring inputs and expanded destination
// packs. It is neither an authority grant nor an OS enforcement receipt. Host
// identity, provider resources and platform capabilities require release checks.
// Version identifies this serialized identity format independently of profiles.
type PolicyMaterialization struct {
	Version     int
	Scopes      []ScopeSelection
	Composition Composition
	Packs       []NetworkPackPin
	ContentHash string
}

// NetworkPackPin retains executable destination content without mutable labels.
type NetworkPackPin struct {
	ID          string
	ContentHash string
	Entries     []model.SandboxDestination
}

func MaterializeIncludes(ctx context.Context, root model.SandboxProfileRef, reader RevisionReader, paths HostPathResolver) (PolicyMaterialization, error) {
	composed, err := ComposeIncludes(ctx, root, reader, paths)
	if err != nil {
		return PolicyMaterialization{}, err
	}
	return materialize(composed, nil, NetworkPackCatalog())
}

func MaterializeScopes(ctx context.Context, selections []ScopeSelection, reader RevisionReader, paths HostPathResolver) (PolicyMaterialization, error) {
	composed, err := ComposeScopes(ctx, selections, reader, paths)
	if err != nil {
		return PolicyMaterialization{}, err
	}
	return materialize(composed.Combined, composed.Applied, NetworkPackCatalog())
}

func materialize(composed Composition, scopes []ScopeSelection, catalog []NetworkPack) (PolicyMaterialization, error) {
	// Detach before expanding: a preview must not rewrite the authored document or
	// a shared include. JSON also gives map keys a stable order for the identity.
	encoded, err := json.Marshal(composed)
	if err != nil {
		return PolicyMaterialization{}, err
	}
	out := PolicyMaterialization{Version: 1, Scopes: scopes}
	if err := json.Unmarshal(encoded, &out.Composition); err != nil {
		return PolicyMaterialization{}, err
	}
	seen := map[string]bool{}
	for i := range out.Composition.NetworkAll {
		policy := &out.Composition.NetworkAll[i].Policy
		for _, polarity := range []struct {
			ids          []string
			destinations *[]model.SandboxDestination
		}{{policy.Packs, &policy.Allow}, {policy.DenyPacks, &policy.Deny}} {
			for _, id := range polarity.ids {
				var found *NetworkPack
				for j := range catalog {
					if catalog[j].ID == id {
						found = &catalog[j]
						break
					}
				}
				if found == nil {
					return PolicyMaterialization{}, invalidClosure("destination pack is unavailable during materialization")
				}
				*polarity.destinations = append(*polarity.destinations, found.Entries...)
				if !seen[id] {
					out.Packs = append(out.Packs, NetworkPackPin{ID: found.ID, ContentHash: found.ContentHash, Entries: found.Entries})
					seen[id] = true
				}
			}
		}
		policy.Packs, policy.DenyPacks = nil, nil
	}
	// The entire versioned document, including canonical paths, exact source
	// revisions, scope precedence and expanded pack entries, determines identity.
	// Pack labels are display metadata; they must not change effective identity.

	encoded, err = json.Marshal(out)
	if err != nil {
		return PolicyMaterialization{}, err
	}
	if len(encoded) > 4<<20 {
		return PolicyMaterialization{}, invalidClosure("materialized sandbox policy exceeds its bounded result budget")
	}
	// Detach catalog slices as well so callers cannot mutate release-owned packs.
	if err := json.Unmarshal(encoded, &out); err != nil {
		return PolicyMaterialization{}, err
	}
	digest := sha256.Sum256(encoded)
	out.ContentHash = hex.EncodeToString(digest[:])
	return out, nil
}
